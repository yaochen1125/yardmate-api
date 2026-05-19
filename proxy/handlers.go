package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

// Body cap for POST /v1/identify: 8 MB image + 1 MB multipart overhead headroom.
// Enforced by http.MaxBytesReader at handler entry per SPEC §4.2.
const identifyMaxBody = 9 << 20 // 9 MiB

// Hard timeout for the upstream Plant.id call (per SPEC §5.2). The chi
// middleware.Timeout (10 s) is overridden via a per-request derived context
// so Plant.id has up to 30 s — the proxy is the slow-path tenant.
const identifyUpstreamTimeout = 30 * time.Second

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
// vision is optional. When non-nil AND the request sets ai_enhance=true, the
// handler asks OpenAI to rerank the top-N candidates against the uploaded
// image and re-orders Suggestions so the LLM pick is first. On any LLM
// error / timeout the original engine ranking is preserved and AIEnhancedAt
// stays null in the response.
func HandleIdentify(plantNet *PlantNetClient, plantID *PlantIDClient, content *ContentIndex, vision *VisionClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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
			imgBytes  []byte
			aiEnhance bool
			organ     = "auto"
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
			case "ai_enhance":
				b, err := io.ReadAll(io.LimitReader(part, 16))
				if err == nil {
					switch strings.TrimSpace(string(b)) {
					case "true", "1", "yes":
						aiEnhance = true
					}
				}
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

		ctx, cancel := context.WithTimeout(r.Context(), identifyUpstreamTimeout)
		defer cancel()

		// --- Two-engine cascade (SPEC §1.1 / §7). Single attempt per engine,
		//     no per-engine retry. Pl@ntNet primary → Plant.id fallback. ---
		// `err` is already declared in this scope (from r.MultipartReader);
		// reuse it via plain assignment so it's not redeclared.
		var (
			result *IdentifyResult
			engine string
		)
		err = nil

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
			result, err = plantID.Identify(ctx, bytes.NewReader(imgBytes), mime)
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

		// 7. Optional AI rerank. Failures here do not affect the 200 response
		//    contract — AIEnhancedAt simply stays null. The vision call uses
		//    the same ctx but its client has an inner 8 s timeout (see SPEC §2.1).
		if aiEnhance && vision != nil && len(result.Suggestions) > 0 {
			pick, verr := vision.RerankIdentify(ctx, imgBytes, mime, result.Suggestions)
			if verr != nil {
				log.Printf("identify ai_enhance failed: deviceID=%s err=%v", deviceID, verr)
			} else {
				// Move the picked candidate to index 0 if it isn't already.
				for i, s := range result.Suggestions {
					if s.Name == pick {
						if i != 0 {
							result.Suggestions[0], result.Suggestions[i] =
								result.Suggestions[i], result.Suggestions[0]
						}
						break
					}
				}
				ts := time.Now().UTC().Format(time.RFC3339)
				result.AIEnhancedAt = &ts
			}
		}

		// 7b. Resolve YardMate plantId per suggestion from scientific_name
		//     (SPEC §2.1 "plant_id mapping"). Same resolver /v1/diagnose uses
		//     at the handler layer; content/LookupPlantID are nil-safe. Done
		//     after the optional rerank but order-independent (per-suggestion).
		plantIDsResolved := 0
		for i := range result.Suggestions {
			if id, ok := content.LookupPlantID(result.Suggestions[i].ScientificName); ok {
				pid := id
				result.Suggestions[i].PlantID = &pid
				plantIDsResolved++
			}
		}

		// 8. Success — single-line structured log (SPEC §5.2 forensics).
		log.Printf("identify ok: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v engine=%s mime=%s isPlant=%v suggestions=%d plantIdsResolved=%d aiEnhanced=%v",
			deviceID, appVer, attKeyID, attAssertPresent, engine, mime, result.IsPlant, len(result.Suggestions), plantIDsResolved, result.AIEnhancedAt != nil)
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

// diagnoseUpstreamTimeout caps the Plant.id call. The handler context is
// further bounded by the chi RequestID + Logger middleware; vision
// disambiguation runs inside the same context but has its own client
// timeout (≤8 s) inside VisionClient.
const diagnoseUpstreamTimeout = 30 * time.Second

// HandleDiagnose returns the http.HandlerFunc for POST /v1/diagnose.
// Combines Plant.id v3 health_assessment with YardMate catalog lookups
// (content) and an optional LLM disambiguation pass (vision). See SPEC §2.2.
//
// content / vision may be nil — both are graceful no-ops (plantId stays
// null, catalogId falls back to name-match only, generic Leaf-spot tail).
func HandleDiagnose(client *PlantIDClient, content *ContentIndex, vision *VisionClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
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

		var imagePart *multipart.Part
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
			if part.FormName() == "image" {
				imagePart = part
				break
			}
			_ = part.Close()
		}
		if imagePart == nil {
			writeError(w, http.StatusBadRequest, "missing_image")
			return
		}
		defer imagePart.Close()

		// Read full image bytes — Diagnose has to base64-encode the body
		// upstream, so we buffer once here (bounded by the 9 MB cap above).
		imgBytes, rerr := io.ReadAll(imagePart)
		if rerr != nil {
			if isMaxBytesErr(rerr) {
				writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
				return
			}
			writeError(w, http.StatusBadRequest, "bad_image")
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

		ctx, cancel := context.WithTimeout(r.Context(), diagnoseUpstreamTimeout)
		defer cancel()

		api, err := client.Diagnose(ctx, imgBytes, mime)
		if err != nil {
			log.Printf("diagnose upstream err: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v err=%v",
				deviceID, appVer, attKeyID, attAssertPresent, err)
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

		result := buildDiagnoseResult(ctx, api, content, vision)
		log.Printf("diagnose ok: deviceID=%s appVer=%s isHealthy=%v issues=%d plantIdResolved=%v",
			deviceID, appVer, result.IsHealthy, len(result.Issues), result.PlantID != nil)
		writeJSON(w, http.StatusOK, result)
	}
}

// buildDiagnoseResult maps a Plant.id /identification health_assessment
// response into the YardMate-facing DiagnoseResult.
//
// Healthy path: issues=[] + Top + plantId populated.
// Unhealthy path: top-3 disease suggestions from Plant.id; on each, attempt
// catalog id lookup (name-match, then LLM disambiguation). If Plant.id says
// unhealthy but returns zero suggestions, an AI layer picks the single most
// likely disease (candidate set narrows when plantId resolves), with the
// static common_diseases_list[0] → L06 chain as the graceful safety net.
func buildDiagnoseResult(ctx context.Context, api *plantIDDiagnoseResponse, content *ContentIndex, vision *VisionClient) *DiagnoseResult {
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
		// Healthy path — iOS shows the plant detail with a "this plant is
		// healthy" toast; no disease card. F-option-2 (诚实 fallback).
		return res
	}

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

// mapCatalogID resolves a Plant.id disease name to a YardMate catalog id.
// Order: exact/fuzzy name match → LLM disambiguation → nil.
func mapCatalogID(ctx context.Context, name string, content *ContentIndex, vision *VisionClient) *string {
	if id, ok := content.LookupCatalogID(name); ok {
		s := id
		return &s
	}
	if vision == nil || content == nil {
		return nil
	}
	refs := content.AllDiseaseNames()
	if len(refs) == 0 {
		return nil
	}
	id, err := vision.DisambiguateDiseaseName(ctx, name, refs)
	if err != nil {
		log.Printf("diagnose disambiguate err: name=%q err=%v", name, err)
		return nil
	}
	if id == "" {
		return nil
	}
	return &id
}

// fallbackIssueFrom builds the canonical isFallback=true HealthIssue from a
// catalog entry. The AI-suggested pick and the static [0]/L06 safety net
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

// buildFallbackIssue is the unhealthy-but-empty-suggestions tail (SPEC §2.2).
//
// An AI layer picks the single most likely disease, constrained to a
// candidate set that narrows when plantId resolves:
//   - plantId resolved → that plant's curated common_diseases_list
//     (plant-grounded; replaces the old mechanical [0] pick);
//   - plantId miss      → the full ~70-entry catalog, chosen by plant name.
//
// The static common_diseases_list[0] → L06 → hard-coded chain is the safety
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
		if d, ok := content.DiseaseByID("L06"); ok && d != nil {
			return fallbackIssueFrom(d)
		}
	}
	return HealthIssue{
		Name:        "Leaf spot",
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
