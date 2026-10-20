package main

// rewards_wiring.go — builds the referral-rewards service (rewards/SPEC.md):
// the RevenueCat webhook, the gift-card payout endpoint, the /me "rewards"
// block (injected into the referral service) and the 10-minute maturing loop.

import (
	"log"

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
// REVENUECAT_WEBHOOK_SECRET (for the webhook). The /me block is
// injected into referralSvc whenever the pool exists; the maturing loop rides
// the same REFERRAL_SETTLE_INTERVAL cadence as the referral settlement.
func buildRewardsService(vault *secrets.Vault, db *enrichment.DB, referralSvc *referral.Service) *rewards.Service {
	var store rewards.Store
	if db != nil {
		if p := rewards.NewPGStore(db.Pool()); p != nil {
			store = p
		}
	}
	// Payout requests go to the operator inbox over the /v1/feedback account
	// (FEEDBACK_SMTP_* / FEEDBACK_EMAIL_TO). A typed nil must not reach the
	// interface, or notify would call Alert on a nil mailer.
	var mailer rewards.Mailer
	if m := enrichment.NewFeedbackMailer(
		vault.Get("FEEDBACK_SMTP_HOST"), vault.Get("FEEDBACK_SMTP_PORT"),
		vault.Get("FEEDBACK_SMTP_FROM"), vault.Get("FEEDBACK_SMTP_PASS"),
		vault.Get("FEEDBACK_EMAIL_TO")); m != nil {
		mailer = m
	} else {
		log.Printf("WARN: referral payout mail disabled (FEEDBACK_SMTP_FROM/PASS/EMAIL_TO not all set); requests stored, see deploy/referral-payout.sh list")
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
		Mailer:        mailer,
		DeviceBucket:  bucket,
		Interval:      vaultDurationOr(vault, "REFERRAL_SETTLE_INTERVAL", 0),
	})
	if svc.CanRead() && referralSvc != nil {
		referralSvc.SetRewards(svc)
	}
	if m := svc.Missing(); len(m) > 0 {
		log.Printf("WARN: referral rewards partly configured (missing %v)", m)
	}
	return svc
}
