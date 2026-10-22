package rewards

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The SQL side (migrations 013 → 014 → 015, the production order) against a
// real Postgres: event-id and transaction-id dedupe, void only pending, the
// refund-before-charge tombstone, maturing on now(), the balance sums, the
// payout transaction's three outcomes, and the account-deletion cascade.
// Needs a scratch database:
//
//	REWARDS_PG_TEST_URL=postgres://postgres:postgres@localhost:5432/scratch go test ./rewards/ -run PG -v
//
// Skipped when the variable is unset. Everything lives in a throwaway schema
// that is dropped afterwards — never point it at staging or prod. The roles
// 013 grants to (anon / authenticated / service_role) are created when
// missing, which needs CREATEROLE.
func pgTest(t *testing.T) (*PGStore, *pgxpool.Pool, context.Context) {
	t.Helper()
	url := os.Getenv("REWARDS_PG_TEST_URL")
	if url == "" {
		t.Skip("REWARDS_PG_TEST_URL not set")
	}
	ctx := context.Background()
	schema := fmt.Sprintf("rewards_test_%d", time.Now().UnixNano())

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := admin.Exec(ctx, `do $$ begin
		if not exists (select 1 from pg_roles where rolname = 'anon')          then create role anon nologin;          end if;
		if not exists (select 1 from pg_roles where rolname = 'authenticated') then create role authenticated nologin; end if;
		if not exists (select 1 from pg_roles where rolname = 'service_role')  then create role service_role nologin;  end if;
	end $$`); err != nil {
		t.Fatalf("roles: %v", err)
	}
	if _, err := admin.Exec(ctx, "create schema "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	admin.Close(ctx)

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse url: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema + ",public"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if c, err := pgx.Connect(ctx, url); err == nil {
			_, _ = c.Exec(ctx, "drop schema "+schema+" cascade")
			c.Close(ctx)
		}
	})
	// In the order production runs them; 015 and 016 twice: they must be re-runnable.
	for _, f := range []string{"013_referral.sql", "014_referral_pool_margin.sql", "015_referral_rewards.sql", "015_referral_rewards.sql",
		"016_referral_antiabuse.sql", "016_referral_antiabuse.sql", "018_reward_payout_undo.sql", "018_reward_payout_undo.sql", "019_referral_w9_threshold.sql", "019_referral_w9_threshold.sql"} {
		sql, err := os.ReadFile(filepath.Join("..", "proxy", "enrichment", "migrations", f))
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		// No arguments → simple protocol → the whole file runs as one script.
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("apply %s: %v", f, err)
		}
	}
	return NewPGStore(pool), pool, ctx
}

// seedReferral makes A and B users and an A→B referral in state.
func seedReferral(t *testing.T, ctx context.Context, pool *pgxpool.Pool, a, b, state string) int64 {
	t.Helper()
	for _, u := range []string{a, b} {
		if _, err := pool.Exec(ctx, `insert into referral_users (user_id, code) values ($1, $2) on conflict do nothing`,
			u, "YM-"+u[:4]); err != nil {
			t.Fatalf("user: %v", err)
		}
	}
	var id int64
	if err := pool.QueryRow(ctx, `insert into referrals (referrer, referee, state) values ($1, $2, $3) returning id`,
		a, b, state).Scan(&id); err != nil {
		t.Fatalf("referral: %v", err)
	}
	return id
}

