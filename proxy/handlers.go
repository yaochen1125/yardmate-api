package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yaochen1125/yardmate-api/proxy/rosererank"
)

// Body cap for POST /v1/identify: 8 MB image + 1 MB multipart overhead headroom.
// Enforced by http.MaxBytesReader at handler entry per SPEC §4.2.
const identifyMaxBody = 9 << 20 // 9 MiB

// Hard timeout for the upstream Plant.id call (per SPEC §5.2). The chi
// middleware.Timeout (10 s) is overridden via a per-request derived context
// so Plant.id has up to 30 s — the proxy is the slow-path tenant.
const identifyUpstreamTimeout = 30 * time.Second

// aiCatalogRecoveryMinConfidence is the floor the AI vision guess must clear
// to be ACCEPTED as a curated-catalog recovery (SPEC §2.1 catalog-preference
// cascade). When NO engine candidate resolves to the 1522 catalog AND the
// engine was NOT confident (its top candidate < plantnetConfidentSkipAIConfidence),
// the handler asks AI vision for a species and only adopts it (as the
// in-catalog answer) if it (a) resolves to a catalog plantId AND (b)
// self-reports confidence ≥ this threshold. Below it, the engine's own top
// candidate is kept as the out-of-catalog answer (the engine is the trusted
// identifier; AI is only a catalog-recovery probe here, NOT a
// re-identification). 0.10 is a deliberately VERY LOW bar: the user wants to
// MAXIMIZE curated-library (1522) hits and accepts the tradeoff that, once the
// engine was NOT confident (<0.80) and AI's free identification resolves to a
// curated 1522 plant, a curated match — even at low AI confidence — is
// preferred over a likely-worse out-of-catalog enrichment. The user explicitly
// accepts occasional wrong-but-curated answers in exchange for the "rich
// library" feel; see SPEC §7 resolved decisions (do NOT re-debate).
const aiCatalogRecoveryMinConfidence = 0.10

// plantnetConfidentSkipAIConfidence is the engine-confidence gate above which
// the AI catalog-recovery probe is SKIPPED entirely (SPEC §2.1 catalog-
// preference cascade). When NO engine candidate resolves to the 1522 catalog
// but the engine's OWN top candidate (the original top, before any reorder)
// self-reports confidence ≥ this value, the engine is treated as sure of a
// specific out-of-catalog plant: a curated match is unlikely and the AI
// catalog-recovery probe is not worth the GPT-4o latency/cost, so the engine
// top is used as-is (out-of-catalog → iOS enrichment) and `vision` is NOT
// called. Below it (including the no-candidates case where there is no top),
// the AI catalog-recovery path runs as before. 0.80 is a deliberate
// "engine is very sure" bar; see SPEC §7 resolved decisions.
const plantnetConfidentSkipAIConfidence = 0.80

// --- P1C fusion-accuracy tuning (identify #3 / #5) ---

// Engine↔GPT agreement boost (#3, AGREEMENT_BOOST_ENABLED). When the parallel
// GPT-4o arbiter independently resolves to the SAME species as the chosen
// in-catalog suggestion, the two independent signals corroborate each other, so
// the returned Confidence is RAISED (never lowered, plant never changed). The
// boosted value is max(engineConf, gptConf) pulled up to at least the floor and
// clamped to the cap — but never below the engine's own reported confidence.
const (
	agreementBoostFloor = 0.90
	agreementBoostCap   = 0.99
)

// Bloom-month tiebreak (#5, BLOOM_TIEBREAK_ENABLED). When ≥2 in-catalog
// candidates are within this confidence epsilon of the best, the one whose
// BloomMonthsNorth contains the current (Northern-hemisphere) month is
// preferred. A clear winner (no near-tie blooming candidate) is never
// overridden — this is a tiebreak, not a hard filter.
const bloomTiebreakEpsilon = 0.05

// nowMonth returns the current month used by the bloom tiebreak. It is a var so
// tests can pin a deterministic month; production uses the wall clock. Northern
// hemisphere only (no lat/long → no hemisphere flip), matching the tiebreak's
// BloomMonthsNorth semantics.
var nowMonth = func() time.Month { return time.Now().Month() }

// Unknown sentinel (SPEC §2.1 "Unknown sentinel"). When identify cannot name a
// real plant — AI vision explicitly reports is_plant=false, OR every engine
// plus the AI probe yields ZERO suggestions — /v1/identify returns this single
// canonical suggestion instead of an empty list or an upstream placeholder
// name ("N/A" / "Unknown"). plant_id is the RESERVED AAA0000 (the curated 1522
// catalog starts at AAA0001, so it never collides); the iOS client detects it
// to open the "Mysterious plants" easter-egg page and block add-to-garden.
// Contract mirrored in yardmate-swiftui recognition.md / mysterious-plants.md.
const (
	unknownSentinelPlantID        = "AAA0000"
	unknownSentinelScientificName = "Plantae incognita"
	unknownSentinelCommonName     = "Mysterious plants"
	unknownSentinelImageURL       = "https://images.yardmate.ai/plant_images/AAA0000/1_whole.png"
)

// unknownSentinelResult builds the canonical Unknown sentinel IdentifyResult.
// All fields are hardcoded; the handler MUST skip every post-processing step
// (AI rerank, per-suggestion plant_id resolution, common-name upgrade) for a
// sentinel result, else plant_id resolution would overwrite AAA0000 with nil.
func unknownSentinelResult() *IdentifyResult {
	pid := unknownSentinelPlantID
	img := unknownSentinelImageURL
	return &IdentifyResult{
		IsPlant:           false,
		IsPlantConfidence: 0,
		Suggestions: []Suggestion{{
			Name:           unknownSentinelScientificName,
			ScientificName: unknownSentinelScientificName,
			CommonNames:    []string{unknownSentinelCommonName},
			Confidence:     0,
			PlantID:        &pid,
			ImageURL:       &img,
		}},
		AIEnhancedAt: nil,
	}
}

// HandleIdentify returns the http.HandlerFunc for POST /v1/identify.
// See SPEC §2.1, §3, §7 for the contract.
//
// TWO-ENGINE CASCADE (SPEC §1.1 / §7): Pl@ntNet is the PRIMARY engine,
// Plant.id is the FALLBACK. Pl@ntNet is tried once (no per-engine retry).
// The Plant.id fallback fires iff plantNet is nil, OR the Pl@ntNet call
// returned one of ErrPlantNetUnavailable / ErrPlantNetRateLimit /
// ErrPlantNetUnauthorized / ErrPlantNetBadResponse. A successful Pl@ntNet
// answer — INCLUDING a "no match" empty result (upstream 404) — is
// authoritative and does NOT fall back (Plant.id credit must not be spent).
// ErrPlantNetImageRejected also does NOT fall back (Plant.id would reject
// the same bytes) → mapped to bad_image. Plant.id is then tried once; if it
// also fails the wire codes stay plant_id_unavailable / plant_id_unauthorized
// (NOT renamed — iOS error mapping is unchanged; SPEC §3 note).
//
// plantNet (primary) and plantID (fallback) may each be nil; server.go only
// registers the route when at least one is non-nil. Both nil is defended
// against here anyway (502 plant_id_unavailable).
//
// V1 NOTES (per SPEC):
//   - per-IP rate limit is applied by ratelimit.PerIPMiddleware at the /v1
//     scope, and per-deviceInstallId by ratelimit.PerDeviceMiddleware on the
//     proxy endpoint group (both in server.go); this handler does not call
//     either directly.
//   - App Attest assertion headers are read + logged for forensics. V1 does
//     NOT call attest.VerifyAssertion (iOS 26 issue, memory option_d_progress.md).
//
// content is optional. When non-nil, each suggestion's scientific_name is
// resolved to a YardMate plantId via ContentIndex.LookupPlantID — the same
// resolver /v1/diagnose uses (SPEC §2.1 "plant_id mapping"). A catalog miss
// (or nil content) leaves that suggestion's plant_id null; it never changes
// the 200 contract. LookupPlantID is nil-safe so no guard is needed here.
//
// vision is optional and drives TWO independent OpenAI paths here:
//   - ai_enhance rerank: when non-nil AND the request sets ai_enhance=true,
//     the handler asks OpenAI to rerank the top-N candidates against the
//     uploaded image and re-orders Suggestions so the LLM pick is first. On
//     any LLM error / timeout the original engine ranking is preserved and
//     AIEnhancedAt stays null in the response.
//   - catalog-recovery probe (SPEC §1.1 / §2.1 / §7): part of the catalog-
//     preference selection cascade. When the Pl@ntNet→Plant.id cascade
//     SUCCEEDED but NO candidate (across the full up-to-10 set) resolves to
//     the curated 1522 catalog, the handler asks OpenAI (vision, json_schema
//     strict) for a species and adopts it as the in-catalog answer iff it
//     resolves to a catalog plantId AND its self-reported confidence ≥
//     aiCatalogRecoveryMinConfidence. EXCEPTION: if the engine's OWN top
//     candidate self-reports confidence ≥ plantnetConfidentSkipAIConfidence
//     the engine is treated as sure of a specific out-of-catalog plant — the
//     AI probe is SKIPPED (not worth the latency/cost) and the engine top is
//     used as-is. Otherwise (no catalog hit / low AI conf / vision err) the
//     engine's own top candidate is kept as the out-of-catalog answer (engine
//     is the trusted identifier); the zero-engine case still gets the AI
//     guess so a result is always returned (#18). The AI suggestion carries
//     its own confidence and is NOT flagged (product decision: AI provenance
//     not surfaced). vision==nil → no probe; behaves gracefully (engine top
//     or, when the engine also returned nothing, the unchanged "can't
//     identify" empty result). This subsumes the old tier-3 "zero suggestions
//     → AI" block.
//
// visionArbiterResult carries the parallel GPT-4o open-world identify result
// (identify-gpt-arbiter) back to the reconciliation switch.
type visionArbiterResult struct {
	sug *Suggestion
	err error
}

// SpendGate is called once per request AFTER all validation and immediately
// BEFORE the first paid upstream call. It returns true when the request may
// proceed; on a global-budget denial it has already written the 429 and returns
// false (the handler must stop). Injected so the budget is consumed only by
// requests that actually reach upstream — a nil gate always allows
// (ratelimit.GlobalGate).
type SpendGate func(w http.ResponseWriter) bool

// parseCoord parses one coarse GPS coordinate from a multipart body value,
// returning a rounded pointer or nil for anything invalid. It rejects non-finite
// and out-of-range values and rounds to 2 decimals (~1.1 km) as a server-side
// privacy floor even if the client sent finer precision. The returned value is
// forwarded to the location prior only; it is NEVER logged or echoed.
func parseCoord(s string, min, max float64) *float64 {
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < min || v > max {
		return nil
	}
	v = math.Round(v*100) / 100
	return &v
}

