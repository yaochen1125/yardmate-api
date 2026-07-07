package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// feedbackRequestPayload is the wire shape of POST /v1/feedback. Field names
// match the iOS FeedbackService encoder (camelCase). The device / system /
// appLanguage / region strings are display values captured client-side
// (marketing name + identifier, "iOS 26.0", BCP-47 app language, region code)
// purely for triage context — never parsed, never trusted beyond truncation.
type feedbackRequestPayload struct {
	Message     string `json:"message"`
	Device      string `json:"device"`
	System      string `json:"system"`
	AppLanguage string `json:"appLanguage"`
	Region      string `json:"region"`
}

const (
	// feedbackMessageMaxRunes mirrors the iOS composer's hard cap and the
	// feedback.message DB CHECK (migration 008).
	feedbackMessageMaxRunes = 1000
	// feedbackMetaMaxRunes bounds the triage-context strings; anything longer
	// is a hand-crafted request, silently truncated.
	feedbackMetaMaxRunes = 120
	// feedbackDailyCapPerDevice is the per-device rolling-24h insert cap,
	// enforced inside RecordFeedback's insert statement.
	feedbackDailyCapPerDevice = 10
)

// HandleFeedback returns the handler for POST /v1/feedback — the in-app
// "Send feedback" composer (iOS: More → SUPPORT). Anonymous one-way messages:
// the only identifier is the device install id, used solely as the rate-cap
// key. Reuses the shared Supabase pool (enrichDB); no attestation (log-only
// parity with /v1/plants/signal). Registered outside the per-device
// expensive-call group — abuse is bounded by the /v1 per-IP limit plus the
// per-device daily cap in SQL.
//
// mailer (nil = disabled) forwards each stored message to the operator inbox
// asynchronously — mail failures never affect the client response.
func HandleFeedback(db *DB, mailer *FeedbackMailer) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, requestBodyCap)

		// Required headers (parity with /v1/plants/signal).
		deviceID := r.Header.Get("X-Device-Install-Id")
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		appVersion := r.Header.Get("X-App-Version")
		if appVersion == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}

		var req feedbackRequestPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json")
			return
		}

		message := strings.TrimSpace(req.Message)
		if message == "" {
			writeError(w, http.StatusBadRequest, "missing_message")
			return
		}
		if utf8.RuneCountInString(message) > feedbackMessageMaxRunes {
			writeError(w, http.StatusBadRequest, "message_too_long")
			return
		}

		row := feedbackRow{
			deviceID:    deviceID,
			appVersion:  truncateRunes(appVersion, feedbackMetaMaxRunes),
			message:     message,
			device:      truncateRunes(strings.TrimSpace(req.Device), feedbackMetaMaxRunes),
			system:      truncateRunes(strings.TrimSpace(req.System), feedbackMetaMaxRunes),
			appLanguage: truncateRunes(strings.TrimSpace(req.AppLanguage), feedbackMetaMaxRunes),
			region:      truncateRunes(strings.TrimSpace(req.Region), feedbackMetaMaxRunes),
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		id, inserted, err := db.RecordFeedback(ctx, row)
		if err != nil {
			writeError(w, http.StatusBadGateway, "db_unavailable")
			log.Printf("feedback err: deviceID=%s err=%v", deviceID, err)
			return
		}
		if !inserted {
			writeError(w, http.StatusTooManyRequests, "rate_limit_feedback")
			return
		}
		if mailer != nil {
			go mailer.notify(id, row)
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "received"})
	}
}

// feedbackRow carries one validated, truncated feedback insert.
type feedbackRow struct {
	deviceID    string
	appVersion  string
	message     string
	device      string
	system      string
	appLanguage string
	region      string
}

// RecordFeedback inserts one feedback row unless the device already submitted
// feedbackDailyCapPerDevice messages in the last 24 hours. The cap check and
// insert run as a single statement (CTE), so concurrent submissions cannot
// meaningfully overshoot. Returns the new row id, or ("", false, nil) when
// the cap suppressed the insert. Nil-safe (returns ErrDBUnavailable when the
// shared pool is absent, matching the enrichment DB contract).
func (d *DB) RecordFeedback(ctx context.Context, row feedbackRow) (string, bool, error) {
	if d == nil || d.pool == nil {
		return "", false, ErrDBUnavailable
	}
	const stmt = `
		WITH recent AS (
			SELECT count(*) AS n FROM feedback
			WHERE device_id = $1 AND created_at > now() - interval '24 hours'
		)
		INSERT INTO feedback (device_id, app_version, message, device, system, app_language, region)
		SELECT $1, $2, $3, $4, $5, $6, $7 FROM recent WHERE n < $8
		RETURNING id`
	var id string
	err := d.pool.QueryRow(ctx, stmt,
		row.deviceID, row.appVersion, row.message,
		row.device, row.system, row.appLanguage, row.region,
		feedbackDailyCapPerDevice,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("%w: feedback insert: %v", ErrDBUnavailable, err)
	}
	return id, true, nil
}

// truncateRunes hard-caps s at max runes (not bytes), preserving valid UTF-8.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	runes := []rune(s)
	return string(runes[:max])
}
