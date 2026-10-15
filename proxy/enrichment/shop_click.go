package enrichment

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"time"
)

// shopClickPayload is the wire shape of POST /v1/shop/click — one product-card
// click in the Shop tab (Amazon affiliate storefront). Contract: yardmate-swiftui
// docs/releases/v1/main-navigation/shop/shop.md ## 数据契约「商品点击上报」.
type shopClickPayload struct {
	ItemID  string `json:"itemId"`  // shop.json item id, e.g. "seeds-lavender"
	Screen  string `json:"screen"`  // "shop" | "shop_category" | "shop_collection"
	Context string `json:"context"` // category / collection id the card sat in; may be empty
	Locale  string `json:"locale"`  // app UI language code; may be empty
}

// shopClickScreens is the allowed set of screens. Kept in sync with the
// shop_clicks.screen CHECK constraint (migration 013).
var shopClickScreens = map[string]bool{"shop": true, "shop_category": true, "shop_collection": true}

// shopClickID matches shop.json ids (kebab-case; we also tolerate dots and
// underscores). Bounded to 64 chars to match the column CHECKs.
var shopClickID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// shopClickMaxLocaleLen matches the shop_clicks.locale CHECK (migration 013).
const shopClickMaxLocaleLen = 16

// shopClickDailyCapPerDevice bounds how many clicks one device install id can
// log per rolling 24h. Normal browsing is a few dozen at most; the cap only
// stops a scripted client from flooding the admin stats. Enforced inside the
// INSERT (count + insert in one statement); a slight overshoot under
// concurrency is harmless for a stats table, so no advisory lock.
const shopClickDailyCapPerDevice = 300

// HandleShopClick returns the handler for POST /v1/shop/click — a cheap,
// fire-and-forget click log. Same posture as /v1/plants/signal: required
// X-Device-Install-Id (also the rate-cap key) + X-App-Version, no attestation,
// registered outside the per-device expensive-call bucket. iOS only calls this
// when the Amazon link actually opened (US storefront) and never in EEA/UK.
func HandleShopClick(db *DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, requestBodyCap)

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

		var req shopClickPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json")
			return
		}
		if !shopClickID.MatchString(req.ItemID) {
			writeError(w, http.StatusBadRequest, "invalid_item_id")
			return
		}
		if !shopClickScreens[req.Screen] {
			writeError(w, http.StatusBadRequest, "invalid_screen")
			return
		}
		if req.Context != "" && !shopClickID.MatchString(req.Context) {
			writeError(w, http.StatusBadRequest, "invalid_context")
			return
		}
		if len(req.Locale) > shopClickMaxLocaleLen {
			writeError(w, http.StatusBadRequest, "invalid_locale")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		if err := db.RecordShopClick(ctx, deviceID, appVersion, req); err != nil {
			writeError(w, http.StatusBadGateway, "db_unavailable")
			log.Printf("shop click err: deviceID=%s item=%q screen=%q err=%v",
				deviceID, req.ItemID, req.Screen, err)
			return
		}
		// Capped clicks are also "recorded" from the client's point of view —
		// it never reads the response, and leaking the cap teaches nothing useful.
		writeJSON(w, http.StatusOK, map[string]string{"status": "recorded"})
	}
}

// RecordShopClick appends one click row unless the device already logged
// shopClickDailyCapPerDevice clicks in the last 24h (then it silently drops the
// row). Nil-safe (ErrDBUnavailable when the shared pool is absent).
func (d *DB) RecordShopClick(ctx context.Context, deviceID, appVersion string, c shopClickPayload) error {
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}
	const stmt = `
		INSERT INTO shop_clicks (device_id, app_version, item_id, screen, context_id, locale)
		SELECT $1, $2, $3, $4, $5, $6
		WHERE (SELECT count(*) FROM shop_clicks
		       WHERE device_id = $1 AND created_at > now() - interval '24 hours') < $7`
	if _, err := d.pool.Exec(ctx, stmt,
		deviceID, appVersion, c.ItemID, c.Screen, c.Context, c.Locale, shopClickDailyCapPerDevice); err != nil {
		return fmt.Errorf("%w: shop click insert: %v", ErrDBUnavailable, err)
	}
	return nil
}