func HandleIdentify(plantNet *PlantNetClient, plantID *PlantIDClient, content *ContentIndex, vision *VisionClient, inat *INatClient, visionKNN *VisionKNNClient, roseEnabled, disambigEnabled, agreementBoostEnabled, bloomTiebreakEnabled, geoPriorEnabled bool, spendGate SpendGate) http.HandlerFunc {
	// Rose cultivar rerank candidates, built once here at route registration
	// (startup) and captured by the closure — no server.go/main.go change needed,
	// the factory already receives content (rosererank SPEC §2.2 / §7 #5).
	roseCands := buildRoseCandidates(content)
	roseIDs := roseIDSet(roseCands)
	roseMap := roseByID(roseCands)
	return func(w http.ResponseWriter, r *http.Request) {
		reqStart := time.Now() // WriteTimeout wall-clock start, for the rose budget (SPEC §2.1 #4)

		// 1. Body cap (drops the connection on overflow, returning *MaxBytesError
		//    on the next Read so we can map to image_too_large).
		r.Body = http.MaxBytesReader(w, r.Body, identifyMaxBody)

		// 2. Required headers.
		deviceID := r.Header.Get("X-Device-Install-Id")
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		appVer := r.Header.Get("X-App-Version")
		if appVer == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}

		// 3. Optional App Attest signals (logged only V1).
		attKeyID := r.Header.Get("X-AppAttest-KeyID")
		attAssertPresent := r.Header.Get("X-AppAttest-Assertion") != ""

		// 4. Content-Type must be multipart/form-data.
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			writeError(w, http.StatusBadRequest, "bad_multipart")
			return
		}

		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_multipart")
			return
		}

		// 5. Scan all multipart parts. We need the image bytes plus the
		//    optional ai_enhance flag; either can appear first depending on
		//    client encoding order. multipart.Part doesn't support skip-then-
		//    rewind, so each part is fully consumed when found.
		var (
			imgBytes []byte
			organ    = "auto"
			// Optional geographic prior (identify-geo-prior). Pointers so
			// "absent" is distinguishable from a real 0.0 coordinate. Populated
			// only from the multipart BODY — never a URL/query param — and never
			// logged (SPEC privacy: "Never place personal data in URL params").
			// The iOS toggle is opt-in and already coarsens to ~2 decimals; we
			// re-round server-side as defense-in-depth (~1.1 km cell).
			geoLat, geoLon *float64
		)
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				if isMaxBytesErr(perr) {
					writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
					return
				}
				writeError(w, http.StatusBadRequest, "bad_multipart")
				return
			}
			switch part.FormName() {
			case "image":
				if imgBytes != nil {
					_ = part.Close()
					continue
				}
				b, err := io.ReadAll(part)
				if err != nil {
					_ = part.Close()
					if isMaxBytesErr(err) {
						writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
						return
					}
					writeError(w, http.StatusBadRequest, "bad_image")
					return
				}
				imgBytes = b
			case "organ":
				// Pl@ntNet organ hint (SPEC §2.1). Accept only the known
				// set case-insensitively; anything else / absent → "auto"
				// (already the default). Forwarded to Pl@ntNet only; the
				// Plant.id fallback ignores it.
				b, err := io.ReadAll(io.LimitReader(part, 16))
				if err == nil {
					switch strings.ToLower(strings.TrimSpace(string(b))) {
					case "leaf", "flower", "fruit", "bark", "auto":
						organ = strings.ToLower(strings.TrimSpace(string(b)))
					}
				}
			case "latitude":
				// Body-only geo prior. Parse defensively; a bad value is dropped
				// (nil), never surfaced or logged. 32 bytes is ample for any
				// legal coordinate string.
				if b, e := io.ReadAll(io.LimitReader(part, 32)); e == nil {
					geoLat = parseCoord(string(b), -90, 90)
				}
			case "longitude":
				if b, e := io.ReadAll(io.LimitReader(part, 32)); e == nil {
					geoLon = parseCoord(string(b), -180, 180)
				}
			}
			_ = part.Close()
		}
		if len(imgBytes) == 0 {
			writeError(w, http.StatusBadRequest, "missing_image")
			return
		}

		// 6. MIME byte-sniff first 512 bytes (SPEC §6 pitfall 6). The
		//    multipart Content-Type header from the client is untrusted.
		head := imgBytes
		if len(head) > 512 {
			head = head[:512]
		}
		mime := http.DetectContentType(head)
		if mime != "image/jpeg" && mime != "image/png" {
			writeError(w, http.StatusBadRequest, "bad_image")
			return
		}

		// Global spend ceiling — consumed HERE, after every validation, so a
		// malformed request that 400s above never draws down the shared budget
		// (which would let cheap invalid requests deny legitimate paid traffic).
		if spendGate != nil && !spendGate(w) {
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), identifyUpstreamTimeout)
		defer cancel()

		// --- Parallel GPT-4o open-world arbiter (identify-gpt-arbiter). Fire the
		//     vision identify CONCURRENTLY with the engine cascade so its latency
		//     hides behind the engine's (wall clock ≈ max, not sum). Its guess is
		//     reconciled against the engine in the switch below. Buffered (cap 1)
		//     so the goroutine never blocks if we bail before reading it (cascade
		//     error path); ctx-cancel on return aborts an in-flight call. GPT runs
		//     for EVERY identify now — the arbiter is universal, no ai_enhance /
		//     free-vs-paid branch (free is gated only by request count). ---
		var gptCh chan visionArbiterResult
		if vision != nil {
			gptCh = make(chan visionArbiterResult, 1)
			go func() {
				s, e := vision.IdentifyPlant(ctx, imgBytes, mime)
				gptCh <- visionArbiterResult{sug: s, err: e}
			}()
		}

		// --- Parallel L1 vision-kNN (catalog-native nearest-neighbour over a
		//     real-photo reference index). Same concurrent shape as the GPT
		//     arbiter so its CPU latency hides behind the engine cascade. nil
		//     client (VISION_KNN_ENABLED off) → skipped. Buffered (cap 1) so the
		//     goroutine never blocks if 7a-4 bails before reading it; fail-open. ---
		var knnCh chan visionKNNResult
		if visionKNN != nil {
			knnCh = make(chan visionKNNResult, 1)
			go func() {
				resp, e := visionKNN.Identify(ctx, imgBytes, mime)
				knnCh <- visionKNNResult{resp: resp, err: e}
			}()
		}

		// --- Two-engine cascade (SPEC §1.1 / §7). Single attempt per engine,
		//     no per-engine retry. Pl@ntNet primary → Plant.id fallback. ---
		// `err` is already declared in this scope (from r.MultipartReader);
		// reuse it via plain assignment so it's not redeclared.
		var (
			result *IdentifyResult
			engine string
		)
		// Set true when result is the Unknown sentinel — gates OUT all
		// post-processing (rerank / plant_id resolution / common-name upgrade),
		// which would otherwise overwrite the hardcoded AAA0000 sentinel fields.
		unknownSentinel := false
		err = nil

		// Effective geographic prior: forwarded to the engine ONLY when the
		// GEO_PRIOR_ENABLED kill-switch is on AND both coordinates parsed
		// cleanly. Either missing / disabled → nil pair → a normal identify with
		// no location bias (graceful, no error). Pl@ntNet region mapping is
		// deferred (Phase 2); today only Plant.id consumes the prior.
		var priorLat, priorLon *float64
		if geoPriorEnabled && geoLat != nil && geoLon != nil {
			priorLat, priorLon = geoLat, geoLon
		}

		if plantNet != nil {
			engine = "plantnet"
			result, err = plantNet.Identify(ctx, bytes.NewReader(imgBytes), mime, organ)
		}

		// Decide whether to fall back to Plant.id. Fall back iff Pl@ntNet was
		// not available at all, OR the Pl@ntNet call failed with one of the
		// transient/auth/bad-body sentinels. A successful Pl@ntNet answer
		// (including the 404 empty result) and ErrPlantNetImageRejected do
		// NOT fall back.
		plantNetFellBack := plantNet != nil &&
			(errors.Is(err, ErrPlantNetUnavailable) ||
				errors.Is(err, ErrPlantNetRateLimit) ||
				errors.Is(err, ErrPlantNetUnauthorized) ||
				errors.Is(err, ErrPlantNetBadResponse))

		if (plantNet == nil || plantNetFellBack) && plantID != nil {
			if plantNetFellBack {
				log.Printf("identify plantnet fallback: deviceID=%s err=%v", deviceID, err)
				engine = "plantid-fallback"
			} else {
				engine = "plantid"
			}
			result, err = plantID.Identify(ctx, bytes.NewReader(imgBytes), mime, priorLat, priorLon)
		}

		// --- Catalog-preference selection cascade (SPEC §1.1 / §2.1 / §7).
		//     Runs ONLY when the Pl@ntNet→Plant.id cascade SUCCEEDED
		//     (err == nil). A both-engines-down case keeps err != nil and
		//     falls through UNCHANGED to the 502 mapping below (AI never
		//     substitutes for engine-unavailable — locked decision); wire
		//     codes + image_too_large handling are untouched.
		//
		//     Goal: MAXIMIZE curated-catalog (1522) hits across the FULL
		//     PlantNet/Plant.id candidate set (up to 10), not just [0]:
		//
		//       1. If ANY candidate resolves to the catalog → pick the
		//          in-catalog one with the HIGHEST engine confidence (rule B)
		//          and make it Suggestions[0] (engine=<base>-catalog).
		//          ACCEPTED tradeoff (SPEC §7): a low-score in-catalog
		//          candidate can override a higher-score out-of-catalog one
		//          — chosen knowingly to prefer reviewed data.
		//          This takes precedence even over a high-confidence
		//          out-of-catalog top (no threshold here — unchanged #20).
		//       2. Else (0 in catalog) if the engine's ORIGINAL TOP candidate
		//          (cands[0], before any reorder) has Confidence ≥
		//          plantnetConfidentSkipAIConfidence → the engine is sure of a
		//          specific out-of-catalog plant; use the engine top as-is
		//          (out-of-catalog → iOS enrichment) and DO NOT call vision —
		//          EXCEPT for one GPT-4o vision cross-check that OVERRIDES the
		//          engine only on a curated catalog hit at >= engine confidence
		//          (engine=ai-catalog-override), else keeps the engine top
		//          out-of-catalog (engine=<base>-confident-oob). AI not-a-plant
		//          / low-conf never displaces it. Edge: zero
		//          candidates ⇒ no top ⇒ treated as < the gate ⇒ fall to
		//          step 3 (do NOT skip AI on an empty set).
		//       3. Else (0 in catalog, engine NOT confident) if vision != nil
		//          → ask AI vision to recover a catalog match. If the AI
		//          species resolves to the catalog AND its self-reported
		//          confidence ≥ aiCatalogRecoveryMinConfidence → adopt it as
		//          the sole in-catalog suggestion (engine=ai-catalog-recovery).
		//       4. Else (AI no catalog hit / low conf / vision err / nil):
		//          - cands non-empty → keep the engine's ORIGINAL top
		//            candidate (out-of-catalog, PlantID nil → iOS
		//            enrichment); the engine is the trusted identifier, the
		//            AI guess is NOT used here (engine=<base>-raw-oob).
		//          - cands empty but an AI guess exists (any confidence) →
		//            use it as the single out-of-catalog suggestion so the
		//            "always a result" guarantee holds for the zero-engine
		//            case (engine=ai-raw-oob).
		//          - cands empty and vision nil/err → empty suggestions →
		//            iOS "can't identify" (engine=<base>, unchanged).
		//
		//     The AI suggestion (when used) carries its OWN model-reported
		//     confidence and is NOT flagged (product decision: AI provenance
		//     not surfaced — same stance as the diagnose AI fallback). iOS
		//     sees a normal IdentifyResult; no client change. This subsumes
		//     the old tier-3 "zero suggestions → AI" block (the zero-engine
		//     case is covered by branch 3/4 above; the confident-oob skip in
		//     branch 2 never applies to an empty set). ---
		// GPT-4o arbiter guess/err, hoisted to the handler scope so the #3
		// agreement boost (post-rerank, below) can read it too. Populated only on
		// the err == nil path; nil when vision == nil or the cascade failed.
		var gptSug *Suggestion
		var gptErr error
		if err == nil {
			// Await the parallel GPT arbiter fired at cascade start (gptSug is nil
			// when vision == nil). Errors are handled per-case below (best-effort:
			// a vision failure never blocks the engine result).
			if gptCh != nil {
				arb := <-gptCh
				gptSug, gptErr = arb.sug, arb.err
			}

			base := engine // "plantnet" or "plantid-fallback"/"plantid"
			if base == "plantid" {
				base = "plantid-fallback"
			}

			var cands []Suggestion
			if result != nil {
				cands = result.Suggestions
			}

			// Find the highest-engine-confidence candidate that resolves to
			// the curated catalog (rule B). content may be nil → LookupPlantID
			// is nil-safe and reports no catalog.
			bestIdx := -1
			var bestPID string
			// #5 bloom tiebreak — collect every in-catalog candidate so a
			// near-tie can prefer the one blooming this month (built only when
			// the switch is on; empty/nil otherwise → zero overhead + no-op).
			var inCat []bloomTiebreakCand
			bestPos := -1
			tbMonth := nowMonth()
			for i := range cands {
				// two-pass: original then species-level (resolvePlantID), so
				// a non-top engine candidate whose reported NAME is a
				// subspecies of a catalog species is still picked here — not
				// only resolved in step 7b after [0] has been chosen.
				id, ok := resolvePlantID(content, cands[i].ScientificName)
				if !ok {
					continue
				}
				pos := -1
				if bloomTiebreakEnabled {
					pos = len(inCat)
					inCat = append(inCat, bloomTiebreakCand{
						origIdx:     i,
						plantID:     id,
						confidence:  cands[i].Confidence,
						bloomsMonth: bloomsInMonth(content, id, tbMonth),
					})
				}
				if bestIdx == -1 || cands[i].Confidence > cands[bestIdx].Confidence {
					bestIdx = i
					bestPID = id
					bestPos = pos
				}
			}

			// #5 bloom tiebreak — BLOOM_TIEBREAK_ENABLED. When ≥2 in-catalog
			// candidates are within bloomTiebreakEpsilon of the confidence best
			// and the best does NOT bloom this month, prefer a near-tie candidate
			// that DOES. A clear winner is never overridden (see bloomTiebreak).
			if bloomTiebreakEnabled && bestPos >= 0 && len(inCat) > 1 {
				if winner, changed := bloomTiebreak(inCat, bestPos, bloomTiebreakEpsilon); changed {
					log.Printf("identify bloom tiebreak: deviceID=%s month=%d from=idx%d/%s(%.2f) to=idx%d/%s(%.2f)",
						deviceID, int(tbMonth), bestIdx, bestPID, cands[bestIdx].Confidence,
						winner.origIdx, winner.plantID, winner.confidence)
					bestIdx = winner.origIdx
					bestPID = winner.plantID
				}
			}

			// Engine's ORIGINAL top confidence (cands[0], BEFORE any reorder).
			// Empty set ⇒ no top ⇒ -1 (always < the skip gate), so the AI
			// path is NOT skipped on an empty engine result (Change 1 edge).
			engineTopConf := -1.0
			if len(cands) > 0 {
				engineTopConf = cands[0].Confidence
			}

			switch {
			case bestIdx >= 0:
				// ≥1 candidate in catalog → promote the highest-confidence
				// in-catalog one to [0] and stamp its resolved PlantID. The
				// per-suggestion resolver below re-runs LookupPlantID on the
				// whole slice (idempotent) so [0] keeps a correct PlantID.
				// This wins even over a higher-confidence out-of-catalog top
				// (no threshold here — rule B precedence, unchanged from #20).
				bestConf := cands[bestIdx].Confidence
				if bestIdx != 0 {
					result.Suggestions[0], result.Suggestions[bestIdx] =
						result.Suggestions[bestIdx], result.Suggestions[0]
				}
				pid := bestPID
				result.Suggestions[0].PlantID = &pid
				engine = base + "-catalog"
				// Arbiter reconciliation (identify-gpt-arbiter). The engine can be
				// confidently WRONG about an in-catalog species, OR weakly
				// false-positive a NON-plant onto a catalog species (rule-B has no
				// confidence floor). Two arbiter verdicts act here:
				switch {
				case errors.Is(gptErr, ErrVisionNotAPlant) && bestConf < plantnetConfidentSkipAIConfidence:
					// GPT says not-a-plant AND the engine's catalog hit is weak
					// (< plantnetConfidentSkipAIConfidence) → trust GPT and return
					// the Unknown sentinel, same as the no-catalog recovery branch.
					// A >=0.80 engine catalog hit is still trusted (mirrors
					// confident-oob: guard against a GPT is_plant false-negative).
					result = unknownSentinelResult()
					unknownSentinel = true
					engine = "unknown-sentinel"
				case gptErr == nil && gptSug != nil:
					// GPT resolves to a DIFFERENT catalog plant AND is at least as
					// confident as the engine's in-catalog candidate → adopt GPT's
					// (GPT is the more accurate identifier per real-photo evidence;
					// resolves-to-catalog + >=conf gates guard against a
					// hallucinated override).
					if id, ok := resolvePlantID(content, gptSug.ScientificName); ok &&
						id != bestPID && gptSug.Confidence >= bestConf {
						gpid := id
						gptSug.PlantID = &gpid
						result = &IdentifyResult{
							IsPlant:           true,
							IsPlantConfidence: gptSug.Confidence,
							Suggestions:       []Suggestion{*gptSug},
						}
						engine = "ai-catalog-override"
					}
				}

			case engineTopConf >= plantnetConfidentSkipAIConfidence:
				// 0 candidates in catalog BUT the engine's own top candidate is
				// highly confident (≥ plantnetConfidentSkipAIConfidence) about a
				// specific OUT-OF-catalog plant. The engine can be confidently
				// WRONG here — real case: a California-poppy photo returned as
				// Papaver cambricum (Welsh poppy), an out-of-catalog species not
				// in the 1522 catalog, so it never reaches the bestIdx>=0 case
				// and the app renders an enriched page for the wrong species,
				// even though the correct species (Eschscholzia californica,
				// AAA0505) IS curated. Previously the engine was trusted and
				// vision was skipped entirely, so nothing could catch this.
				//
				// Now we DO run a GPT-4o vision cross-check, but keep it
				// conservative: it may OVERRIDE the confident engine ONLY when
				// its guess resolves to a curated catalog plant AND it is at
				// least as confident as the engine (engineTopConf). A correct
				// confident out-of-catalog engine result is thus never displaced
				// by a weak/hallucinated GPT catalog guess. Vision's
				// not-a-plant verdict and low-confidence guesses are ignored
				// here (the confident engine stays trusted by default). Cost:
				// one GPT-4o call only on this confident-out-of-catalog subset;
				// on a hit we keep the engine's ORIGINAL top out-of-catalog
				// (PlantID nil → iOS enrichment). (len(cands) > 0 is implied —
				// empty set ⇒ engineTopConf = -1.)
				engine = base + "-confident-oob"
				if vision != nil {
					aiSug, verr := gptSug, gptErr
					switch {
					case verr == nil && aiSug != nil:
						if id, ok := resolvePlantID(content, aiSug.ScientificName); ok &&
							aiSug.Confidence >= engineTopConf {
							pid := id
							aiSug.PlantID = &pid
							result = &IdentifyResult{
								IsPlant:           true,
								IsPlantConfidence: aiSug.Confidence,
								Suggestions:       []Suggestion{*aiSug},
							}
							engine = "ai-catalog-override"
						}
					case verr != nil && !errors.Is(verr, ErrVisionNotAPlant):
						log.Printf("identify confident-oob vision cross-check err: deviceID=%s err=%v", deviceID, verr)
					}
				}

			case vision != nil:
				// 0 candidates in catalog AND engine NOT confident (top <
				// plantnetConfidentSkipAIConfidence, or no top) → AI vision
				// catalog-recovery probe (reuses the parallel GPT arbiter guess).
				aiSug, verr := gptSug, gptErr
				var aiPID string
				aiHasPID := false
				if verr == nil && aiSug != nil {
					if id, ok := resolvePlantID(content, aiSug.ScientificName); ok {
						aiPID = id
						aiHasPID = true
					}
				}
				switch {
				case errors.Is(verr, ErrVisionNotAPlant):
					// AI vision EXPLICITLY says the image is not a plant →
					// Unknown sentinel ("Mysterious plants", SPEC §2.1). This is
					// authoritative even if the engine returned low-confidence
					// candidates (engine never resolved to catalog and wasn't
					// confident, else we'd not be here): vision's verdict wins.
					result = unknownSentinelResult()
					unknownSentinel = true
					engine = "unknown-sentinel"
				case verr == nil && aiSug != nil && aiHasPID &&
					aiSug.Confidence >= aiCatalogRecoveryMinConfidence:
					// AI recovered a catalog match with enough confidence →
					// adopt it as the sole in-catalog suggestion.
					pid := aiPID
					aiSug.PlantID = &pid
					result = &IdentifyResult{
						IsPlant:           true,
						IsPlantConfidence: aiSug.Confidence,
						Suggestions:       []Suggestion{*aiSug},
					}
					engine = "ai-catalog-recovery"
				case len(cands) > 0:
					// AI no catalog hit / low conf / generic vision err, but the
					// engine DID return candidates → keep the engine's
					// ORIGINAL top candidate (out-of-catalog → iOS
					// enrichment). The AI guess is NOT used (engine is the
					// trusted identifier; AI was only a catalog probe).
					if verr != nil {
						log.Printf("identify ai-catalog-recovery vision err: deviceID=%s err=%v", deviceID, verr)
					}
					engine = base + "-raw-oob"
				case verr == nil && aiSug != nil:
					// Engine returned ZERO candidates but AI produced a plant
					// guess (any confidence, out-of-catalog) → use it so the
					// "always a result" guarantee holds (#18).
					result = &IdentifyResult{
						IsPlant:           true,
						IsPlantConfidence: aiSug.Confidence,
						Suggestions:       []Suggestion{*aiSug},
					}
					engine = "ai-raw-oob"
				default:
					// Engine returned ZERO candidates AND vision errored (generic
					// failure, not a not-a-plant verdict) → Unknown sentinel so
					// identify still returns a result (was: empty → "can't
					// identify"; now unified to the Mysterious plants easter egg,
					// SPEC §2.1).
					if verr != nil {
						log.Printf("identify ai-vision fallback failed: deviceID=%s err=%v", deviceID, verr)
					}
					result = unknownSentinelResult()
					unknownSentinel = true
					engine = "unknown-sentinel-fallback"
				}

			default:
				// 0 candidates in catalog AND vision == nil (no OPENAI key).
				// cands non-empty → keep engine top as out-of-catalog
				// (PlantID resolved nil by the loop below). cands empty → no
				// engine result and no AI to confirm → Unknown sentinel (SPEC
				// §2.1) so identify still returns a result.
				if len(cands) == 0 {
					result = unknownSentinelResult()
					unknownSentinel = true
					engine = base + "-unknown-sentinel"
				}
			}
		}

		if err != nil {
			if isMaxBytesErr(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
				return
			}
			log.Printf("identify upstream err: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v engine=%s err=%v",
				deviceID, appVer, attKeyID, attAssertPresent, engine, err)
			writeError(w, identifyErrStatus(err), identifyErrCode(err))
			return
		}
		if result == nil {
			// Defensive: both plantNet == nil and plantID == nil (route would
			// not be registered by server.go, but guard anyway). SPEC §3.
			log.Printf("identify no engine: deviceID=%s appVer=%s", deviceID, appVer)
			writeError(w, http.StatusBadGateway, "plant_id_unavailable")
			return
		}

		// 7a. Cultivar refinement — TWO mutually-exclusive vision reranks, at most
		//     one per identify (an else-if chain: Rosa goes to rose rerank, every
		//     other genus to the generic species-group disambiguation). Both are
		//     ON by default with independent server kill-switches, both best-effort
		//     (any failure / timeout / uncertainty falls back to the species
		//     result unchanged), and both share identify's budget via roseBudget.
		//
		// 7a-1. Rose cultivar rerank — ROSE_RERANK_ENABLED=false kill-switches it.
		//     Genus-wide (all ~110 Rosa candidates), for the "every rose comes back
		//     as generic China Rose" problem (rosererank SPEC).
		if roseEnabled && !unknownSentinel && vision != nil &&
			len(result.Suggestions) > 0 && len(roseCands) > 0 &&
			genusOf(result.Suggestions[0].ScientificName) == "Rosa" {
			budget := roseBudget(ctx, reqStart)
			if budget >= minRoseBudget {
				rctx, cancel := context.WithTimeout(ctx, budget)
				res, verr := vision.RerankRose(rctx, imgBytes, mime, roseCands)
				cancel()
				if verr != nil {
					log.Printf("identify rose rerank failed: deviceID=%s err=%v", deviceID, verr)
				} else {
					matches, ok := rosererank.Decide(res, roseIDs)
					topID, topConf := "", 0.0
					if len(res.Matches) > 0 {
						topID, topConf = res.Matches[0].PlantID, res.Matches[0].Confidence
					}
					// Observability (rose-rerank-observability): make the silent
					// fallback visible — why did the rose land where it did?
					log.Printf("identify rose rerank: deviceID=%s certain=%v rawMatches=%d topId=%s topConf=%.2f applied=%v",
						deviceID, res.CultivarCertain, len(res.Matches), topID, topConf, ok)
					if ok {
						rewriteSuggestionsFromRose(result, matches, roseMap)
					}
				}
			} else {
				log.Printf("identify rose rerank: deviceID=%s SKIPPED-budget budget=%v min=%v", deviceID, budget, minRoseBudget)
			}

			// 7a-2. Species-level cultivar disambiguation (non-Rosa) —
			//     CULTIVAR_DISAMBIG_ENABLED=false kill-switches it. Fires ONLY when
			//     the engine resolved to a BARE species (isBareSpecies: no cultivar
			//     quote, no infraspecific marker) whose catalog holds ≥2 rows
			//     (species + cultivars/varieties, e.g. Juncus effusus AAA0701 +
			//     'Spiralis' AAA1633). A vision pass over just those group members
			//     picks the right row; on any uncertainty it falls back to the
			//     first-write-wins species result. Genus Rosa is excluded (handled
			//     by 7a-1 above, even when that branch is kill-switched off) so the
			//     two never both fire.
		} else if disambigEnabled && !unknownSentinel && vision != nil &&
			len(result.Suggestions) > 0 && content != nil &&
			genusOf(result.Suggestions[0].ScientificName) != "Rosa" &&
			isBareSpecies(result.Suggestions[0].ScientificName) {
			key := speciesGroupKey(result.Suggestions[0].ScientificName)
			if group, ok := content.SpeciesGroupFor(key); ok && len(group) > 1 {
				budget := roseBudget(ctx, reqStart)
				if budget >= minRoseBudget {
					rctx, cancel := context.WithTimeout(ctx, budget)
					res, verr := vision.RerankCultivar(rctx, imgBytes, mime, group)
					cancel()
					if verr != nil {
						log.Printf("identify cultivar disambig failed: deviceID=%s key=%q err=%v", deviceID, key, verr)
					} else {
						matches, applied := rosererank.Decide(res, roseIDSet(group))
						topID, topConf := "", 0.0
						if len(res.Matches) > 0 {
							topID, topConf = res.Matches[0].PlantID, res.Matches[0].Confidence
						}
						log.Printf("identify cultivar disambig: deviceID=%s key=%q members=%d certain=%v rawMatches=%d topId=%s topConf=%.2f applied=%v",
							deviceID, key, len(group), res.CultivarCertain, len(res.Matches), topID, topConf, applied)
						if applied {
							rewriteSuggestionsFromRose(result, matches, roseByID(group))
						}
					}
				} else {
					log.Printf("identify cultivar disambig: deviceID=%s SKIPPED-budget key=%q budget=%v min=%v", deviceID, key, budget, minRoseBudget)
				}
			}
		}

		// 7a-3. Engine↔GPT agreement boost — AGREEMENT_BOOST_ENABLED. The
		//     parallel GPT-4o arbiter ran for every identify; when it
		//     independently resolves to the SAME species as the chosen in-catalog
		//     suggestion, the two signals corroborate one another, so we RAISE the
		//     returned confidence. Strictly bounded: fires ONLY on an in-catalog
		//     hit (Suggestions[0].PlantID != nil) with GPT agreement; it NEVER
		//     lowers confidence and NEVER changes which plant is returned
		//     (plant_id / scientific_name untouched). GPT missing / errored /
		//     disagreeing → no-op (original behavior). Placed after the cultivar
		//     reranks so it reflects the final chosen row.
		//     Codex #98: SKIP when the chosen row ITSELF came from the GPT arbiter
		//     (engine "ai-catalog-override" / "ai-catalog-recovery" / "ai-raw-oob",
		//     all "ai-"-prefixed). Comparing a GPT-sourced result back to the same
		//     gptSug is circular self-agreement, not independent corroboration — it
		//     would inflate a low-confidence ai-catalog-recovery straight to the
		//     boost floor, defeating the recovery threshold. Only engine-sourced
		//     (Pl@ntNet / Plant.id) in-catalog hits get the boost.
		if agreementBoostEnabled && !unknownSentinel && !strings.HasPrefix(engine, "ai-") &&
			gptErr == nil && gptSug != nil &&
			len(result.Suggestions) > 0 && result.Suggestions[0].PlantID != nil {
			s0 := &result.Suggestions[0]
			engKey := speciesKey(s0.ScientificName)
			if engKey != "" && engKey == speciesKey(gptSug.ScientificName) {
				if boosted := boostedConfidence(s0.Confidence, gptSug.Confidence); boosted > s0.Confidence {
					log.Printf("identify agreement boost: deviceID=%s plantId=%s engineConf=%.2f gptConf=%.2f boostedConf=%.2f",
						deviceID, *s0.PlantID, s0.Confidence, gptSug.Confidence, boosted)
					s0.Confidence = boosted
				}
			}
		}

		// 7a-4. L1 vision-kNN corroboration — VISION_KNN_ENABLED. The parallel
		//     catalog-native nearest-neighbour pass (real-photo reference index)
		//     ran for every identify. v1 is deliberately CONSERVATIVE (P0: a new,
		//     uncalibrated signal): it ACTS only to RAISE confidence — via the
		//     GENTLE visionKNNBoostedConfidence (a capped partial nudge toward the
		//     visual similarity, NOT the GPT boost's 0.90 floor) — when vision
		//     independently agrees with the chosen in-catalog row (never lowers,
		//     never changes the plant). Unlike 7a-3 this signal IS independent of
		//     the GPT arbiter, so it may corroborate a GPT-sourced recovery too, but
		//     only gently. Every other outcome — disagreement inside the catalog, or
		//     an out-of-catalog verdict against an in-catalog decision — is LOGGED
		//     ONLY, to gather staging data before we let it override. Fail-open:
		//     nil client / error / timeout → no-op.
		if visionKNN != nil && knnCh != nil && !unknownSentinel && len(result.Suggestions) > 0 {
			// Bounded wait (Codex #100): the goroutine ran concurrently with the
			// whole cascade, so a healthy local service is usually already done
			// here. Cap the extra wait at visionKNNWaitBudget so a slow/wedged
			// service can't add its full HTTP timeout to the already-decided user
			// response — skip the optional signal instead of blocking on it.
			select {
			case kr := <-knnCh:
				if kr.err != nil || kr.resp == nil {
					log.Printf("identify vision-knn failed: deviceID=%s err=%v", deviceID, kr.err)
				} else {
					s0 := &result.Suggestions[0]
					knnTopID, knnTopSim := "", 0.0
					if len(kr.resp.Candidates) > 0 {
						knnTopID, knnTopSim = kr.resp.Candidates[0].CatalogID, kr.resp.Candidates[0].VisionSim
					}
					decidedID := ""
					if s0.PlantID != nil {
						decidedID = *s0.PlantID
					}
					agrees := visionKNNAgreesWithDecision(s0, kr.resp)
					log.Printf("identify vision-knn: deviceID=%s decidedId=%s knnTopId=%s knnTopSim=%.3f inCatalog=%v nnSim=%.3f agrees=%v",
						deviceID, decidedID, knnTopID, knnTopSim, kr.resp.InCatalog, kr.resp.NNSim, agrees)
					if agrees {
						if boosted := visionKNNBoostedConfidence(s0.Confidence, knnTopSim); boosted > s0.Confidence {
							log.Printf("identify vision-knn boost: deviceID=%s plantId=%s conf=%.2f knnSim=%.2f boostedConf=%.2f",
								deviceID, *s0.PlantID, s0.Confidence, knnTopSim, boosted)
							s0.Confidence = boosted
						}
					}
				}
			case <-time.After(visionKNNWaitBudget):
				// Still running past the grace budget — skip rather than delay the
				// response. The buffered goroutine finishes into knnCh and is GC'd;
				// ctx-cancel on return aborts the in-flight call.
				log.Printf("identify vision-knn: deviceID=%s SKIPPED-not-ready-within=%v", deviceID, visionKNNWaitBudget)
			}
		}

		// 7b. Resolve YardMate plantId per suggestion from scientific_name
		//     (SPEC §2.1 "plant_id mapping"). Same resolver /v1/diagnose uses
		//     at the handler layer; content/LookupPlantID are nil-safe. Done
		//     after the optional rerank but order-independent (per-suggestion).
		//     SKIPPED for the Unknown sentinel: resolving "Plantae incognita"
		//     would miss and overwrite the hardcoded AAA0000 with nil.
		plantIDsResolved := 0
		if !unknownSentinel {
			for i := range result.Suggestions {
				sci := result.Suggestions[i].ScientificName
				if id, ok := resolvePlantID(content, sci); ok {
					pid := id
					result.Suggestions[i].PlantID = &pid
					plantIDsResolved++
				}
				result.Suggestions[i].ScientificName = speciesBinomial(sci) // display species-level (SPEC §2.1)
			}

			// 7b-2. Upgrade the PRIMARY suggestion's common name (Q2: top1 only).
			// Priority (SPEC §2.1): curated catalog common_name > iNat preferred >
			// upstream engine names. Best-effort — a miss/error keeps upstream.
			if len(result.Suggestions) > 0 {
				s0 := &result.Suggestions[0]
				if s0.PlantID != nil {
					if cn, ok := content.LookupCommonName(*s0.PlantID); ok {
						s0.CommonNames = prependUnique(cn, s0.CommonNames)
					}
				} else if inat != nil {
					// Best-effort common-name upgrade — MUST NOT dominate the
					// identify latency budget. The species is already resolved;
					// cap this to a short derived deadline so a slow iNat (its
					// own client Timeout is 8 s) can't stall the response. A
					// timeout just returns ("",false) and keeps upstream names.
					const inatCommonNameTimeout = 3 * time.Second
					inatCtx, inatCancel := context.WithTimeout(ctx, inatCommonNameTimeout)
					if cn, ok := inat.PreferredCommonName(inatCtx, s0.ScientificName); ok {
						s0.CommonNames = prependUnique(cn, s0.CommonNames)
					}
					inatCancel()
				}
			}
		}

		// 7c. Trim the RESPONSE to top-3 (SPEC §2.1 "top 3 suggestions max").
		//     Selection above happened across the FULL set (up to 10) so the
		//     curated-catalog winner could be found at any rank; the chosen
		//     candidate is already Suggestions[0]. Keep [0] (the decision) and
		//     append the next 2 highest-`confidence` of the remainder so the
		//     payload stays bounded and the chosen plant is never dropped. The
		//     AI-recovery / ai-raw-oob paths already hold exactly 1 suggestion
		//     → this is a no-op there. iOS contract unchanged (navigates [0]).
		const maxResponseSuggestions = 3
		if len(result.Suggestions) > maxResponseSuggestions {
			head := result.Suggestions[0]
			rest := append([]Suggestion(nil), result.Suggestions[1:]...)
			sort.SliceStable(rest, func(i, j int) bool {
				return rest[i].Confidence > rest[j].Confidence
			})
			trimmed := make([]Suggestion, 0, maxResponseSuggestions)
			trimmed = append(trimmed, head)
			trimmed = append(trimmed, rest[:maxResponseSuggestions-1]...)
			result.Suggestions = trimmed
		}

		// 7d. Final nameless guard — never return a top suggestion the client
		//     cannot turn into a loadable plant detail. The engine converters
		//     already drop blank/genus-only candidates at the source; this
		//     backstops the AI-vision paths (ai-catalog-recovery / ai-raw-oob)
		//     and any future producer. If Suggestions[0] has NO plant_id AND no
		//     usable scientific name, iOS would store a dead Recent-snaps record
		//     whose detail can never load — fall back to the Unknown sentinel,
		//     which iOS refuses to record. Skipped when already the sentinel.
		if !unknownSentinel && len(result.Suggestions) > 0 {
			s0 := result.Suggestions[0]
			noPID := s0.PlantID == nil || strings.TrimSpace(*s0.PlantID) == ""
			if noPID && !hasUsableScientificName(s0.ScientificName) {
				log.Printf("identify nameless-guard: deviceID=%s engine=%s sci=%q dropped→unknown-sentinel", deviceID, engine, s0.ScientificName)
				result = unknownSentinelResult()
				unknownSentinel = true
				engine += "-nameless-guard"
			}
		}

		// 8. Success — single-line structured log (SPEC §5.2 forensics).
		//    suggestionsWithImage counts how many carry a Pl@ntNet reference
		//    image_url (always 0 on the Plant.id fallback path). catalogHit is
		//    true iff the chosen Suggestions[0] resolved to a curated catalog
		//    plantId (catalog-preference cascade observability, SPEC §2.1).
		suggestionsWithImage := 0
		for i := range result.Suggestions {
			if result.Suggestions[i].ImageURL != nil {
				suggestionsWithImage++
			}
		}
		catalogHit := len(result.Suggestions) > 0 && result.Suggestions[0].PlantID != nil
		log.Printf("identify ok: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v engine=%s mime=%s isPlant=%v suggestions=%d plantIdsResolved=%d catalogHit=%v suggestionsWithImage=%d aiEnhanced=%v",
			deviceID, appVer, attKeyID, attAssertPresent, engine, mime, result.IsPlant, len(result.Suggestions), plantIDsResolved, catalogHit, suggestionsWithImage, result.AIEnhancedAt != nil)
		writeJSON(w, http.StatusOK, result)
	}
}

