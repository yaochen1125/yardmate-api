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
// See SPEC §2.1, §3 for the contract.
//
// V1 NOTES (per SPEC):
//   - per-IP rate limit is applied by ratelimit.PerIPMiddleware at the /v1
//     scope (server.go); this handler does not call it directly.
//   - per-deviceInstallId rate limit is a TODO V1.1 (SPEC §4.1) — we log
//     the device ID + assertion presence today; no enforcement yet.
//   - App Attest assertion headers are read + logged for forensics. V1 does
//     NOT call attest.VerifyAssertion (iOS 26 issue, memory option_d_progress.md).
func HandleIdentify(client *PlantIDClient) http.HandlerFunc {
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

		// 5. Locate the "image" form field.
		var (
			imagePart *multipart.Part
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

		// 6. MIME byte-sniff first 512 bytes (SPEC §6 pitfall 8). The
		//    multipart Content-Type header from the client is untrusted.
		first := make([]byte, 512)
		n, rerr := io.ReadFull(imagePart, first)
		if rerr != nil && !errors.Is(rerr, io.EOF) && !errors.Is(rerr, io.ErrUnexpectedEOF) {
			if isMaxBytesErr(rerr) {
				writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
				return
			}
			writeError(w, http.StatusBadRequest, "bad_image")
			return
		}
		first = first[:n]
		mime := http.DetectContentType(first)
		if mime != "image/jpeg" && mime != "image/png" {
			writeError(w, http.StatusBadRequest, "bad_image")
			return
		}

		// 7. Stream-and-call Plant.id. The 512-byte sniff prefix is concatenated
		//    back via io.MultiReader so we don't lose data.
		body := io.MultiReader(bytes.NewReader(first), imagePart)
		ctx, cancel := context.WithTimeout(r.Context(), identifyUpstreamTimeout)
		defer cancel()

		result, err := client.Identify(ctx, body, mime)
		if err != nil {
			// Detect over-cap error during streaming to upstream.
			if isMaxBytesErr(err) {
				writeError(w, http.StatusRequestEntityTooLarge, "image_too_large")
				return
			}
			log.Printf("identify upstream err: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v err=%v",
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

		// 8. Success — single-line structured log (SPEC §5.2 forensics).
		log.Printf("identify ok: deviceID=%s appVer=%s attKeyID=%q assertPresent=%v mime=%s isPlant=%v suggestions=%d",
			deviceID, appVer, attKeyID, attAssertPresent, mime, result.IsPlant, len(result.Suggestions))
		writeJSON(w, http.StatusOK, result)
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
