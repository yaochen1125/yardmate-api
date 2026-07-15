package main

import (
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/yaochen1125/yardmate-api/attest"
	"github.com/yaochen1125/yardmate-api/inflight"
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
	inflightLim *inflight.Limiter,
) *Server {
	// Rose cultivar rerank is ON by default; ROSE_RERANK_ENABLED=false kill-switches it.
	roseEnabled := vault.GetBool("ROSE_RERANK_ENABLED", true)
	// Species-level cultivar disambiguation (non-Rosa) is ON by default;
	// CULTIVAR_DISAMBIG_ENABLED=false kill-switches it independently of rose.
	disambigEnabled := vault.GetBool("CULTIVAR_DISAMBIG_ENABLED", true)
	// P1C #3 — Engine↔GPT agreement confidence boost is ON by default;
	// AGREEMENT_BOOST_ENABLED=false kill-switches it (raise-only, never changes
	// which plant is returned).
	agreementBoostEnabled := vault.GetBool("AGREEMENT_BOOST_ENABLED", true)
	// P1C #5 — Bloom-month tiebreak for near-tie in-catalog candidates is ON by
	// default; BLOOM_TIEBREAK_ENABLED=false kill-switches it (tiebreak only,
	// never overrides a clear confidence winner).
	bloomTiebreakEnabled := vault.GetBool("BLOOM_TIEBREAK_ENABLED", true)
	// L1 catalog-native vision-kNN signal (vision/README.md). OFF by default —
	// a new, uncalibrated signal (P0_CONCLUSION: low-strength start, calibrate
	// on staging). When VISION_KNN_ENABLED=true the same-host microservice client
	// is built (VISION_KNN_ENDPOINT overrides the localhost default); a nil
	// client makes HandleIdentify skip it. Fail-open: an unreachable/slow service
	// never blocks the main cascade.
	var visionKNN *proxy.VisionKNNClient
	if vault.GetBool("VISION_KNN_ENABLED", false) {
		visionKNN = proxy.NewVisionKNNClient(vault.Get("VISION_KNN_ENDPOINT"))
	}
	// Geographic prior — forward the client's coarse GPS (latitude/longitude,
	// body-only, never URL/log) to Plant.id's location prior. ON by default;
	// GEO_PRIOR_ENABLED=false kill-switches it (coords are then parsed-and-dropped,
	// never reaching any engine). The iOS toggle is opt-in, so most requests carry
	// no coords regardless — this switch is the server-side circuit breaker.
	geoPriorEnabled := vault.GetBool("GEO_PRIOR_ENABLED", true)
	// #21 High-confidence out-of-catalog escape from rule B. Default OFF — a new
	// selection change. Enable on staging first (OOB_ESCAPE_ENABLED=true) and read
	// the "identify oob-escape" logs (incl. gptInCat) before prod; the GPT arbiter
	// is logged but does NOT gate the decision yet (proxy/SPEC §7 #21).
	oobEscapeEnabled := vault.GetBool("OOB_ESCAPE_ENABLED", false)
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(realIPFromNginx)
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
				// /v1/identify + /v1/diagnose each buffer the uploaded image
				// (≤8 MB) in memory, so they share a concurrency bound — a burst
				// otherwise risks OOM-killing the whole process (inflight/SPEC).
				// Nested sub-group so ONLY these two carry the bound; enrichment
				// (text) / account-delete / signal below stay unbounded. A nil
				// inflightLim (bound disabled / tests) is a pass-through.
				r.Group(func(r chi.Router) {
					r.Use(inflight.Middleware(inflightLim, "server_busy"))
					// Global hourly spend ceiling for the two paid upstream
					// endpoints. Passed INTO the handlers (not mounted as
					// middleware) so it is consumed only AFTER validation, right
					// before the upstream call — a malformed request that 400s
					// early must not draw down the shared budget, or an attacker
					// could exhaust it with cheap invalid requests and deny
					// legitimate paid traffic. Backstops the per-device limit,
					// which resets per fresh UUID (ratelimit.GlobalGate).
					spendGate := ratelimit.GlobalGate(lim.Global, "rate_limit_global")
					// /v1/identify cascades Pl@ntNet (primary) → Plant.id
					// (fallback); register when EITHER engine is present
					// (SPEC §1.1 / §7).
					if plantNet != nil || plantID != nil {
						r.Post("/identify", proxy.HandleIdentify(plantNet, plantID, content, vision, inat, visionKNN, roseEnabled, disambigEnabled, agreementBoostEnabled, bloomTiebreakEnabled, geoPriorEnabled, oobEscapeEnabled, spendGate))
					}
					// /v1/diagnose is Plant.id-only (Pl@ntNet has no health
					// assessment, SPEC §1.5) — still requires plantID.
					if plantID != nil {
						r.Post("/diagnose", proxy.HandleDiagnose(plantID, content, vision, diseaseEnricher, spendGate))
					}
				})
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

		// /v1/feedback — anonymous in-app "Send feedback" messages (More →
		// SUPPORT). Same posture as /v1/plants/signal: outside the per-device
		// expensive-call group, bounded by this /v1 scope's per-IP limit plus a
		// per-device daily cap enforced in SQL (RecordFeedback). The handler
		// requires + validates X-Device-Install-Id itself. Each stored message
		// is also emailed to the operator (FEEDBACK_SMTP_* / FEEDBACK_EMAIL_TO
		// in the Vault; all-absent = mail disabled, rows still stored).
		if enrichDB != nil {
			feedbackMailer := enrichment.NewFeedbackMailer(
				vault.Get("FEEDBACK_SMTP_HOST"), vault.Get("FEEDBACK_SMTP_PORT"),
				vault.Get("FEEDBACK_SMTP_FROM"), vault.Get("FEEDBACK_SMTP_PASS"),
				vault.Get("FEEDBACK_EMAIL_TO"))
			if feedbackMailer == nil {
				log.Printf("feedback mail disabled (FEEDBACK_SMTP_FROM/PASS/EMAIL_TO not all set)")
			}
			r.Post("/feedback", enrichment.HandleFeedback(enrichDB, feedbackMailer))
		}

		// /v1/attribution — Apple Search Ads (AdServices) install attribution.
		// The iOS app posts an AAAttribution token; the server exchanges it at
		// Apple's api-adservices endpoint and stores the campaign/keyword
		// breakdown (proxy/enrichment/attribution.go), deduped per device
		// install id (first write wins). Same posture as /v1/plants/signal:
		// outside the per-device expensive-call group, bounded by this /v1
		// scope's per-IP limit. No IDFA / ATT — a forged token just 404s at
		// Apple (nothing stored). The handler requires + validates
		// X-Device-Install-Id itself.
		if enrichDB != nil {
			r.Post("/attribution", enrichment.HandleAttribution(enrichDB, enrichment.NewAppleAdServicesClient()))
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

// realIPFromNginx overwrites r.RemoteAddr from the single X-Real-IP header that
// our nginx reverse proxy sets explicitly (proxy_set_header X-Real-IP
// $remote_addr), so the per-IP rate limiter keys on the real client IP.
//
// TRUST BOUNDARY: we trust ONLY this one header, and only because nginx sits in
// front of the localhost-bound server and rewrites it with the real TCP peer on
// every request. We deliberately DO NOT honour True-Client-IP or
// X-Forwarded-For — those are client-settable, and chi's middleware.RealIP would
// trust the first XFF hop, letting an attacker forge/rotate source IPs to dodge
// the per-IP rate limit. When the header is absent (a direct hit that bypassed
// nginx, or a unit test), we leave the real TCP RemoteAddr untouched so
// extractIP still keys on the actual connection.
func realIPFromNginx(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := r.Header.Get("X-Real-IP"); ip != "" {
			r.RemoteAddr = ip
		}
		next.ServeHTTP(w, r)
	})
}
