package enrichment

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// attributionRequestPayload is the wire shape of POST /v1/attribution. The only
// field is the opaque AdServices token the iOS app obtains from
// AAAttribution.attributionToken(); UserID is best-effort (empty when signed
// out) and lets install attribution be correlated with the account later.
type attributionRequestPayload struct {
	Token  string `json:"token"`
	UserID string `json:"userId"`
}

// attributionTokenMaxLen bounds the token before it hits Apple / the DB. Real
// AdServices tokens are a few hundred base64 chars; this is a generous ceiling
// (anything larger is malformed, rejected up front like the other size caps).
const attributionTokenMaxLen = 4096

// appleAdServicesURL is Apple's attribution-token exchange endpoint. The token
// is POSTed as a text/plain body; the response is the campaign/keyword record.
const appleAdServicesURL = "https://api-adservices.apple.com/api/v1/"

// errAttributionPending is the sentinel for Apple's HTTP 404 — a valid token
// whose attribution record is not ready yet. The client is told to retry later
// with a fresh token; nothing is stored, so the server stays stateless.
var errAttributionPending = errors.New("attribution pending")

// appleAttributionResponse is Apple's 200 body. When Attribution is false the
// install is confirmed-organic (valid token, no campaign) and the numeric /
// string campaign fields are absent — stored as NULL (RecordAttribution).
type appleAttributionResponse struct {
	Attribution     bool   `json:"attribution"`
	OrgID           int64  `json:"orgId"`
	CampaignID      int64  `json:"campaignId"`
	ConversionType  string `json:"conversionType"`
	AdGroupID       int64  `json:"adGroupId"`
	CountryOrRegion string `json:"countryOrRegion"`
	KeywordID       int64  `json:"keywordId"`
	AdID            int64  `json:"adId"`
	ClickDate       string `json:"clickDate"`
}

// AppleAdServicesClient exchanges an AdServices token at Apple's attribution
// endpoint. baseURL is overridable so tests can point at an httptest stub.
type AppleAdServicesClient struct {
	httpClient *http.Client
	baseURL    string
}

// NewAppleAdServicesClient builds a client against the real Apple endpoint with
// a 10 s timeout (the exchange is a single small round-trip).
func NewAppleAdServicesClient() *AppleAdServicesClient {
	return &AppleAdServicesClient{
		httpClient: &http.Client{Timeout: 10 * time.Second},
		baseURL:    appleAdServicesURL,
	}
}

// Fetch POSTs the token to Apple and decodes the record. Returns
// errAttributionPending on 404 (record not ready), a wrapped error on any other
// non-200, and the decoded record on 200.
func (c *AppleAdServicesClient) Fetch(ctx context.Context, token string) (*appleAttributionResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL, strings.NewReader(token))
	if err != nil {
		return nil, fmt.Errorf("build apple attribution request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("apple attribution request: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var out appleAttributionResponse
		if err := json.NewDecoder(io.LimitReader(resp.Body, requestBodyCap)).Decode(&out); err != nil {
			return nil, fmt.Errorf("decode apple attribution: %w", err)
		}
		return &out, nil
	case http.StatusNotFound:
		return nil, errAttributionPending
	default:
		return nil, fmt.Errorf("apple attribution status %d", resp.StatusCode)
	}
}

// HandleAttribution returns the handler for POST /v1/attribution — Apple Search
// Ads (AdServices) install attribution. The iOS app posts an AAAttribution
// token; the server exchanges it at Apple's api-adservices endpoint and stores
// the campaign/keyword breakdown, deduped per device install id (first write
// wins). No IDFA, no ATT prompt — AdServices is anonymous.
//
// Reuses the shared Supabase pool (enrichDB); no attestation (log-only parity
// with /v1/plants/signal). Registered outside the per-device expensive-call
// group — abuse is bounded by the /v1 per-IP limit, and a forged token simply
// 404s at Apple (nothing stored).
//
// Response contract: {"status":"stored"} on success, {"status":"pending"} when
// Apple has no record yet (client retries later with a fresh token).
func HandleAttribution(db *DB, apple *AppleAdServicesClient) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, requestBodyCap)

		// Required headers (parity with /v1/plants/signal). The install id
		// doubles as the dedup key — no separate identifier is sent.
		deviceID := r.Header.Get("X-Device-Install-Id")
		if !isUUID(deviceID) {
			writeError(w, http.StatusBadRequest, "missing_device_id")
			return
		}
		if r.Header.Get("X-App-Version") == "" {
			writeError(w, http.StatusBadRequest, "missing_app_version")
			return
		}

		var req attributionRequestPayload
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, "bad_json")
			return
		}
		token := strings.TrimSpace(req.Token)
		if token == "" || len(token) > attributionTokenMaxLen {
			writeError(w, http.StatusBadRequest, "invalid_token")
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		defer cancel()

		rec, err := apple.Fetch(ctx, token)
		if errors.Is(err, errAttributionPending) {
			// No record yet — the client retries later with a fresh token.
			writeJSON(w, http.StatusOK, map[string]string{"status": "pending"})
			return
		}
		if err != nil {
			log.Printf("attribution apple fetch err: deviceID=%s err=%v", deviceID, err)
			writeError(w, http.StatusBadGateway, "attribution_upstream")
			return
		}

		if err := db.RecordAttribution(ctx, deviceID, req.UserID, rec); err != nil {
			log.Printf("attribution db err: deviceID=%s err=%v", deviceID, err)
			writeError(w, http.StatusBadGateway, "db_unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "stored"})
	}
}

// RecordAttribution inserts one attribution row keyed by device install id
// (first write wins — install attribution is immutable, so re-reports are a
// no-op). Campaign columns are NULL for organic installs (Attribution false).
// Nil-safe (ErrDBUnavailable when the shared pool is absent).
func (d *DB) RecordAttribution(ctx context.Context, deviceID, userID string, rec *appleAttributionResponse) error {
	if d == nil || d.pool == nil {
		return ErrDBUnavailable
	}

	// Organic installs carry no campaign — keep those columns NULL rather than
	// storing 0, which would read as "campaign 0".
	var (
		orgID, campaignID, adGroupID, adID, keywordID *int64
		conversionType, country, clickDate            *string
	)
	if rec.Attribution {
		orgID = nzInt64(rec.OrgID)
		campaignID = nzInt64(rec.CampaignID)
		adGroupID = nzInt64(rec.AdGroupID)
		adID = nzInt64(rec.AdID)
		keywordID = nzInt64(rec.KeywordID)
		conversionType = nzStr(rec.ConversionType)
		country = nzStr(rec.CountryOrRegion)
		clickDate = nzStr(rec.ClickDate)
	}
	userIDPtr := nzStr(userID)

	const stmt = `
		INSERT INTO ad_attribution
		  (device_id, user_id, attribution, org_id, campaign_id, ad_group_id,
		   ad_id, keyword_id, conversion_type, country_or_region, click_date)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (device_id) DO NOTHING`
	if _, err := d.pool.Exec(ctx, stmt,
		deviceID, userIDPtr, rec.Attribution, orgID, campaignID, adGroupID,
		adID, keywordID, conversionType, country, clickDate); err != nil {
		return fmt.Errorf("%w: attribution upsert: %v", ErrDBUnavailable, err)
	}
	return nil
}

// nzInt64 / nzStr map a zero value to nil so the driver writes SQL NULL.
func nzInt64(v int64) *int64 {
	if v == 0 {
		return nil
	}
	return &v
}

func nzStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}