// identifyErrCode maps the final cascade error to the stable wire code
// (SPEC §3). The codes are NOT renamed — `plant_id_unavailable` /
// `plant_id_unauthorized` now denote "all identification engines down", not
// literally Plant.id, so the iOS error mapping is unchanged. Both the
// Pl@ntNet sentinels (when no Plant.id fallback was available) and the
// Plant.id sentinels (after fallback) funnel through here.
func identifyErrCode(err error) string {
	switch {
	case errors.Is(err, ErrPlantNetImageRejected), errors.Is(err, ErrPlantIDImageRejected):
		return "bad_image"
	case errors.Is(err, ErrPlantNetUnauthorized), errors.Is(err, ErrPlantIDUnauthorized):
		return "plant_id_unauthorized"
	default:
		// ErrPlantNetRateLimit / ErrPlantNetUnavailable / ErrPlantNetBadResponse
		// / ErrPlantIDRateLimit / ErrPlantIDUnavailable / ErrPlantIDBadResponse
		// and any unmapped error → identification unavailable.
		return "plant_id_unavailable"
	}
}

// identifyErrStatus is the HTTP status paired with identifyErrCode.
func identifyErrStatus(err error) int {
	switch {
	case errors.Is(err, ErrPlantNetImageRejected), errors.Is(err, ErrPlantIDImageRejected):
		return http.StatusBadRequest // 400
	default:
		return http.StatusBadGateway // 502
	}
}

