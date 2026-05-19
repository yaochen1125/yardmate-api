package main

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/proxy"
	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/ratelimit"
	"github.com/yaochen1125/yardmate-api/secrets"
)

// Server bundles the chi router with the verifier, vault, rate limiter,
// and (optional) upstream proxy clients + embedded content index it
// serves. Tests construct it via newServer with a test root pool +
// synthetic vault; main constructs it from real production values.
type Server struct {
	verifier *attest.Verifier
	vault    *secrets.Vault
	limiter  *ratelimit.Limiter
	plantNet *proxy.PlantNetClient // optional; PRIMARY identify engine (SPEC §7). nil → Plant.id-only
	plantID  *proxy.PlantIDClient  // optional; identify FALLBACK + sole /v1/diagnose engine. nil disables /v1/diagnose
	vision   *proxy.VisionClient   // optional; nil disables ai_enhance + LLM catalog disambiguation
	content  *proxy.ContentIndex   // optional; nil disables plantId/catalogId lookups in /v1/diagnose
	enrich   *enrichment.Service   // optional; nil disables /v1/plants/enrichment
	router   chi.Router
}

// newServer wires routes. plantNet (PRIMARY identify engine, SPEC §7) and
// plantID (identify FALLBACK + sole /v1/diagnose engine) may each be nil.
// /v1/identify is registered when EITHER is non-nil (cascade degrades to
// whichever is present); /v1/diagnose requires plantID (Pl@ntNet has no
// health assessment). When both identify engines are nil and enrich is nil,
// the per-device group is not registered. content + vision may be nil —
// gracefully degraded (no YardMate cross-reference / no LLM enhancement).
func newServer(
	verifier *attest.Verifier,
	vault *secrets.Vault,
	lim *ratelimit.Limiter,
	plantNet *proxy.PlantNetClient,
	plantID *proxy.PlantIDClient,
	vision *proxy.VisionClient,
	content *proxy.ContentIndex,
	enrich *enrichment.Service,
) *Server {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthz)

	// All /v1 endpoints share the per-IP rate limit. Per-keyID is applied
	// inside /v1/app-secrets after assertion verification (ratelimit/SPEC §4).
	r.Route("/v1", func(r chi.Router) {
		r.Use(ratelimit.PerIPMiddleware(lim.PerIP, "rate_limit_ip"))

		// Fast endpoints — 10 s chi-level timeout fine.
		r.Group(func(r chi.Router) {
			r.Use(middleware.Timeout(10 * time.Second))
			r.Post("/attest/challenge", handleAttestChallenge(verifier))
			r.Post("/attest/register", handleAttestRegister(verifier))
			r.Post("/secrets/challenge", handleSecretsChallenge(verifier))
			r.Post("/app-secrets", handleAppSecrets(verifier, vault, lim.PerKeyID))
		})

		// Slow proxy endpoints — upstream call (Plant.id) can take up to 30 s
		// (proxy/SPEC §5.2). No chi-level Timeout middleware here; the handler
		// manages its own context deadline.
		//
		// Per-device rate limit (in addition to per-IP at /v1 scope) applies
		// here only: these endpoints carry a device install id, and the
		// double-bucket defends against IP-rotation attackers reusing the
		// same client install (proxy/SPEC §4.1, ratelimit/SPEC §4).
		// All per-device endpoints share the per-device rate-limit middleware.
		// /v1/plants/enrichment joins the same group as identify/diagnose so
		// an attacker rotating IPs is still bounded per-device (SPEC §4.1).
		if plantNet != nil || plantID != nil || enrich != nil {
			r.Group(func(r chi.Router) {
				r.Use(ratelimit.PerDeviceMiddleware(lim.PerDevice, "rate_limit_device"))
				// /v1/identify cascades Pl@ntNet (primary) → Plant.id
				// (fallback); register when EITHER engine is present
				// (SPEC §1.1 / §7).
				if plantNet != nil || plantID != nil {
					r.Post("/identify", proxy.HandleIdentify(plantNet, plantID, content, vision))
				}
				// /v1/diagnose is Plant.id-only (Pl@ntNet has no health
				// assessment, SPEC §1.5) — still requires plantID.
				if plantID != nil {
					r.Post("/diagnose", proxy.HandleDiagnose(plantID, content, vision))
				}
				if enrich != nil {
					r.Post("/plants/enrichment", enrichment.HandleEnrichment(enrich))
				}
			})
		}
	})

	return &Server{
		verifier: verifier, vault: vault, limiter: lim,
		plantNet: plantNet, plantID: plantID, vision: vision, content: content,
		enrich: enrich, router: r,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
