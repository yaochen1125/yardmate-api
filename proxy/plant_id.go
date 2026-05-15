// Package proxy implements server-side proxies for Plant.id (image
// identification) and OpenAI (AI care advice). See proxy/SPEC.md for the
// full design contract. V1 replaces D-Server "key vending" (kept compiled
// but unused by V1 clients) — see memory option_d_progress.md.
package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"time"
)

// defaultPlantIDEndpoint is the Plant.id v3 identification URL with the
// query params V1 needs (common_names + scientific_name in English).
const defaultPlantIDEndpoint = "https://plant.id/api/v3/identification?details=common_names,scientific_name&language=en"

// defaultPlantIDDiagnoseEndpoint is the Plant.id v3 identification URL used
// for diagnose calls — same endpoint with health-assessment details and the
// `health=all` flag passed in the JSON body (SPEC §2.2).
const defaultPlantIDDiagnoseEndpoint = "https://plant.id/api/v3/identification?details=local_name,description,treatment,cause&language=en"

// defaultPlantIDTimeout caps the upstream Plant.id call. 30 s matches SPEC §5.2.
const defaultPlantIDTimeout = 30 * time.Second

// PlantIDClient calls Plant.id v3 for identification (Identify) and combined
// identification + health assessment (Diagnose). Construct with
// NewPlantIDClient. Endpoint, DiagnoseEndpoint and HTTP client are exported
// for tests.
type PlantIDClient struct {
	APIKey           string
	Endpoint         string
	DiagnoseEndpoint string
	HTTP             *http.Client
}

// NewPlantIDClient returns a PlantIDClient with production defaults.
// apiKey comes from secrets.Vault (env PLANT_ID_API_KEY) at startup —
// never exposed to clients.
func NewPlantIDClient(apiKey string) *PlantIDClient {
	return &PlantIDClient{
		APIKey:           apiKey,
		Endpoint:         defaultPlantIDEndpoint,
		DiagnoseEndpoint: defaultPlantIDDiagnoseEndpoint,
		HTTP:             &http.Client{Timeout: defaultPlantIDTimeout},
	}
}

// Identify uploads the image to Plant.id v3 and returns the sanitized
// IdentifyResult. mime must be "image/jpeg" or "image/png" (caller validates).
//
// V1 buffers the image into a bytes.Buffer for the upstream request body;
// SPEC §6 pitfall 4 calls out io.Pipe streaming as a V1.1 optimization.
// At V1 scale (8 MB image cap × per-IP rate limit), the memory cost is
// bounded and acceptable.
func (c *PlantIDClient) Identify(ctx context.Context, image io.Reader, mime string) (*IdentifyResult, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Plant.id v3 multipart field name is "images" (plural), SPEC §6 pitfall 1.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="images"; filename="image"`)
	h.Set("Content-Type", mime)
	part, err := writer.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("plant_id: build multipart: %w", err)
	}
	if _, err := io.Copy(part, image); err != nil {
		return nil, fmt.Errorf("plant_id: copy image: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("plant_id: close multipart: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, &body)
	if err != nil {
		return nil, fmt.Errorf("plant_id: build request: %w", err)
	}
	req.Header.Set("Api-Key", c.APIKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Network error, timeout, or context cancel — all treated as transient.
		return nil, fmt.Errorf("%w: %v", ErrPlantIDUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		// Plant.id v3 returns 201 Created on successful identification (each
		// call creates an identification resource with an access_token). 200
		// also accepted for forward-compat / mock servers.
		var apiResp plantIDAPIResponse
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return nil, fmt.Errorf("%w: decode: %v", ErrPlantIDBadResponse, err)
		}
		return apiResp.toIdentifyResult(), nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, ErrPlantIDUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrPlantIDRateLimit
	case resp.StatusCode == http.StatusBadRequest:
		// Plant.id 400 typically: "Unsupported file format" / "Not an image".
		return nil, ErrPlantIDImageRejected
	case resp.StatusCode >= 500:
		return nil, ErrPlantIDUnavailable
	default:
		return nil, fmt.Errorf("%w: status %d", ErrPlantIDBadResponse, resp.StatusCode)
	}
}

// Diagnose calls Plant.id v3 with `health=all`, returning both the plant
// identification (classification.suggestions) and the health assessment
// (disease.suggestions). image is the raw bytes (≤8 MB cap enforced by the
// handler); mime must be "image/jpeg" or "image/png" (caller validates).
//
// Plant.id v3 accepts both multipart and JSON-body uploads. We use JSON +
// base64 here (instead of multipart like Identify) because the contract
// requires `images: [...]` as a JSON array plus the sibling `health: "all"`
// flag — multipart can't express the named flag cleanly. The trade-off is
// ~33% extra payload size from base64 encoding.
func (c *PlantIDClient) Diagnose(ctx context.Context, image []byte, mime string) (*plantIDDiagnoseResponse, error) {
	endpoint := c.DiagnoseEndpoint
	if endpoint == "" {
		endpoint = defaultPlantIDDiagnoseEndpoint
	}

	body := map[string]any{
		"images": []string{"data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)},
		"health": "all",
	}
	bs, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("plant_id diagnose: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bs))
	if err != nil {
		return nil, fmt.Errorf("plant_id diagnose: build request: %w", err)
	}
	req.Header.Set("Api-Key", c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPlantIDUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		var apiResp plantIDDiagnoseResponse
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return nil, fmt.Errorf("%w: decode: %v", ErrPlantIDBadResponse, err)
		}
		return &apiResp, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, ErrPlantIDUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrPlantIDRateLimit
	case resp.StatusCode == http.StatusBadRequest:
		return nil, ErrPlantIDImageRejected
	case resp.StatusCode >= 500:
		return nil, ErrPlantIDUnavailable
	default:
		return nil, fmt.Errorf("%w: status %d", ErrPlantIDBadResponse, resp.StatusCode)
	}
}
