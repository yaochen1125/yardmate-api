package main

// referral_wiring.go — builds the /v1/referral/* service (referral/SPEC.md)
// from the vault + the shared Supabase pool, and the Supabase-JWT
// authenticator it borrows from the account-delete path.

import (
	"log"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/referral"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// newSupabaseAuthenticator returns the bearer-token verifier the referral
// endpoints use: the project JWKS (ES256) + iss/aud pinning, exactly as
// POST /v1/account/delete does (account_delete.go). nil when SUPABASE_URL is
// absent — the referral endpoints then answer 503 not_configured.
func newSupabaseAuthenticator(vault *secrets.Vault) referral.Authenticator {
	base := strings.TrimRight(vault.Get("SUPABASE_URL"), "/")
	if base == "" {
		return nil
	}
	jwks := newJWKSCache(base)
	return func(r *http.Request) (string, error) {
		return verifySupabaseToken(r.Context(), bearerToken(r), jwks.keyForKID,
			jwt.WithIssuer(base+"/auth/v1"),
			jwt.WithAudience("authenticated"))
	}
}

// buildReferralService wires referral.Service. ALWAYS returns a service (the
// routes are always registered so a half-configured server answers
// 503 not_configured instead of 404); each missing dependency is left nil and
// logged. Needs: the shared Supabase pool (SUPABASE_DB_URL), SUPABASE_URL (JWT
// verification), REVENUECAT_SECRET_KEY (v1 secret key) and
// REVENUECAT_ENTITLEMENT_ID (the entitlement identifier string as shown in the
// RevenueCat dashboard — verified on prod 2026-10-19 to be the display-name
// string "YardMate: AI Plant ID & Care Pro", spaces and colon included).
// REFERRAL_SETTLE_INTERVAL (Go duration) overrides the 10-minute cadence.
func buildReferralService(vault *secrets.Vault, db *enrichment.DB) *referral.Service {
	var store referral.Store
	if db != nil {
		if p := referral.NewPGStore(db.Pool()); p != nil {
			store = p
		}
	}
	var rc referral.Subscribers
	if c := referral.NewClient(vault.Get("REVENUECAT_SECRET_KEY")); c != nil {
		rc = c
	}
	// The code-pool low-stock / expiry alert goes to the operator mailbox the
	// feedback forwarder uses (mail_wiring.go). A typed nil must not reach the
	// interface, or the watch would call Alert on a nil mailer.
	var alerter referral.Alerter
	if m := operatorMailer(vault); m != nil {
		alerter = m
	} else {
		log.Printf("referral pool alert mail disabled (FEEDBACK_SMTP_FROM/PASS/EMAIL_TO not all set); WARN log only")
	}
	svc := referral.New(store, rc, vault.Get("REVENUECAT_ENTITLEMENT_ID"), newSupabaseAuthenticator(vault), referral.Options{
		Interval: vaultDurationOr(vault, "REFERRAL_SETTLE_INTERVAL", 0),
		Alerter:  alerter,
	})
	if !svc.Configured() {
		log.Printf("WARN: referral not configured (missing %v); /v1/referral/* answers 503 not_configured", svc.Missing())
	} else {
		log.Printf("referral service ready: per-device claim %d/%s, me+redeemed %d/%s",
			referral.DefaultClaimLimit, referral.DefaultClaimWindow, referral.DefaultReadLimit, referral.DefaultReadWindow)
	}
	return svc
}
