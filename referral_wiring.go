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
// Anti-abuse gate 1 (DeviceCheck) reads DEVICECHECK_KEY_ID +
// DEVICECHECK_PRIVATE_KEY (or DEVICECHECK_PRIVATE_KEY_PATH) + APPLE_TEAM_ID
// and DEVICECHECK_MODE (enforce | log; unset = enforce) — see
// buildDeviceCheck.
func buildReferralService(vault *secrets.Vault, db *enrichment.DB, mailer *enrichment.FeedbackMailer) *referral.Service {
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
	// The code-pool low-stock / expiry alert goes to the operator mailbox main
	// built once (mail_wiring.go). A typed nil must not reach the interface,
	// or the watch would call Alert on a nil mailer.
	var alerter referral.Alerter
	if mailer != nil {
		alerter = mailer
	}
	dc, mode := buildDeviceCheck(vault)
	svc := referral.New(store, rc, vault.Get("REVENUECAT_ENTITLEMENT_ID"), newSupabaseAuthenticator(vault), referral.Options{
		Interval:        vaultDurationOr(vault, "REFERRAL_SETTLE_INTERVAL", 0),
		Alerter:         alerter,
		DeviceCheck:     dc,
		DeviceCheckMode: mode,
	})
	if !svc.Configured() {
		log.Printf("WARN: referral not configured (missing %v); /v1/referral/* answers 503 not_configured", svc.Missing())
	} else {
		log.Printf("referral service ready: per-device claim %d/%s, me+redeemed %d/%s",
			referral.DefaultClaimLimit, referral.DefaultClaimWindow, referral.DefaultReadLimit, referral.DefaultReadWindow)
	}
	return svc
}

// buildDeviceCheck wires gate 1 (referral/devicecheck.go). The .p8 is read
// exactly like APPLE_PRIVATE_KEY (appleP8Bytes: inline with "\n" escapes, or a
// _PATH). Any piece missing or unparsable → nil client: in enforce mode every
// NEW claim then answers 503 device_check_unavailable (re-claims, /me and
// /redeemed are unaffected); in log mode claims go through unchecked. Only
// the mode and yes/no are logged, never the key.
func buildDeviceCheck(vault *secrets.Vault) (referral.DeviceChecker, referral.DeviceCheckMode) {
	mode, ok := referral.ParseDeviceCheckMode(vault.Get("DEVICECHECK_MODE"))
	if !ok {
		log.Printf("WARN: DEVICECHECK_MODE is neither enforce nor log; using enforce")
	}
	keyID, teamID := vault.Get("DEVICECHECK_KEY_ID"), vault.Get("APPLE_TEAM_ID")
	pemBytes, err := appleP8Bytes(vault.Get("DEVICECHECK_PRIVATE_KEY"), vault.Get("DEVICECHECK_PRIVATE_KEY_PATH"), "DEVICECHECK_PRIVATE_KEY")
	if keyID == "" || teamID == "" || err != nil {
		var missing []string
		if keyID == "" {
			missing = append(missing, "DEVICECHECK_KEY_ID")
		}
		if teamID == "" {
			missing = append(missing, "APPLE_TEAM_ID")
		}
		if err != nil {
			missing = append(missing, "DEVICECHECK_PRIVATE_KEY")
		}
		log.Printf("WARN: referral devicecheck not configured (missing %v), mode=%s — %s", missing, mode, deviceCheckOffNote(mode))
		return nil, mode
	}
	c, err := referral.NewAppleDeviceCheck(keyID, teamID, pemBytes)
	if err != nil {
		log.Printf("WARN: referral devicecheck: %v, mode=%s — %s", err, mode, deviceCheckOffNote(mode))
		return nil, mode
	}
	log.Printf("referral devicecheck ready: mode=%s", mode)
	return c, mode
}

func deviceCheckOffNote(mode referral.DeviceCheckMode) string {
	if mode == referral.DeviceCheckLog {
		return "new claims go through unchecked (log mode)"
	}
	return "every new /v1/referral/claim answers 503 device_check_unavailable"
}
