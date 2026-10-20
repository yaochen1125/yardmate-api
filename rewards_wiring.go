package main

// rewards_wiring.go — builds the referral-rewards service (rewards/SPEC.md):
// the RevenueCat webhook, the gift-card payout endpoint, the /me "rewards"
// block (injected into the referral service) and the 10-minute maturing loop.

import (
	"log"

	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/referral"
	"github.com/yaochen1125/yardmate-api/rewards"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// buildRewardsService ALWAYS returns a service (routes registered, a
// half-configured server answers 503 instead of 404). Needs the shared
// Supabase pool (SUPABASE_DB_URL, tables from migration 015). The /me block is
// injected into referralSvc whenever the pool exists; the maturing loop rides
// the same REFERRAL_SETTLE_INTERVAL cadence as the referral settlement.
func buildRewardsService(vault *secrets.Vault, db *enrichment.DB, referralSvc *referral.Service) *rewards.Service {
	var store rewards.Store
	if db != nil {
		if p := rewards.NewPGStore(db.Pool()); p != nil {
			store = p
		}
	}
	svc := rewards.New(store, nil, rewards.Options{
		Interval: vaultDurationOr(vault, "REFERRAL_SETTLE_INTERVAL", 0),
	})
	if svc.CanRead() && referralSvc != nil {
		referralSvc.SetRewards(svc)
	}
	if m := svc.Missing(); len(m) > 0 {
		log.Printf("WARN: referral rewards partly configured (missing %v)", m)
	}
	return svc
}
