package main

// rewards_wiring.go — builds the referral-rewards service (rewards/SPEC.md):
// the RevenueCat webhook, the gift-card payout endpoint, the /me "rewards"
// block (injected into the referral service) and the 10-minute maturing loop.

import (
	"context"
	"log"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/ratelimit"
	"github.com/yaochen1125/yardmate-api/referral"
	"github.com/yaochen1125/yardmate-api/rewards"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// buildRewardsService ALWAYS returns a service (routes registered, a
// half-configured server answers 503 instead of 404). Needs the shared
// Supabase pool (SUPABASE_DB_URL, tables from migration 015), SUPABASE_URL
// (the same JWT verifier as /v1/referral/*, for /payout) and
// REVENUECAT_WEBHOOK_SECRET (for the webhook). Payout requests are mailed to
// the operator mailbox main built once (mail_wiring.go; nil = stored and
// logged only, main logs that once). The /me block is injected into
// referralSvc whenever the pool exists; the maturing loop rides the same
// REFERRAL_SETTLE_INTERVAL cadence as the referral settlement.
func buildRewardsService(vault *secrets.Vault, db *enrichment.DB, referralSvc *referral.Service, mailer *enrichment.FeedbackMailer) *rewards.Service {
	var store rewards.Store
	if db != nil {
		if p := rewards.NewPGStore(db.Pool()); p != nil {
			store = p
		}
	}
	// A typed nil must not reach the interface, or notify would call Alert on
	// a nil mailer.
	var m rewards.Mailer
	if mailer != nil {
		m = mailer
	}
	// /payout spends from /claim's per-device bucket (docs: 复用 /claim 的设备桶).
	var bucket *ratelimit.Bucket
	if referralSvc != nil {
		bucket = referralSvc.ClaimBucket()
	}
	svc := rewards.New(store, rewards.Authenticator(newSupabaseAuthenticator(vault)), rewards.Options{
		// What RevenueCat puts in the Authorization header (dashboard →
		// Integrations → Webhooks), with or without "Bearer ". Unset → 503.
		WebhookSecret: vault.Get("REVENUECAT_WEBHOOK_SECRET"),
		Mailer:        m,
		DeviceBucket:  bucket,
		AllowSandbox:  webhookAllowSandbox(vault),
		Interval:      vaultDurationOr(vault, "REFERRAL_SETTLE_INTERVAL", 0),
	})
	if svc.CanRead() && referralSvc != nil {
		referralSvc.SetRewards(svc)
	}
	// A friend's RENEWAL that beats settlement ① (an upgrade out of the free
	// month) has the referral redeemed on the spot (rewards.md ### 记账来源).
	if referralSvc != nil {
		svc.SetRedeemer(chargeRedeemer{referralSvc})
	}
	if missing := svc.Missing(); len(missing) > 0 {
		log.Printf("WARN: referral rewards partly configured (missing %v)", missing)
	}
	return svc
}

// webhookAllowSandbox reads REVENUECAT_WEBHOOK_ALLOW_SANDBOX: staging books
// sandbox purchases so the referral flow can be tested end to end there.
func webhookAllowSandbox(vault *secrets.Vault) bool {
	on := vault.GetBool("REVENUECAT_WEBHOOK_ALLOW_SANDBOX", false)
	if on {
		log.Printf("WARN: REVENUECAT_WEBHOOK_ALLOW_SANDBOX=true: the RevenueCat webhook books SANDBOX events — staging only, never set it on prod")
	}
	return on
}

// chargeRedeemer adapts referral.Service.RedeemForCharge to rewards.Redeemer
// (neither package imports the other).
type chargeRedeemer struct{ svc *referral.Service }

func (c chargeRedeemer) RedeemForCharge(ctx context.Context, referralID int64, referee string, claimedAt time.Time, productID string) (rewards.ChargeVerdict, error) {
	v, err := c.svc.RedeemForCharge(ctx, referralID, referee, claimedAt, productID)
	if err != nil {
		return 0, err
	}
	switch v {
	case referral.ChargeRedeemed:
		return rewards.ChargeRedeemed, nil
	case referral.ChargeVoided:
		return rewards.ChargeVoided, nil
	default:
		return rewards.ChargeNotCounted, nil
	}
}
