package training

import (
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// trainingMaxBody caps the request body — same as identify (8 MB image + ~1 MB
// overhead). Overflow surfaces as *http.MaxBytesError → image_too_large.
const trainingMaxBody = 9 << 20

// Per-field read caps (bytes). Kept tight; these are short metadata strings.
const (
	capLabel   = 16
	capPlantID = 32
	capSciName = 128
	capEngine  = 32
	capConf    = 16
	capConsent = 32
	capUserID  = 64
)

// HandleUpload accepts one opt-in training photo (multipart) + its identify
// label metadata, strips JPEG metadata, and stores it (SPEC §2). Registered only
// when TRAINING_UPLOAD_ENABLED and inside the per-device + inflight groups.
func HandleUpload(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, trainingMaxBody)

		deviceID := r.Header.Get("X-Device-Install-Id")
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		if r.Header.Get("X-App-Version") == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}
		if !strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
			writeError(w, http.StatusBadRequest, "bad_multipart")
			return
		}
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_multipart")
			return
		}

		meta := PhotoMeta{InstallID: deviceID, AppVersion: r.Header.Get("X-App-Version")}
		var imgBytes []byte
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
			case "label_kind":
				meta.LabelKind = readField(part, capLabel)
			case "plant_id":
				meta.PlantID = readField(part, capPlantID)
			case "scientific_name":
				meta.ScientificName = readField(part, capSciName)
			case "source_engine":
				meta.SourceEngine = readField(part, capEngine)
			case "top_confidence":
				if f, ok := parseFloat(readField(part, capConf)); ok {
					meta.TopConfidence = &f
				}
			case "consent_version":
				meta.ConsentVersion = readField(part, capConsent)
			case "user_id":
				// Soft attribution only (SPEC §5). Stored solely to drive the
				// account-deletion cascade; validated as a UUID, dropped otherwise.
				if v := readField(part, capUserID); isUUID(v) {
					meta.UserID = v
				}
			}
			_ = part.Close()
		}

		if len(imgBytes) == 0 {
			writeError(w, http.StatusBadRequest, "missing_image")
			return
		}
		if meta.ConsentVersion == "" {
			// Proof-of-consent is required — we don't retain a photo we can't tie
			// to a consent version the user agreed to.
			writeError(w, http.StatusBadRequest, "missing_consent_version")
			return
		}
		// MIME sniff (untrusted client Content-Type). JPEG only for training.
		head := imgBytes
		if len(head) > 512 {
			head = head[:512]
		}
		if http.DetectContentType(head) != "image/jpeg" {
			writeError(w, http.StatusBadRequest, "bad_image")
			return
		}
		// Strip metadata (EXIF/GPS/XMP). A parse failure means it wasn't a usable
		// JPEG → bad_image (doubles as validation).
		stripped, err := stripJPEGMetadata(imgBytes)
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_image")
			return
		}

		id, err := store.Insert(r.Context(), meta, stripped)
		if err != nil {
			log.Printf("training/photo: store insert failed: %v", err)
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "stored": true})
	}
}

// HandleDelete removes every training photo uploaded by the calling device
// (X-Device-Install-Id). Backs the iOS "delete my uploaded photos" action + the
// opt-out flow. Needs no Bearer — the keychain install id is the device key
// (SPEC §5). The account-deletion cascade (by user id) runs separately inside
// the Bearer-verified /v1/account/delete.
func HandleDelete(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		deviceID := r.Header.Get("X-Device-Install-Id")
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		n, err := store.DeleteByInstallID(r.Context(), deviceID)
		if err != nil {
			log.Printf("training/delete: delete by install id failed: %v", err)
			writeError(w, http.StatusInternalServerError, "server_error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
	}
}

// --- package-local helpers (deliberately not shared with the main/proxy
// packages, mirroring imageingest's self-contained helpers) ---

func readField(part io.Reader, limit int64) string {
	b, err := io.ReadAll(io.LimitReader(part, limit))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func parseFloat(s string) (float64, bool) {
	if s == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}

func isMaxBytesErr(err error) bool {
	var mbe *http.MaxBytesError
	return errors.As(err, &mbe)
}

// isUUID reports whether s is a canonical 8-4-4-4-12 lowercase-or-mixed hex
// UUID. Mirrors ratelimit.isUUID (kept local to avoid a cross-package import).
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
			continue
		}
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