// --- helpers (local to proxy package; small enough to duplicate vs export from main) ---

// resolvePlantID is the identify-side two-pass plant_id resolver: the original
// scientific_name first (so a curated subspecies row like "Ceanothus griseus
// horizontalis" still hits), then — on a miss — the speciesBinomial
// species-level form (so a reported subspecies whose SPECIES is in the catalog
// resolves to it, SPEC §2.1). Used by BOTH the catalog-preference selection
// (so the cascade can pick a subspecies candidate whose species is in catalog
// — without this, finding 2 of the PR #24 review) AND the per-suggestion
// resolver in step 7b. /v1/diagnose stays single-pass. content nil-safe.
func resolvePlantID(content *ContentIndex, sci string) (string, bool) {
	if id, ok := content.LookupPlantID(sci); ok {
		return id, true
	}
	species := speciesBinomial(sci)
	if species == sci {
		return "", false
	}
	return content.LookupPlantID(species)
}

// --- P1C fusion-accuracy helpers (identify #3 / #5) ---

// speciesKey reduces a scientific name to its species-level catalog key —
// speciesBinomial to drop any cultivar/infraspecific tail, then
// normalizeScientificName (the same key LookupPlantID uses) so casing, hybrid
// markers and spacing don't matter. Used to compare the GPT arbiter's guess
// against the chosen suggestion for the #3 agreement boost. "" on empty input.
func speciesKey(sci string) string {
	return normalizeScientificName(speciesBinomial(sci))
}

