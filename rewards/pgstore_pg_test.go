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
	// In the order production runs them; 015 twice: it must be re-runnable.
	for _, f := range []string{"013_referral.sql", "014_referral_pool_margin.sql", "015_referral_rewards.sql", "015_referral_rewards.sql"} {
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
