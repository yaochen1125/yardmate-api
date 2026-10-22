package rewards

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// pgRedeemer is referral.Service.RedeemForCharge reduced to its SQL: the
// charged subscription began after the claim (the RevenueCat half is
// unit-tested in referral/redeem_now_test.go) → referral_mark_redeemed (016)
// with its start and subscription key, exactly as settlement ① calls it.
type pgRedeemer struct {
	pool  *pgxpool.Pool
	start time.Time
	key   string
	calls atomic.Int32
}

func (r *pgRedeemer) RedeemForCharge(ctx context.Context, id int64, _ string, _ time.Time, _ string) (ChargeVerdict, error) {
	r.calls.Add(1)
	var out string
	if err := r.pool.QueryRow(ctx, `select referral_mark_redeemed($1::bigint, $2::timestamptz, $3::text)`,
		id, r.start, r.key).Scan(&out); err != nil {
		return 0, err
	}
	if out == "void" {
		return ChargeVoided, nil
	}
	return ChargeRedeemed, nil // 'redeemed', or 'skipped' (no longer claimed)
}

func pgWebhookSvc(store *PGStore, r Redeemer) *Service {
	svc := New(store, nil, Options{WebhookSecret: secret})
	svc.SetRedeemer(r)
	return svc
}

// yearlyRenewal is the upgrade's first charge for friend, a minute from now
// (after the seeded claim).
func yearlyRenewal(friend, id, txn string) map[string]any {
	return with(purchase(id), "type", "RENEWAL", "product_id", prodYearly, "app_user_id", strings.ToUpper(friend),
		"transaction_id", txn, "purchased_at_ms", time.Now().Add(time.Minute).UnixMilli())
}

func pgCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

// The free month's upgrade beats settlement: the RENEWAL redeems the referral
// in SQL (state, redeemed_at, subscription_key) and is credited once.
func TestPGRenewalBeforeSettlement(t *testing.T) {
	store, pool, ctx := pgTest(t)
	refID := seedReferral(t, ctx, pool, referrer, referee, "claimed")
	start := time.Now().Add(30 * time.Second).UTC().Truncate(time.Microsecond)
	red := &pgRedeemer{pool: pool, start: start, key: "app_store:" + start.Format(time.RFC3339Nano)}
	svc := pgWebhookSvc(store, red)

	ev := yearlyRenewal(referee, "evt-pg-up", "2000000666666666")
	if w := post(t, svc, secret, ev); w.Code != http.StatusOK || status(t, w) != outRecorded {
		t.Fatalf("renewal: %d %s", w.Code, w.Body.String())
	}
	var state, key string
	var redeemedAt time.Time
	if err := pool.QueryRow(ctx, `select state, subscription_key, redeemed_at from referrals where id = $1`, refID).
		Scan(&state, &key, &redeemedAt); err != nil {
		t.Fatal(err)
	}
	if state != "redeemed" || key != red.key || !redeemedAt.Equal(start) {
		t.Fatalf("referral = %s %q %v, want redeemed %q %v", state, key, redeemedAt, red.key, start)
	}
	if n := pgCount(t, ctx, pool, `select count(*) from reward_ledger where credit_cents = 1020 and state = 'pending'`); n != 1 {
		t.Fatalf("ledger rows = %d, want 1", n)
	}
	// Redelivered: duplicate, not asked again.
	if w := post(t, svc, secret, ev); status(t, w) != outDuplicate || red.calls.Load() != 1 {
		t.Fatalf("redelivery: %s calls=%d", w.Body.String(), red.calls.Load())
	}
	if n := pgCount(t, ctx, pool, `select count(*) from reward_ledger`); n != 1 {
		t.Fatalf("ledger rows after redelivery = %d", n)
	}
}

// The webhook and settlement ① redeem the same referral at the same moment:
// one wins under the row lock, the other is skipped, nobody is voided as a
// duplicate of itself, and the charge is credited exactly once.
func TestPGRenewalRacesSettlement(t *testing.T) {
	store, pool, ctx := pgTest(t)
	for i := 0; i < 8; i++ {
		friend := fmt.Sprintf("%04x0000-7777-8888-9999-aaaaaaaaaaaa", 0x1000+i)
		refID := seedReferral(t, ctx, pool, referrer, friend, "claimed")
		start := time.Now().Add(30 * time.Second).UTC().Truncate(time.Microsecond)
		key := fmt.Sprintf("app_store:race-%d", i)
		svc := pgWebhookSvc(store, &pgRedeemer{pool: pool, start: start, key: key})

		var wg sync.WaitGroup
		gate := make(chan struct{})
		var code int
		wg.Add(2)
		go func() { // settlement ①
			defer wg.Done()
			<-gate
			var out string
			if err := pool.QueryRow(ctx, `select referral_mark_redeemed($1::bigint, $2::timestamptz, $3::text)`,
				refID, start, key).Scan(&out); err != nil {
				t.Errorf("settlement: %v", err)
			}
		}()
		go func() { // the webhook
			defer wg.Done()
			<-gate
			code = post(t, svc, secret, yearlyRenewal(friend, fmt.Sprintf("evt-race-%d", i), fmt.Sprintf("20000007%08d", i))).Code
		}()
		close(gate)
		wg.Wait()

		var state string
		if err := pool.QueryRow(ctx, `select state from referrals where id = $1`, refID).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if code != http.StatusOK || state != "redeemed" {
			t.Fatalf("run %d: webhook %d, referral %s; want 200 and redeemed", i, code, state)
		}
		if n := pgCount(t, ctx, pool, `select count(*) from reward_ledger where referral_id = $1`, refID); n != 1 {
			t.Fatalf("run %d: ledger rows %d, want 1", i, n)
		}
	}
}

// Gate 3 on the spot: the subscription is already another live referral's →
// voided as duplicate_apple_id in SQL, nothing credited.
func TestPGRenewalBeforeSettlementDuplicateAppleID(t *testing.T) {
	store, pool, ctx := pgTest(t)
	key := "app_store:2026-10-22T12:00:00Z"
	other := seedReferral(t, ctx, pool, referrer, "99999999-7777-8888-9999-aaaaaaaaaaaa", "redeemed")
	if _, err := pool.Exec(ctx, `update referrals set subscription_key = $2 where id = $1`, other, key); err != nil {
		t.Fatal(err)
	}
	refID := seedReferral(t, ctx, pool, referrer, referee, "claimed")
	svc := pgWebhookSvc(store, &pgRedeemer{pool: pool, start: time.Now().Add(30 * time.Second), key: key})

	if w := post(t, svc, secret, yearlyRenewal(referee, "evt-pg-dup", "2000000888888888")); w.Code != http.StatusOK || status(t, w) != outNotRedeemed {
		t.Fatalf("renewal: %d %s", w.Code, w.Body.String())
	}
	var state, reason string
	if err := pool.QueryRow(ctx, `select state, coalesce(void_reason, '') from referrals where id = $1`, refID).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "void" || reason != "duplicate_apple_id" {
		t.Fatalf("referral = %s %q, want void duplicate_apple_id", state, reason)
	}
	if n := pgCount(t, ctx, pool, `select count(*) from reward_ledger`); n != 0 {
		t.Fatalf("ledger rows = %d, want 0", n)
	}
}