// boostedConfidence computes the #3 agreement-boosted confidence when the engine
// and the GPT arbiter independently agree on the species. It takes the higher of
// the two confidences, pulls it up to at least agreementBoostFloor and clamps to
// agreementBoostCap — but NEVER returns below engineConf (the boost only ever
// raises; if the engine already reports above the cap that value is preserved).
func boostedConfidence(engineConf, gptConf float64) float64 {
	boosted := math.Max(engineConf, gptConf)
	if boosted < agreementBoostFloor {
		boosted = agreementBoostFloor
	}
	if boosted > agreementBoostCap {
		boosted = agreementBoostCap
	}
	if boosted < engineConf {
		boosted = engineConf
	}
	return boosted
}

// bloomTiebreakCand is one in-catalog candidate the #5 bloom tiebreak considers:
// its position in the engine's original candidate slice, its resolved catalog
// plantId, its engine confidence, and whether its BloomMonthsNorth contains the
// target month.
type bloomTiebreakCand struct {
	origIdx     int
	plantID     string
	confidence  float64
	bloomsMonth bool
}

// bloomTiebreak applies the #5 near-tie bloom preference over the in-catalog
// candidates (in engine order). bestPos is the index within `cands` of the
// confidence winner (from the caller's selection loop). Rules:
//   - If the confidence best already blooms this month, it is already the
//     preferred pick → returned unchanged (changed=false).
//   - Otherwise, among candidates within epsilon of the best's confidence
//     (near-ties) that DO bloom this month, the highest-confidence one is
//     chosen. A clear winner — no near-tie blooming candidate — is never
//     overridden (changed=false). This is a tiebreak, not a hard filter.
//
// Candidates with no bloom data have bloomsMonth=false, so they never win a
// tiebreak but also never block one.
func bloomTiebreak(cands []bloomTiebreakCand, bestPos int, epsilon float64) (bloomTiebreakCand, bool) {
	best := cands[bestPos]
	if best.bloomsMonth {
		return best, false
	}
	winner := best
	changed := false
	for i := range cands {
		c := cands[i]
		if i == bestPos || !c.bloomsMonth {
			continue
		}
		if c.confidence < best.confidence-epsilon {
			continue // beyond the near-tie window — clear loser, not a tie
		}
		if !changed || c.confidence > winner.confidence {
			winner = c
			changed = true
		}
	}
	return winner, changed
}

