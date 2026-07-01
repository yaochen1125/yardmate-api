package main

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/proxy"
	"github.com/yaochen1125/yardmate-api/proxy/enrichment"
	"github.com/yaochen1125/yardmate-api/proxy/imageingest"
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
	enrichDB *enrichment.DB        // optional; shared Supabase pgx pool. nil disables POST /v1/account/delete
	ingest   *imageingest.Service  // optional; nil disables POST /v1/plants/imageingest + /internal/imageingest/run
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
	inat *proxy.INatClient,
	content *proxy.ContentIndex,
	enrich *enrichment.Service,
	diseaseEnricher proxy.DiseaseEnricher,
	ingest *imageingest.Service,
	enrichDB *enrichment.DB,
) *Server {
	// Rose cultivar rerank is ON by default; ROSE_RERANK_ENABLED=false kill-switches it.
	roseEnabled := vault.GetBool("ROSE_RERANK_ENABLED", true)
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/healthz", healthz)

	// Internal image-ingest trigger (proxy/imageingest/SPEC.md §2.1 / §5).
	// TOP-LEVEL route, deliberately OUTSIDE the /v1 group: it is admin-token
	// gated + internal-only (nginx proxies only /v1/* + /healthz publicly, and
	// the server binds localhost), so it must NOT carry the public per-IP /
	// per-device rate-limit middleware. Registered only when the service is
	// configured (R2 + DB + admin token present; nil otherwise → unregistered).
	if ingest != nil {
		r.Post("/internal/imageingest/run", imageingest.HandleRun(ingest))
	}

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
		// /v1/account/delete also joins this group: it carries the same per-IP
		// limit, passes through the per-device middleware (no device id → no-op,
		// ratelimit/SPEC), and only needs the shared Supabase pool (enrichDB).
		if plantNet != nil || plantID != nil || enrich != nil || ingest != nil || enrichDB != nil {
			r.Group(func(r chi.Router) {
				r.Use(ratelimit.PerDeviceMiddleware(lim.PerDevice, "rate_limit_device"))
				// /v1/identify cascades Pl@ntNet (primary) → Plant.id
				// (fallback); register when EITHER engine is present
				// (SPEC §1.1 / §7).
				if plantNet != nil || plantID != nil {
					r.Post("/identify", proxy.HandleIdentify(plantNet, plantID, content, vision, inat, roseEnabled))
				}
				// /v1/diagnose is Plant.id-only (Pl@ntNet has no health
				// assessment, SPEC §1.5) — still requires plantID.
				if plantID != nil {
					r.Post("/diagnose", proxy.HandleDiagnose(plantID, content, vision, diseaseEnricher))
				}
				if enrich != nil {
					r.Post("/plants/enrichment", enrichment.HandleEnrichment(enrich))
				}
				// On-demand out-of-catalog gallery ingest (App Attest log-only,
				// same envelope as /v1/identify — proxy/imageingest/SPEC.md §2.1).
				if ingest != nil {
					r.Post("/plants/imageingest", imageingest.HandlePublic(ingest))
					// In-catalog (AAA-id) supplementary third-party gallery
					// ingest → plant_images/{AAA}/external/ (zero-DB, per-species
					// index.json; proxy/imageingest/SPEC.md §"catalog external").
					r.Post("/plants/catalog-images", imageingest.HandleCatalog(ingest))
				}
				// /v1/account/delete — Supabase account + data deletion + Apple
				// token revoke (account_delete.go). Registered only when the
				// shared Supabase pool is present (row deletes need it).
				if enrichDB != nil {
					r.Post("/account/delete", handleAccountDelete(vault, enrichDB))
				}
			})
		}

		// /v1/plants/signal — cheap fire-and-forget interest counter for
		// library-outside plants (search / garden-add), deduped per device install
		// id, feeding the catalog review tool's promotion-priority badges.
		// Deliberately OUTSIDE the per-device group above: it only needs the per-IP
		// limit (this /v1 scope), and must NOT consume the per-device expensive-call
		// bucket that protects identify / diagnose / enrichment (Codex api#73). The
		// handler still requires + validates X-Device-Install-Id itself.
		if enrichDB != nil {
			r.Post("/plants/signal", enrichment.HandleSignal(enrichDB))
		}
	})

	return &Server{
		verifier: verifier, vault: vault, limiter: lim,
		plantNet: plantNet, plantID: plantID, vision: vision, content: content,
		enrich: enrich, enrichDB: enrichDB, ingest: ingest, router: r,
	}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