func TestPGLedgerAndPayouts(t *testing.T) {
	store, pool, ctx := pgTest(t)
	refID := seedReferral(t, ctx, pool, referrer, referee, "redeemed")

	ref, ok, err := store.ReferralByReferee(ctx, referee)
	if err != nil || !ok || ref.ID != refID || ref.Referrer != referrer || ref.State != "redeemed" {
		t.Fatalf("ReferralByReferee = %+v %v %v", ref, ok, err)
	}
	if _, ok, err := store.ReferralByReferee(ctx, stranger); ok || err != nil {
		t.Fatalf("stranger found: %v %v", ok, err)
	}

	past := time.Now().Add(-RefundWindow - time.Hour)
	credit := func(event, txn string, cents int64, purchased time.Time) Credit {
		return Credit{ReferralID: refID, Referrer: referrer, Referee: referee, EventID: event, EventType: "RENEWAL",
			TransactionID: txn, ProductID: "com.chenyao.plantapp.premium.year", PeriodType: "NORMAL",
			PriceUSDCents: 3999, Takehome: "0.8500", CreditCents: cents,
			PurchasedAt: purchased, AvailableAt: purchased.Add(RefundWindow)}
	}
	// 12 000 + 12 000 matured (past window), 1 020 still pending.
	for i, c := range []Credit{credit("e1", "t1", 6000, past), credit("e2", "t2", 6000, past),
		credit("e3", "t3", 6000, past), credit("e4", "t4", 6000, past), credit("e5", "t5", 1020, time.Now())} {
		res, err := store.RecordCredit(ctx, c)
		if err != nil || res != CreditInserted {
			t.Fatalf("record %d: %v %v", i, res, err)
		}
	}
	if res, err := store.RecordCredit(ctx, credit("e1", "t1", 6000, past)); err != nil || res != CreditDuplicate {
		t.Fatalf("duplicate event: %v %v", res, err)
	}
	// Same transaction under a new event id: the transaction_id unique index.
	if res, err := store.RecordCredit(ctx, credit("e1-again", "t1", 6000, past)); err != nil || res != CreditDuplicate {
		t.Fatalf("duplicate transaction: %v %v", res, err)
	}

	// Refund of t4 while still pending → voided; of t5 later, too.
	if n, err := store.VoidPending(ctx, "t4"); err != nil || n != 1 {
		t.Fatalf("void t4: %d %v", n, err)
	}
	if n, err := store.MatureDue(ctx); err != nil || n != 3 {
		t.Fatalf("mature: %d %v", n, err)
	}
	// Already available: a late refund does not take it back.
	if n, err := store.VoidPending(ctx, "t1"); err != nil || n != 0 {
		t.Fatalf("void available t1: %d %v", n, err)
	}

	b, err := store.Balance(ctx, referrer)
	if err != nil {
		t.Fatalf("balance: %v", err)
	}
	if b.AvailableLedgerCents != 18000 || b.PendingCents != 1020 || b.SpentCents != 0 || b.Latest != nil {
		t.Fatalf("balance = %+v", b)
	}

	yearStart := time.Date(time.Now().UTC().Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	po, year, err := store.RequestPayout(ctx, referrer, "a@example.com", PayoutAmountCents, ThresholdCents, yearStart)
	if err != nil || po.State != PayoutRequested || po.AmountCents != 10000 || po.Email != "a@example.com" {
		t.Fatalf("payout = %+v %v", po, err)
	}
	if year.RequestedCents != 10000 || year.SentCents != 0 {
		t.Fatalf("year = %+v", year)
	}
	if _, _, err := store.RequestPayout(ctx, referrer, "a@example.com", PayoutAmountCents, ThresholdCents, yearStart); !errors.Is(err, ErrPayoutInProgress) {
		t.Fatalf("second request: %v", err)
	}
	if _, _, err := store.RequestPayout(ctx, stranger, "s@example.com", PayoutAmountCents, ThresholdCents, yearStart); !errors.Is(err, ErrBelowThreshold) {
		t.Fatalf("stranger: %v", err)
	}
	// Mark it sent → balance 8 000 < 10 000 → below_threshold.
	if _, err := pool.Exec(ctx, `update reward_payouts set state = 'sent', sent_at = now() where id = $1`, po.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RequestPayout(ctx, referrer, "a@example.com", PayoutAmountCents, ThresholdCents, yearStart); !errors.Is(err, ErrBelowThreshold) {
		t.Fatalf("after sent: %v", err)
	}
	b, _ = store.Balance(ctx, referrer)
	if b.SpentCents != 10000 || b.Latest == nil || b.Latest.State != PayoutSent || b.Latest.SentAt.IsZero() {
		t.Fatalf("balance after sent = %+v latest=%+v", b, b.Latest)
	}

	// The friend deletes their account: the referral goes, our money stays.
	if _, err := pool.Exec(ctx, `delete from referral_users where user_id = $1`, referee); err != nil {
		t.Fatalf("delete referee: %v", err)
	}
	if b2, _ := store.Balance(ctx, referrer); b2.AvailableLedgerCents != 18000 {
		t.Fatalf("referee deletion changed referrer balance: %+v", b2)
	}
	// The referrer deletes theirs: ledger + payouts go with it.
	if _, err := pool.Exec(ctx, `delete from referral_users where user_id = $1`, referrer); err != nil {
		t.Fatalf("delete referrer: %v", err)
	}
	var left int
	if err := pool.QueryRow(ctx, `select (select count(*) from reward_ledger) + (select count(*) from reward_payouts)`).Scan(&left); err != nil || left != 0 {
		t.Fatalf("rows left after referrer deletion: %d %v", left, err)
	}
}

// The W-9 threshold row (migration 019): $2,000 by default, editable.
func TestPGW9Threshold(t *testing.T) {
	store, pool, ctx := pgTest(t)
	if got, err := store.W9ThresholdCents(ctx); err != nil || got != DefaultW9ThresholdCents {
		t.Fatalf("default = %d %v", got, err)
	}
	if _, err := pool.Exec(ctx, `update referral_settings set w9_threshold_cents = 210000 where id = 1`); err != nil {
		t.Fatal(err)
	}
	if got, err := store.W9ThresholdCents(ctx); err != nil || got != 210000 {
		t.Fatalf("after update = %d %v", got, err)
	}
}

// mark-sent's hour of grace (migration 018, rewards.md ### 标记已发): the two
// SQL functions the operator script calls, and how a payout inside the hour
// reads everywhere else — still requested, no sent_at, /payout refused.
func TestPGMarkSentBufferAndUndo(t *testing.T) {
	store, pool, ctx := pgTest(t)
	refID := seedReferral(t, ctx, pool, referrer, referee, "redeemed")
	past := time.Now().Add(-RefundWindow - time.Hour)
	for i, txn := range []string{"b1", "b2"} {
		c := Credit{ReferralID: refID, Referrer: referrer, Referee: referee, EventID: "ev-" + txn, EventType: "RENEWAL",
			TransactionID: txn, ProductID: "com.chenyao.plantapp.premium.year", PeriodType: "NORMAL",
			PriceUSDCents: 3999, Takehome: "0.8500", CreditCents: 15000,
			PurchasedAt: past, AvailableAt: past.Add(RefundWindow)}
		if res, err := store.RecordCredit(ctx, c); err != nil || res != CreditInserted {
			t.Fatalf("record %d: %v %v", i, res, err)
		}
	}
	if _, err := store.MatureDue(ctx); err != nil {
		t.Fatal(err)
	}
	yearStart := time.Date(time.Now().UTC().Year(), 1, 1, 0, 0, 0, 0, time.UTC)
	po, _, err := store.RequestPayout(ctx, referrer, "a@example.com", PayoutAmountCents, ThresholdCents, yearStart)
	if err != nil {
		t.Fatalf("request: %v", err)
	}

	markSent := func(email string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from reward_payout_mark_sent($1, $2)`, po.ID, email).Scan(&n); err != nil {
			t.Fatalf("mark_sent: %v", err)
		}
		return n
	}
	undo := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `select count(*) from reward_payout_undo($1)`, po.ID).Scan(&n); err != nil {
			t.Fatalf("undo: %v", err)
		}
		return n
	}
	raw := func() (state string, sent, until *time.Time) {
		t.Helper()
		if err := pool.QueryRow(ctx, `select state, sent_at, undo_until from reward_payouts where id = $1`, po.ID).
			Scan(&state, &sent, &until); err != nil {
			t.Fatal(err)
		}
		return
	}

	// The email changed between the operator's look and the commit: nothing.
	if n := markSent("other@example.com"); n != 0 {
		t.Fatalf("mark_sent with a stale email changed %d rows", n)
	}
	if n := undo(); n != 0 {
		t.Fatalf("undo of a requested payout changed %d rows", n)
	}
	if n := markSent("a@example.com"); n != 1 {
		t.Fatalf("mark_sent changed %d rows", n)
	}
	state, sent, until := raw()
	if state != PayoutSent || sent == nil || until == nil || until.Sub(*sent) != time.Hour {
		t.Fatalf("after mark_sent: %s sent=%v until=%v", state, sent, until)
	}
	if n := markSent("a@example.com"); n != 0 {
		t.Fatalf("second mark_sent changed %d rows", n)
	}

	// Inside the hour: requested to everyone else.
	b, err := store.Balance(ctx, referrer)
	if err != nil || b.Latest == nil || b.Latest.State != PayoutRequested || !b.Latest.SentAt.IsZero() {
		t.Fatalf("balance in buffer = %+v latest=%+v err=%v", b, b.Latest, err)
	}
	if b.SpentCents != 10000 {
		t.Fatalf("spent in buffer = %d, want 10000", b.SpentCents)
	}
	if _, _, err := store.RequestPayout(ctx, referrer, "a@example.com", PayoutAmountCents, ThresholdCents, yearStart); !errors.Is(err, ErrPayoutInProgress) {
		t.Fatalf("request in buffer: %v", err)
	}

	// Undo inside the hour: back to requested, as if never marked.
	if n := undo(); n != 1 {
		t.Fatalf("undo changed %d rows", n)
	}
	if state, sent, until := raw(); state != PayoutRequested || sent != nil || until != nil {
		t.Fatalf("after undo: %s sent=%v until=%v", state, sent, until)
	}

	// Mark again and let the hour pass: sent for real, undo refused.
	if n := markSent("a@example.com"); n != 1 {
		t.Fatalf("re-mark changed %d rows", n)
	}
	if _, err := pool.Exec(ctx, `update reward_payouts set undo_until = now() - interval '1 second' where id = $1`, po.ID); err != nil {
		t.Fatal(err)
	}
	b, _ = store.Balance(ctx, referrer)
	if b.Latest == nil || b.Latest.State != PayoutSent || b.Latest.SentAt.IsZero() {
		t.Fatalf("after the hour latest = %+v", b.Latest)
	}
	if n := undo(); n != 0 {
		t.Fatalf("undo after the hour changed %d rows", n)
	}
	// Balance 30 000 − 10 000 sent = 20 000: a new request is allowed again.
	if _, _, err := store.RequestPayout(ctx, referrer, "a@example.com", PayoutAmountCents, ThresholdCents, yearStart); err != nil {
		t.Fatalf("request after the hour: %v", err)
	}
}

func TestPGRefundBeforeCharge(t *testing.T) {
	store, pool, ctx := pgTest(t)
	refID := seedReferral(t, ctx, pool, referrer, referee, "redeemed")
	now := time.Now()
	c := Credit{ReferralID: refID, Referrer: referrer, Referee: referee, EventID: "late", EventType: "INITIAL_PURCHASE",
		TransactionID: "t-refunded", PeriodType: "NORMAL", PriceUSDCents: 3999, Takehome: "0.8500", CreditCents: 1020,
		PurchasedAt: now, AvailableAt: now.Add(RefundWindow)}

	// The refund comes first: nothing pending → tombstone.
	if n, err := store.VoidPending(ctx, "t-refunded"); err != nil || n != 0 {
		t.Fatalf("void before charge: %d %v", n, err)
	}
	// Twice is fine (redelivered refund).
	if _, err := store.VoidPending(ctx, "t-refunded"); err != nil {
		t.Fatalf("second void: %v", err)
	}
	// The late charge, and its redelivery, are not written.
	for i := 0; i < 2; i++ {
		if res, err := store.RecordCredit(ctx, c); err != nil || res != CreditVoided {
			t.Fatalf("late charge %d: %v %v", i+1, res, err)
		}
	}
	var rows, tombstones int
	if err := pool.QueryRow(ctx, `select (select count(*) from reward_ledger), (select count(*) from reward_voided_txns)`).
		Scan(&rows, &tombstones); err != nil || rows != 0 || tombstones != 1 {
		t.Fatalf("ledger rows %d, tombstones %d, err %v", rows, tombstones, err)
	}

	// A normal order (charge, then refund) voids the row and leaves no tombstone.
	c2 := c
	c2.EventID, c2.TransactionID = "normal", "t-normal"
	if res, err := store.RecordCredit(ctx, c2); err != nil || res != CreditInserted {
		t.Fatalf("charge: %v %v", res, err)
	}
	if n, err := store.VoidPending(ctx, "t-normal"); err != nil || n != 1 {
		t.Fatalf("void: %d %v", n, err)
	}
	// 016: a refund says why (the payout review counts refunds by it).
	var reason string
	if err := pool.QueryRow(ctx, `select void_reason from reward_ledger where transaction_id = 't-normal'`).Scan(&reason); err != nil || reason != VoidRefunded {
		t.Fatalf("void_reason = %q %v, want refunded", reason, err)
	}
	// Its redelivery afterwards is a duplicate (the row is there, voided), not a new credit.
	if res, err := store.RecordCredit(ctx, c2); err != nil || res != CreditDuplicate {
		t.Fatalf("redelivered charge after void: %v %v", res, err)
	}
	if err := pool.QueryRow(ctx, `select count(*) from reward_voided_txns`).Scan(&tombstones); err != nil || tombstones != 1 {
		t.Fatalf("tombstones %d %v", tombstones, err)
	}
	// anon / authenticated see nothing (RLS on, no policy).
	var rls bool
	if err := pool.QueryRow(ctx, `select relrowsecurity from pg_class where oid = 'reward_voided_txns'::regclass`).Scan(&rls); err != nil || !rls {
		t.Fatalf("reward_voided_txns RLS: %v %v", rls, err)
	}
}

// RecordCredit re-reads the referral under a share lock (final review P2): a
// referral voided after the webhook's lookup, or deleted with an account,
// gets no new pending credit.
func TestPGRecordCreditRechecksTheReferral(t *testing.T) {
	store, pool, ctx := pgTest(t)
	refID := seedReferral(t, ctx, pool, referrer, referee, "redeemed")
	now := time.Now()
	c := Credit{ReferralID: refID, Referrer: referrer, Referee: referee, EventID: "raced", EventType: "INITIAL_PURCHASE",
		TransactionID: "t-raced", PeriodType: "NORMAL", PriceUSDCents: 3999, Takehome: "0.8500", CreditCents: 1020,
		PurchasedAt: now, AvailableAt: now.Add(RefundWindow)}
	if _, err := pool.Exec(ctx, `update referrals set state = 'void' where id = $1`, refID); err != nil {
		t.Fatal(err)
	}
	if res, err := store.RecordCredit(ctx, c); err != nil || res != CreditReferralVoid {
		t.Fatalf("void referral: %v %v, want CreditReferralVoid", res, err)
	}
	c.ReferralID, c.EventID, c.TransactionID = refID+1000, "gone", "t-gone"
	if res, err := store.RecordCredit(ctx, c); err != nil || res != CreditReferralVoid {
		t.Fatalf("missing referral: %v %v, want CreditReferralVoid", res, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `select count(*) from reward_ledger`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("ledger rows = %d (%v), want 0", n, err)
	}
}

func TestPGOneRequestedPerUserIndex(t *testing.T) {
	_, pool, ctx := pgTest(t)
	seedReferral(t, ctx, pool, referrer, referee, "redeemed")
	if _, err := pool.Exec(ctx, `insert into reward_payouts (user_id, email) values ($1, 'a@example.com')`, referrer); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `insert into reward_payouts (user_id, email) values ($1, 'a@example.com')`, referrer); err == nil {
		t.Fatal("second requested payout accepted by the partial unique index")
	}
}

func TestPGReviewStats(t *testing.T) {
	store, pool, ctx := pgTest(t)
	now := time.Now()
	friends := []struct {
		user, state, reason, device string
	}{
		{"f1111111-0000-4000-8000-000000000000", "rewarded", "", "dev-1"},
		{"f2222222-0000-4000-8000-000000000000", "held", "", "dev-1"},
		{"f3333333-0000-4000-8000-000000000000", "qualified", "", ""},
		{"f4444444-0000-4000-8000-000000000000", "void", "duplicate_apple_id", "dev-4"},
		{"f5555555-0000-4000-8000-000000000000", "void", "rejected", "dev-5"},
	}
	ids := map[string]int64{}
	for _, f := range friends {
		id := seedReferral(t, ctx, pool, referrer, f.user, f.state)
		ids[f.user] = id
		if _, err := pool.Exec(ctx, `update referrals set void_reason = nullif($2, ''), claim_install_id = nullif($3, '') where id = $1`,
			id, f.reason, f.device); err != nil {
			t.Fatal(err)
		}
	}
	credit := func(user, txn, state, reason string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`insert into reward_ledger (referral_id, referrer, referee, rc_event_id, event_type, transaction_id, period_type,
			                            price_usd_cents, takehome_percentage, credit_cents, purchased_at, available_at, state, void_reason)
			 values ($1, $2, $3, 'ev-' || $4, 'RENEWAL', $4, 'NORMAL', 699, 0.85, 178, $5, $6, $7, nullif($8, ''))`,
			ids[user], referrer, user, txn, now, now.Add(RefundWindow), state, reason); err != nil {
			t.Fatal(err)
		}
	}
	credit(friends[0].user, "t1", "available", "")
	credit(friends[0].user, "t2", "voided", "refunded")
	credit(friends[1].user, "t3", "voided", "refunded")
	credit(friends[3].user, "t4", "voided", "duplicate_apple_id")
	rv, err := store.ReviewStats(ctx, referrer)
	if err != nil {
		t.Fatal(err)
	}
	if rv.Friends != 3 || rv.FriendsPaid != 1 || rv.ClaimDevices != 1 || rv.NoDevice != 1 || rv.Held != 1 || rv.Refunds != 2 {
		t.Errorf("review = %+v", rv)
	}
	if rv.VoidByReason["duplicate_apple_id"] != 1 || rv.VoidByReason["rejected"] != 1 || len(rv.VoidByReason) != 2 {
		t.Errorf("void by reason = %v", rv.VoidByReason)
	}
	if empty, err := store.ReviewStats(ctx, stranger); err != nil || empty.Friends != 0 || len(empty.VoidByReason) != 0 {
		t.Errorf("stranger: %+v %v", empty, err)
	}
}

func TestPGProgress(t *testing.T) {
	store, pool, ctx := pgTest(t)
	now := time.Now()
	paid := seedReferral(t, ctx, pool, referrer, "f1111111-0000-4000-8000-000000000000", "rewarded")
	free := seedReferral(t, ctx, pool, referrer, "f2222222-0000-4000-8000-000000000000", "redeemed")
	claimed := seedReferral(t, ctx, pool, referrer, "f3333333-0000-4000-8000-000000000000", "claimed")
	void := seedReferral(t, ctx, pool, referrer, "f4444444-0000-4000-8000-000000000000", "void")
	gone := seedReferral(t, ctx, pool, referrer, "f5555555-0000-4000-8000-000000000000", "rewarded")
	if _, err := pool.Exec(ctx, `update referrals set redeemed_at = $2 where id = $1`, free, now.Add(-day(3))); err != nil {
		t.Fatal(err)
	}
	// Claimed 300 days ago, code reissued 20 days ago: still inside the unredeemed window.
	if _, err := pool.Exec(ctx, `update referrals set claimed_at = $2, reissued_at = $3 where id = $1`,
		claimed, now.Add(-day(300)), now.Add(-day(20))); err != nil {
		t.Fatal(err)
	}
	credit := func(refID int64, txn, state, product string, purchased time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`insert into reward_ledger (referral_id, referrer, rc_event_id, event_type, transaction_id, product_id, period_type,
			                            price_usd_cents, takehome_percentage, credit_cents, purchased_at, available_at, state)
			 values ($1, $2, 'ev-' || $3, 'RENEWAL', $3, $4, 'NORMAL', 699, 0.85, 178, $5, $6, $7)`,
			refID, referrer, txn, product, purchased, purchased.Add(RefundWindow), state); err != nil {
			t.Fatal(err)
		}
	}
	credit(paid, "t1", "pending", "com.chenyao.plantapp.premium.month", now.Add(-day(10)))
	credit(paid, "t2", "available", "com.chenyao.plantapp.premium.month", now.Add(-day(100)))
	credit(paid, "t2b", "available", "com.chenyao.plantapp.premium.month", now.Add(-day(130))) // only the latest available comes back
	credit(paid, "t3", "voided", "com.chenyao.plantapp.premium.month", now.Add(-day(5)))
	credit(void, "t4", "pending", "com.chenyao.plantapp.premium.month", now.Add(-day(5)))
	credit(gone, "t5", "pending", "com.chenyao.plantapp.premium.yearly.3d", now.Add(-day(20)))
	// The friend behind `gone` deletes their account: the referral row goes,
	// the ledger row stays with referral_id null.
	if _, err := pool.Exec(ctx, `delete from referral_users where user_id = 'f5555555-0000-4000-8000-000000000000'`); err != nil {
		t.Fatal(err)
	}

	refs, credits, err := store.Progress(ctx, referrer)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 {
		t.Fatalf("refs = %+v (void and deleted must be gone)", refs)
	}
	for _, r := range refs {
		if r.ID == free && r.RedeemedAt.Sub(now.Add(-day(3))).Abs() > time.Millisecond {
			t.Errorf("redeemed_at = %s", r.RedeemedAt)
		}
		if r.ID != free && !r.RedeemedAt.IsZero() {
			t.Errorf("ref %d redeemed_at = %s, want zero", r.ID, r.RedeemedAt)
		}
		if r.ID == claimed && r.IssuedAt.Sub(now.Add(-day(20))).Abs() > time.Millisecond {
			t.Errorf("issued_at = %s, want reissued_at", r.IssuedAt)
		}
		if r.ID == paid && !r.IssuedAt.Equal(r.ClaimedAt) {
			t.Errorf("issued_at %s != claimed_at %s without a reissue", r.IssuedAt, r.ClaimedAt)
		}
	}
	if len(credits) != 4 { // t1, t2, t4 (void referral, Progress skips it), t5 (deleted friend)
		t.Fatalf("credits = %+v", credits)
	}
	for _, c := range credits {
		if c.State == LedgerAvailable && c.PurchasedAt.Sub(now.Add(-day(100))).Abs() > time.Millisecond {
			t.Errorf("available row purchased %s: want the latest one (t2, 100 days ago), not t2b", c.PurchasedAt)
		}
	}
	counts, list := Progress(refs, credits, now)
	if counts != (Counts{Joined: 3, FreeMonth: 1, Paying: 1}) {
		t.Errorf("counts = %+v", counts)
	}
	if len(list) != 4 || list[0].JoinedAt != nil || list[0].Plan != PlanYearly || list[1].Plan != PlanMonthly ||
		list[2].Phase != PhaseFreeMonth || list[3].Phase != PhaseUnredeemed {
		t.Errorf("list = %+v", list)
	}
	if refs, credits, err := store.Progress(ctx, stranger); err != nil || len(refs) != 0 || len(credits) != 0 {
		t.Errorf("stranger: %v %v %v", refs, credits, err)
	}
}