// bloomsInMonth reports whether the catalog plant's BloomMonthsNorth contains
// `month` (Northern hemisphere). Missing detail / empty bloom data → false, so
// such candidates simply don't participate in the #5 tiebreak. content nil-safe.
func bloomsInMonth(content *ContentIndex, plantID string, month time.Month) bool {
	det, ok := content.LookupFullDetail(plantID)
	if !ok || det == nil {
		return false
	}
	for _, m := range det.BloomMonthsNorth {
		if time.Month(m) == month {
			return true
		}
	}
	return false
}

// isBareSpecies reports whether an engine-reported scientific name is a plain
// "Genus species" binomial with NO cultivar quote and NO infraspecific rank
// marker — i.e. the engine only resolved to the species level. It gates the
// cultivar-disambiguation rerank (7a-2): we disambiguate ONLY when the engine
// gave us just the species (so the catalog's several cultivars under it are
// genuinely ambiguous). A name the engine already pinned to a variety/subspecies
// (e.g. "Brassica oleracea var. italica") is precise-resolved by resolvePlantID
// and must NOT be re-disambiguated. normalizeScientificNamePrecise keeps the
// infraspecific/hybrid marker, so a bare binomial normalizes to exactly 2
// whitespace tokens ("brassica oleracea var. italica" -> 4, "abelia x
// grandiflora" -> 3), and a quoted cultivar is rejected outright.
func isBareSpecies(sci string) bool {
	if strings.ContainsAny(sci, "'’") {
		return false
	}
	return len(strings.Fields(normalizeScientificNamePrecise(sci))) == 2
}

// prependUnique returns name followed by list with any case-insensitive
// duplicate of name removed — puts the resolved common name first without
// duplicating it when the upstream list already contained it.
func prependUnique(name string, list []string) []string {
	out := make([]string, 0, len(list)+1)
	out = append(out, name)
	for _, n := range list {
		if !strings.EqualFold(n, name) {
			out = append(out, n)
		}
	}
	return out
}

type errorResponse struct {
	Error string `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, errorResponse{Error: code})
}

func isMaxBytesErr(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// isUUID accepts RFC 4122 canonical form (36 chars with dashes at positions
// 8/13/18/23). Case-insensitive for hex digits. iOS NSUUID().uuidString
// always produces uppercase-canonical form.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < 36; i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
				return false
			}
		}
	}
	return true
}

// --- diagnose (POST /v1/diagnose, SPEC §2.2) ---

// diagnoseMaxBody = identifyMaxBody (same 8 MB image cap + multipart overhead).
const diagnoseMaxBody = identifyMaxBody

// diagnoseUpstreamTimeout caps the Plant.id call, anchored at reqStart (request
// entry) — NOT "now" — so a slow multipart upload counts against it and the
// upstream attempt can't run past the diagnoseWallClockBudget ceiling, leaving
// the AI fallback / static net room to write the 200 before the server
// WriteTimeout (Codex #48 P2). Must be ≤ diagnoseWallClockBudget. The
// /v1/diagnose route runs with NO chi-level Timeout middleware (server.go "Slow
// proxy endpoints"), so the handler manages its own deadline; vision
// disambiguation runs inside the same context but has its own client timeout
// (≤8 s) inside VisionClient.
const diagnoseUpstreamTimeout = 30 * time.Second

// diagnoseWallClockBudget bounds the WHOLE diagnose handler (Plant.id attempt +
// the optional GPT-4o vision fallback) by the 35 s server WriteTimeout (main.go)
// measured from request start — NOT from the upstream ctx, which is created only
// AFTER the multipart body is read. Go resets WriteTimeout at header-read, so a
// slow upload counts against it: a deadline anchored post-upload could let the
// 200 fallback response land past WriteTimeout (Codex #48 P2, the same trap the
// rose roseWallClockBudget fixed in #44 P2). 30 s leaves ~5 s write margin.
const diagnoseWallClockBudget = 30 * time.Second

// minDiagnoseFallbackBudget is the floor below which the vision fallback is not
// worth attempting: a real look-at-the-photo diagnosis needs several seconds, so
// below this (e.g. Plant.id consumed nearly the whole wall clock before failing)
// we skip straight to the instant static safety net rather than spend a doomed
// OpenAI call that would itself risk the WriteTimeout. Mirrors minRoseBudget.
const minDiagnoseFallbackBudget = 6 * time.Second

// HandleDiagnose returns the http.HandlerFunc for POST /v1/diagnose.
// Combines Plant.id v3 health_assessment with YardMate catalog lookups
// (content) and an optional LLM disambiguation pass (vision). See SPEC §2.2.
//
// Diagnose NEVER returns a healthy result: a healthy verdict (from Plant.id or
// the GPT-4o fallback) is force-picked into a disease (SPEC §2.2 "Never
// healthy"). content / vision may be nil — both are graceful no-ops (plantId
// stays null, catalogId falls back to name-match only); even with both nil the
// force-pick still ships the generic Leaf-spot tail, so issues is never empty.
func HandleDiagnose(client *PlantIDClient, content *ContentIndex, vision *VisionClient, enricher DiseaseEnricher, spendGate SpendGate) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		reqStart := time.Now() // WriteTimeout wall-clock start, for the AI fallback budget (mirrors HandleIdentify)
		r.Body = http.MaxBytesReader(w, r.Body, diagnoseMaxBody)

		// X-Device-Install-Id is validated by ratelimit.PerDeviceMiddleware
		// in server.go; we re-read it here for logging only.
		deviceID := r.Header.Get("X-Device-Install-Id")
		appVer := r.Header.Get("X-App-Version")
		if appVer == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}
		// Also accept the legacy device-id check at the handler boundary so
		// that direct-call tests (without the middleware) still get the
		// expected 400.
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		attKeyID := r.Header.Get("X-AppAttest-KeyID")
		attAssertPresent := r.Header.Get("X-AppAttest-Assertion") != ""

		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			writeError(w, http.StatusBadRequest, "bad_multipart")
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_multipart")
			return
		}

		// Stream the parts: buffer the image, and capture the optional `lang`
		// display-language hint (iOS sends it as a multipart form field). lang may
		// arrive before OR after the image part, so we read all parts to EOF rather
		// than breaking on `image` (mirrors HandleIdentify's organ/ai_enhance loop).
		var imgBytes []byte
		var lang string
		for {
			part, perr := mr.NextPart()
			if perr == io.EOF {
				break
			}
			if perr != nil {
				if isMaxBytesErr(perr) {
					writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
					return
				}
				writeError(w, http.StatusBadRequest, "bad_multipart")
				return
			}
			switch part.FormName() {
			case "image":
				if imgBytes != nil {
					_ = part.Close()
					continue
				}
				// Read full image bytes — Diagnose has to base64-encode the body
				// upstream, so we buffer once here (bounded by the 9 MB cap above).
				b, rerr := io.ReadAll(part)
				if rerr != nil {
					_ = part.Close()
					if isMaxBytesErr(rerr) {
						writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
						return
					}
					writeError(w, http.StatusBadRequest, "bad_image")
					return
				}
				imgBytes = b
			case "lang":
				// BCP-47 tag; NormalizeLang maps empty / unsupported → "en".
				// Bounded read — a lang tag is short (e.g. "zh-Hant").
				b, err := io.ReadAll(io.LimitReader(part, 32))
				if err == nil {
					lang = strings.TrimSpace(string(b))
				}
			}
			_ = part.Close()
		}
		if imgBytes == nil {
			writeError(w, http.StatusBadRequest, "missing_image")
			return
		}
		if len(imgBytes) < 12 {
			writeError(w, http.StatusBadRequest, "bad_image")
			return
		}
		// MIME sniff on actual bytes (SPEC §6 pitfall 6) — multipart Content-Type
		// from the client is untrusted.
		head := imgBytes
		if len(head) > 512 {
			head = head[:512]
		}
		mime := http.DetectContentType(head)
		if mime != "image/jpeg" && mime != "image/png" {
			writeError(w, http.StatusBadRequest, "bad_image")
			return
		}

		// Global spend ceiling — consumed HERE, after every validation, so a
		// malformed request that 400s above never draws down the shared budget
		// (which would let cheap invalid requests deny legitimate paid traffic).
		if spendGate != nil && !spendGate(w) {
			return
		}

		// Anchored at reqStart (not now) so a slow upload is charged against the
		// upstream budget too — the whole handler (Plant.id + fallback/static net)
		// then fits under the 35 s server WriteTimeout, so even a slow-upload +
		// Plant.id hang still delivers its 200 in time (Codex #48 P2).
		ctx, cancel := context.WithDeadline(r.Context(), reqStart.Add(diagnoseUpstreamTimeout))
		defer cancel()

		api, err := client.Diagnose(ctx, imgBytes, mime)
		if err != nil {
			log.Printf("diagnose upstream err: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v err=%v",
				deviceID, appVer, attKeyID, attAssertPresent, err)
			// Plant.id-down AI vision fallback (SPEC §2.2). When Plant.id is
			// rate-limited (e.g. account balance exhausted → 429) or otherwise
			// unavailable AND a vision client is configured, diagnose the plant
			// directly from the image with GPT-4o instead of 502-ing. Narrow by
			// design: bad_image / unauthorized fall through to their own codes,
			// and Plant.id stays PRIMARY — this is the error path, so a recovered
			// Plant.id account is served by Plant.id and never pre-empted by AI.
			if (errors.Is(err, ErrPlantIDRateLimit) || errors.Is(err, ErrPlantIDUnavailable)) && vision != nil {
				// Budget the fallback on the wall clock from reqStart, NOT on ctx:
				// (1) ctx may already be expired here — a Plant.id *timeout* fails
				//     only once ctx hit diagnoseUpstreamTimeout (its own HTTP client
				//     cap is the same 30 s) — so the call hangs off the still-live
				//     r.Context() (this route has no chi Timeout); and
				// (2) the budget must respect the 35 s server WriteTimeout measured
				//     from request start, so a slow upload + the vision call can't
				//     push the 200 past it (Codex #48 P2; rose roseBudget precedent).
				// Too little wall clock left → skip the doomed OpenAI call and ship
				// the instant static net (still 200, never 502 — diagnose 不全废).
				if budget := diagnoseFallbackBudget(reqStart); budget >= minDiagnoseFallbackBudget {
					fbCtx, fbCancel := context.WithTimeout(r.Context(), budget)
					defer fbCancel()
					result, viaVision := buildDiagnoseResultViaVision(fbCtx, imgBytes, mime, content, vision, enricher, reqStart, lang)
					// Distinct prefix from the "diagnose fallback ai" disease-pick
					// layer (buildFallbackIssue) so the two AI paths stay separable
					// in logs. viaVision=false means the vision call itself failed
					// and the static safety net was used.
					log.Printf("diagnose vision fallback done: deviceID=%s appVer=%s viaVision=%v isHealthy=%v issues=%d plantIdResolved=%v",
						deviceID, appVer, viaVision, result.IsHealthy, len(result.Issues), result.PlantID != nil)
					writeJSON(w, http.StatusOK, result)
					return
				}
				result := diagnoseStaticNetResult(ctx, content, vision)
				log.Printf("diagnose vision fallback done: deviceID=%s appVer=%s viaVision=false budgetSkipped=true isHealthy=%v issues=%d plantIdResolved=false",
					deviceID, appVer, result.IsHealthy, len(result.Issues))
				writeJSON(w, http.StatusOK, result)
				return
			}
			switch {
			case errors.Is(err, ErrPlantIDImageRejected):
				writeError(w, http.StatusBadRequest, "bad_image")
			case errors.Is(err, ErrPlantIDUnauthorized):
				writeError(w, http.StatusBadGateway, "plant_id_unauthorized")
			case errors.Is(err, ErrPlantIDRateLimit), errors.Is(err, ErrPlantIDUnavailable):
				writeError(w, http.StatusBadGateway, "plant_id_unavailable")
			default:
				writeError(w, http.StatusBadGateway, "plant_id_unavailable")
			}
			return
		}

		result := buildDiagnoseResult(ctx, api, content, vision, enricher, reqStart, lang)
		log.Printf("diagnose ok: deviceID=%s appVer=%s isHealthy=%v issues=%d plantIdResolved=%v",
			deviceID, appVer, result.IsHealthy, len(result.Issues), result.PlantID != nil)
		writeJSON(w, http.StatusOK, result)
	}
}

// buildDiagnoseResult maps a Plant.id /identification health_assessment
// response into the YardMate-facing DiagnoseResult.
//
// Healthy path ("Never healthy", SPEC §2.2): a healthy verdict is OVERRIDDEN —
// forceDiseaseOnHealthyVerdict force-picks the single most likely disease and
// sets IsHealthy=false, so the result never ships healthy with empty issues.
// Unhealthy path: top-3 disease suggestions from Plant.id; on each, attempt
// catalog id lookup (name-match, then LLM disambiguation). If Plant.id says
// unhealthy but returns zero suggestions, an AI layer picks the single most
// likely disease (candidate set narrows when plantId resolves), with the
// static common_diseases_list[0] → L08 chain as the graceful safety net.
func buildDiagnoseResult(ctx context.Context, api *plantIDDiagnoseResponse, content *ContentIndex, vision *VisionClient, enricher DiseaseEnricher, reqStart time.Time, lang string) *DiagnoseResult {
	res := &DiagnoseResult{Issues: []HealthIssue{}}

	if len(api.Result.Classification.Suggestions) > 0 {
		top := api.Result.Classification.Suggestions[0]
		cn := top.Details.CommonNames
		if cn == nil {
			cn = []string{}
		}
		res.Top = &PlantSuggestion{
			Name:           top.Name,
			ScientificName: top.Details.ScientificName,
			CommonNames:    cn,
			Confidence:     top.Probability,
		}
		res.IdentifiedName = top.Name
	}

	if res.IdentifiedName != "" {
		if id, ok := content.LookupPlantID(res.IdentifiedName); ok {
			pid := id
			res.PlantID = &pid
		}
	}

	res.HealthProbability = api.Result.IsHealthy.Probability
	res.IsHealthy = api.Result.IsHealthy.Binary

	if res.IsHealthy {
		// "Never healthy" (SPEC §2.2): diagnose must ALWAYS return a disease.
		// Override Plant.id's healthy verdict by force-picking the single most
		// likely disease for the identified plant and flipping IsHealthy=false.
		forceDiseaseOnHealthyVerdict(ctx, res, content, vision)
		return res
	}

	var enrichUsed bool
	for _, s := range api.Result.Disease.Suggestions {
		issue := HealthIssue{
			Name:        s.Name,
			Probability: s.Probability,
			Description: diagnoseDescriptionString(s.Details.Description),
			Cause:       s.Details.Cause,
			IsFallback:  false,
			Treatment: Treatment{
				Biological: nonNil(s.Details.Treatment.Biological),
				Chemical:   nonNil(s.Details.Treatment.Chemical),
				Prevention: nonNil(s.Details.Treatment.Prevention),
			},
		}
		issue.CatalogID = mapCatalogID(ctx, s.Name, content, vision)
		maybeEnrichIssue(ctx, &issue, res.IdentifiedName, enricher, reqStart, &enrichUsed, lang)
		res.Issues = append(res.Issues, issue)
		if len(res.Issues) >= 3 {
			break
		}
	}
	if len(res.Issues) > 0 {
		return res
	}

	// Plant.id says unhealthy but returned zero disease suggestions —
	// construct a fallback issue rather than ship an empty Issues array.
	res.Issues = []HealthIssue{buildFallbackIssue(ctx, res.PlantID, res.IdentifiedName, res.HealthProbability, content, vision)}
	return res
}

// diagnoseFallbackBudget returns how long the AI vision fallback may run: the
// wall clock remaining until the diagnoseWallClockBudget ceiling measured from
// reqStart (request entry ≈ when the server armed the 35 s WriteTimeout at
// header-read). Anchoring to reqStart — not to the post-body-read ctx — means a
// slow multipart upload eats INTO the budget instead of stacking on top of it,
// so the 200 fallback response can never overrun WriteTimeout. May be ≤ 0 (or
// below minDiagnoseFallbackBudget) when Plant.id consumed most of the clock; the
// caller then skips the OpenAI call for the instant static net. Mirrors roseBudget.
func diagnoseFallbackBudget(reqStart time.Time) time.Duration {
	return diagnoseWallClockBudget - time.Since(reqStart)
}

// diagnoseStaticNetResult is the last-resort DiagnoseResult when no vision
// diagnosis is available (the vision call failed, or too little wall clock
// remained to attempt it): an unhealthy result carrying the generic L08
// safety-net issue (200, not 502 — diagnose 不全废). With no plant/disease
// context, buildFallbackIssue skips its AI layer (plantName="") and makes NO
// OpenAI call, so this is instant and safe even on an already-expired ctx.
func diagnoseStaticNetResult(ctx context.Context, content *ContentIndex, vision *VisionClient) *DiagnoseResult {
	return &DiagnoseResult{
		IsHealthy: false,
		Issues:    []HealthIssue{buildFallbackIssue(ctx, nil, "", 0, content, vision)},
	}
}

// buildDiagnoseResultViaVision is the Plant.id-down fallback (SPEC §2.2): it
// runs the GPT-4o look-at-the-photo diagnosis and maps it into a DiagnoseResult.
// It NEVER returns nil — if DiagnosePlant itself fails (OpenAI down / timeout /
// refusal / malformed reply) it degrades to the generic L08 safety-net issue, a
// 200 result rather than a 502 (diagnose 不全废). The bool reports whether the AI
// diagnosis succeeded (true) or the safety net was used (false), for the
// handler's observability log.
func buildDiagnoseResultViaVision(ctx context.Context, image []byte, mime string, content *ContentIndex, vision *VisionClient, enricher DiseaseEnricher, reqStart time.Time, lang string) (*DiagnoseResult, bool) {
	vr, err := vision.DiagnosePlant(ctx, image, mime, lang)
	if err != nil {
		// Vision unavailable too — no plant/disease context to ground on, so
		// fall straight to the static net (same instant L08 the budget-skip path uses).
		log.Printf("diagnose vision fallback err: err=%v", err)
		return diagnoseStaticNetResult(ctx, content, vision), false
	}
	return diagnoseResultFromVision(ctx, vr, content, vision, enricher, reqStart, lang), true
}

// diagnoseResultFromVision maps a successful GPT-4o vision diagnosis into the
// client-facing DiagnoseResult — the SAME shape the Plant.id path produces, so
// iOS cannot tell the two apart (无声 fallback, SPEC §2.2). scientific_name
// drives identifiedName / top / plantId (via the shared LookupPlantID resolver);
// each issue name is mapped to a catalogId via the same name-match → LLM
// disambiguation chain the Plant.id path uses (mapCatalogID); top-3 cap. The
// per-issue isFallback=true marks AI-sourced issues for server/log distinction
// only — iOS does not branch on it (it already ships true on the unhealthy-empty
// path). On a healthy verdict the result is OVERRIDDEN by the same force-pick as
// the Plant.id path ("Never healthy", SPEC §2.2 — forceDiseaseOnHealthyVerdict
// sets IsHealthy=false with a forced issue); on unhealthy-but-no-usable-issue it
// falls to the same static safety net.
func diagnoseResultFromVision(ctx context.Context, vr *visionDiagnoseResult, content *ContentIndex, vision *VisionClient, enricher DiseaseEnricher, reqStart time.Time, lang string) *DiagnoseResult {
	res := &DiagnoseResult{Issues: []HealthIssue{}}

	name := strings.TrimSpace(vr.ScientificName)
	cn := vr.CommonNames
	if cn == nil {
		cn = []string{}
	}
	res.IdentifiedName = name
	res.Top = &PlantSuggestion{
		Name:           name,
		ScientificName: name,
		CommonNames:    cn,
		Confidence:     clamp01(vr.Confidence),
	}
	if name != "" {
		if id, ok := content.LookupPlantID(name); ok {
			pid := id
			res.PlantID = &pid
		}
	}

	res.HealthProbability = clamp01(vr.HealthProbability)
	res.IsHealthy = vr.IsHealthy
	if res.IsHealthy {
		// "Never healthy" (SPEC §2.2): the GPT-4o fallback also force-picks a
		// disease on a healthy verdict — identical to the Plant.id healthy path.
		forceDiseaseOnHealthyVerdict(ctx, res, content, vision)
		return res
	}

	var enrichUsed bool
	for _, iss := range vr.Issues {
		nm := strings.TrimSpace(iss.Name)
		if nm == "" {
			continue
		}
		issue := HealthIssue{
			Name:        nm,
			Probability: clamp01(iss.Confidence),
			Description: strings.TrimSpace(iss.Description),
			Cause:       strings.TrimSpace(iss.Cause),
			IsFallback:  true, // AI-sourced; server/log distinction only (silent to iOS)
			Treatment: Treatment{
				Biological: nonNil(iss.Treatment.Biological),
				Chemical:   nonNil(iss.Treatment.Chemical),
				Prevention: nonNil(iss.Treatment.Prevention),
			},
		}
		issue.CatalogID = mapCatalogID(ctx, nm, content, vision)
		maybeEnrichIssue(ctx, &issue, res.IdentifiedName, enricher, reqStart, &enrichUsed, lang)
		res.Issues = append(res.Issues, issue)
		if len(res.Issues) >= 3 {
			break
		}
	}
	if len(res.Issues) == 0 {
		// AI flagged unhealthy but gave no usable issue → static safety net,
		// grounded by the resolved plantId / name when available.
		res.Issues = []HealthIssue{buildFallbackIssue(ctx, res.PlantID, res.IdentifiedName, res.HealthProbability, content, vision)}
	}
	return res
}

// maybeEnrichIssue fills an out-of-catalog issue (CatalogID==nil) with generated
// structured detail via the disease enricher, mutating issue in place. It
// enriches at most the FIRST out-of-catalog issue per result (budget) and only
// when enough wall clock remains (budget from reqStart, like the vision
// fallback). Any failure leaves the issue slim — disease enrichment never 502s
// the diagnose (SPEC_disease §6). *used guards the one-shot per result.
func maybeEnrichIssue(ctx context.Context, issue *HealthIssue, plantName string, enricher DiseaseEnricher, reqStart time.Time, used *bool, lang string) {
	if enricher == nil || issue == nil || issue.CatalogID != nil || *used {
		return
	}
	budget := diagnoseFallbackBudget(reqStart)
	if budget < minDiagnoseFallbackBudget {
		return // too little clock left; leave the issue slim
	}
	*used = true
	ectx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	detail, catalogID, err := enricher.GetOrGenerate(ectx, issue.Name, plantName, lang)
	if err != nil || detail == nil {
		log.Printf("diagnose enrich skip: name=%q err=%v", issue.Name, err)
		return
	}
	issue.StructuredDetail = detail
	if catalogID != "" {
		id := catalogID
		issue.CatalogID = &id
	}
}

// mapCatalogID resolves an upstream disease name to a catalog id:
//  1. exact catalog-name match;
//  2. alias table — true synonyms / spelling variants of an existing disease;
//  3. LLM disambiguation against the description-enriched catalog (may NONE).
//
// Returns nil on a genuine miss. The out-of-catalog tail (empty/slim detail
// today) is handled separately by disease enrichment (SPEC_disease.md) — this
// function never force-maps an unrelated name onto a catalog entry. Steps 2+3's
// descriptions + narrowed alias table are the "mapping-recall boost": they map
// names that truly correspond to an existing entry, nothing more.
func mapCatalogID(ctx context.Context, name string, content *ContentIndex, vision *VisionClient) *string {
	if content == nil {
		return nil
	}
	// 1) Exact catalog-name match.
	if id, ok := content.LookupCatalogID(name); ok {
		return &id
	}
	// 2) Alias table — true synonyms / spelling variants only.
	if id, ok := content.LookupDiseaseAlias(name); ok {
		return &id
	}
	// 3) LLM disambiguation against the description-enriched catalog.
	if vision != nil {
		if refs := content.AllDiseaseNames(); len(refs) > 0 {
			if id, err := vision.DisambiguateDiseaseName(ctx, name, refs); err != nil {
				log.Printf("diagnose disambiguate err: name=%q err=%v", name, err)
			} else if id != "" {
				return &id
			}
		}
	}
	// Genuine miss → nil; out-of-catalog detail handled by disease enrichment.
	return nil
}

// fallbackIssueFrom builds the canonical isFallback=true HealthIssue from a
// catalog entry. The AI-suggested pick and the static [0]/L08 safety net
// both go through this, so the wire shape is byte-identical regardless of
// how the disease was chosen — the iOS client cannot tell them apart and
// the /v1/diagnose response contract is unchanged (SPEC §2.2).
func fallbackIssueFrom(d *DiseaseCatalog) HealthIssue {
	id := d.ID
	return HealthIssue{
		Name:        d.Name,
		CatalogID:   &id,
		Probability: 0,
		Description: d.ShortDescription,
		Cause:       "",
		IsFallback:  true,
		Treatment:   Treatment{Biological: []string{}, Chemical: []string{}, Prevention: []string{}},
	}
}

// forceDiseaseOnHealthyVerdict implements the SPEC §2.2 "Never healthy" product
// rule: /v1/diagnose must ALWAYS return a disease, never a "healthy" result.
// When EITHER the Plant.id health assessment (buildDiagnoseResult) OR the GPT-4o
// vision fallback (diagnoseResultFromVision) concludes the plant is healthy, the
// server force-picks the single most likely disease for the identified plant —
// the SAME AI-or-static buildFallbackIssue machinery the unhealthy-but-empty
// path uses — and stamps the result IsHealthy=false. The pick is grounded on the
// resolved plantId's curated common_diseases_list (else the full catalog, by
// name); a nil/keyless vision client or an unidentified plant degrades to the
// static L08 net, so res.Issues is NEVER empty. HealthProbability is left as the
// upstream "healthy" estimate (honest data; iOS routes on IsHealthy, and the
// disease page surfaces neither HealthProbability nor the plant name — so the
// still-populated Top / IdentifiedName are harmless). res must already carry
// IdentifiedName / PlantID / HealthProbability from the upstream mapping.
func forceDiseaseOnHealthyVerdict(ctx context.Context, res *DiagnoseResult, content *ContentIndex, vision *VisionClient) {
	// Distinct log prefix from buildFallbackIssue's "diagnose fallback ai" lines
	// so prod can measure how often a HEALTHY verdict is overridden (vs a
	// genuinely unhealthy-but-empty Plant.id result that also reaches buildFallbackIssue).
	log.Printf("diagnose force-pick on healthy verdict: plant=%q plantIdResolved=%v healthProb=%.2f",
		res.IdentifiedName, res.PlantID != nil, res.HealthProbability)
	res.Issues = []HealthIssue{buildFallbackIssue(ctx, res.PlantID, res.IdentifiedName, res.HealthProbability, content, vision)}
	res.IsHealthy = false
}

// buildFallbackIssue is the unhealthy-but-empty-suggestions tail (SPEC §2.2).
//
// An AI layer picks the single most likely disease, constrained to a
// candidate set that narrows when plantId resolves:
//   - plantId resolved → that plant's curated common_diseases_list
//     (plant-grounded; replaces the old mechanical [0] pick);
//   - plantId miss      → the full ~70-entry catalog, chosen by plant name.
//
// The static common_diseases_list[0] → L08 → hard-coded chain is the safety
// net below the AI layer: every case that worked before still works if
// vision is nil (no OPENAI key) / errors / times out / replies NONE /
// hallucinates an id. Output shape is identical either way
// (fallbackIssueFrom), so the client + contract never see the difference.
func buildFallbackIssue(ctx context.Context, plantID *string, plantName string, healthProb float64, content *ContentIndex, vision *VisionClient) HealthIssue {
	if content != nil && vision != nil && plantName != "" {
		var refs []DiseaseNameRef
		if plantID != nil {
			for _, id := range content.CommonDiseasesFor(*plantID) {
				if d, ok := content.DiseaseByID(id); ok && d != nil {
					refs = append(refs, DiseaseNameRef{ID: d.ID, Name: d.Name})
				}
			}
			if len(refs) == 0 {
				// Resolved plant with an EMPTY curated disease list (e.g. a newly
				// promoted catalog plant whose common_diseases_list isn't populated
				// yet): fall back to the full catalog like an unresolved plant, so
				// the AI still grounds a specific disease on the plant name. Without
				// this, a resolved-but-listless plant skips the AI layer and drops
				// straight to the generic L08 net — behaving WORSE than a plant that
				// never resolved at all.
				refs = content.AllDiseaseNames()
			}
		} else {
			refs = content.AllDiseaseNames()
		}
		if len(refs) > 0 {
			id, err := vision.SuggestCommonDisease(ctx, plantName, healthProb, refs)
			if err != nil {
				log.Printf("diagnose fallback ai err: plant=%q plantIdResolved=%v err=%v", plantName, plantID != nil, err)
			} else if id != "" {
				if d, ok := content.DiseaseByID(id); ok && d != nil {
					// Success log mirrors the "ai err" line so prod can measure
					// AI-fallback trigger rate + pick distribution (resolved vs
					// miss) on this rare path without a metrics backend.
					log.Printf("diagnose fallback ai ok: plant=%q plantIdResolved=%v catalogId=%s", plantName, plantID != nil, d.ID)
					return fallbackIssueFrom(d)
				}
			}
		}
	}

	// Safety net — unchanged from pre-AI behavior.
	if content != nil && plantID != nil {
		if list := content.CommonDiseasesFor(*plantID); len(list) > 0 {
			if d, ok := content.DiseaseByID(list[0]); ok && d != nil {
				return fallbackIssueFrom(d)
			}
		}
	}
	if content != nil {
		if d, ok := content.DiseaseByID("L08"); ok && d != nil {
			return fallbackIssueFrom(d)
		}
	}
	return HealthIssue{
		Name:        "Waterlogging",
		CatalogID:   nil,
		Probability: 0,
		IsFallback:  true,
		Treatment:   Treatment{Biological: []string{}, Chemical: []string{}, Prevention: []string{}},
	}
}

// nonNil swaps a nil []string for an empty slice so the JSON wire form is
// `[]` rather than `null`.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// clamp01 clamps a model-reported probability / confidence into [0,1]; gpt-4o
// occasionally returns a slightly out-of-range value. Used when mapping the AI
// diagnose fallback into DiagnoseResult.
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
