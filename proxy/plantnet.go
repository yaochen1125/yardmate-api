package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"time"
)

// defaultPlantNetEndpoint is the Pl@ntNet API v2 identify URL (all organs).
// The api-key is a QUERY param appended at call time (SPEC §1.5) — NOT a
// header — so this base URL stays key-free and is overridable for tests.
// lang=en + nb-results=5 match the SPEC contract.
const defaultPlantNetEndpoint = "https://my-api.plantnet.org/v2/identify/all"

// defaultPlantNetTimeout caps the upstream Pl@ntNet call. 30 s matches the
// Plant.id client (SPEC §5.2) — the proxy is the slow-path tenant.
const defaultPlantNetTimeout = 30 * time.Second

// PlantNetClient calls Pl@ntNet API v2 for plant identification. It is the
// PRIMARY identify engine (SPEC §7); Plant.id is the fallback. Construct with
// NewPlantNetClient. Endpoint, Lang, NbResults, APIKey and HTTP are exported
// for tests (parity with PlantIDClient).
type PlantNetClient struct {
	APIKey    string
	Endpoint  string // base URL; api-key is appended as a query param at call time
	Lang      string // language for common names (default "en")
	NbResults int    // max results requested from Pl@ntNet (default 5)
	HTTP      *http.Client
}

// NewPlantNetClient returns a PlantNetClient with production defaults.
// apiKey comes from secrets.Vault (env PLANTNET_API_KEY) at startup —
// never exposed to clients. A missing key disables the primary engine
// (caller passes nil → Plant.id-only, SPEC §1.5).
func NewPlantNetClient(apiKey string) *PlantNetClient {
	return &PlantNetClient{
		APIKey:    apiKey,
		Endpoint:  defaultPlantNetEndpoint,
		Lang:      "en",
		NbResults: 5,
		HTTP:      &http.Client{Timeout: defaultPlantNetTimeout},
	}
}

// plantNetAPIResponse is the subset of the Pl@ntNet v2 /identify response
// that we consume. Extra fields (gbif, powo, query, version, language, etc.)
// are ignored.
type plantNetAPIResponse struct {
	BestMatch string `json:"bestMatch"`
	Results   []struct {
		Score   float64 `json:"score"`
		Species struct {
			ScientificNameWithoutAuthor string   `json:"scientificNameWithoutAuthor"`
			ScientificName              string   `json:"scientificName"`
			CommonNames                 []string `json:"commonNames"`
		} `json:"species"`
	} `json:"results"`
	RemainingIdentificationRequests int `json:"remainingIdentificationRequests"`
}

// toIdentifyResult maps Pl@ntNet's raw response into the V1 client shape
// (same IdentifyResult as the Plant.id path). Top-3 suggestions max
// (SPEC §2.1). ScientificName uses scientificNameWithoutAuthor (NOT the
// authored form) so the shared catalog resolver normalizes it identically
// to Plant.id — parity matters (SPEC §2.1 "plant_id mapping"). commonNames
// is normalized to non-nil (empty slice, not null, on the wire). PlantID and
// AIEnhancedAt are left nil here; the handler fills them, exactly like the
// Plant.id path.
func (r *plantNetAPIResponse) toIdentifyResult() *IdentifyResult {
	const maxSuggestions = 3

	out := &IdentifyResult{
		// Pl@ntNet only ever returns plant matches; presence of any result
		// means "is a plant". Empty results (e.g. a 404 mapped upstream) is
		// still is_plant=true with zero suggestions per SPEC §1.4.
		IsPlant:     true,
		Suggestions: []Suggestion{},
	}
	if len(r.Results) > 0 {
		out.IsPlantConfidence = r.Results[0].Score
	}

	n := len(r.Results)
	if n > maxSuggestions {
		n = maxSuggestions
	}
	for i := 0; i < n; i++ {
		s := r.Results[i]
		common := s.Species.CommonNames
		if common == nil {
			common = []string{}
		}
		out.Suggestions = append(out.Suggestions, Suggestion{
			Name:           s.Species.ScientificNameWithoutAuthor,
			ScientificName: s.Species.ScientificNameWithoutAuthor,
			CommonNames:    common,
			Confidence:     s.Score,
		})
	}
	return out
}

// Identify uploads the image to Pl@ntNet v2 and returns the sanitized
// IdentifyResult. mime must be "image/jpeg" or "image/png" (caller validates).
// organ is the Pl@ntNet organ hint (leaf/flower/fruit/bark/auto); "" → "auto"
// (caller already normalizes, this is a defensive guard).
//
// Single image ⇒ exactly one `images` file part + one `organs` text part
// (SPEC §1.5). The api-key is a query param, not a header.
//
// V1 buffers the image (caller passes a bytes.Reader). SPEC §6 pitfall 3
// calls out io.Pipe streaming as a V1.1 optimization; at V1 scale the memory
// cost is bounded and acceptable.
func (c *PlantNetClient) Identify(ctx context.Context, image io.Reader, mime, organ string) (*IdentifyResult, error) {
	if organ == "" {
		organ = "auto"
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// File part: form field name "images" (plural), Content-Type = sniffed
	// mime so Pl@ntNet treats it as the uploaded image.
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", `form-data; name="images"; filename="image"`)
	h.Set("Content-Type", mime)
	part, err := writer.CreatePart(h)
	if err != nil {
		return nil, fmt.Errorf("plantnet: build multipart: %w", err)
	}
	if _, err := io.Copy(part, image); err != nil {
		return nil, fmt.Errorf("plantnet: copy image: %w", err)
	}
	// Parallel text part: form field name "organs", body = the organ hint.
	if err := writer.WriteField("organs", organ); err != nil {
		return nil, fmt.Errorf("plantnet: write organs field: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("plantnet: close multipart: %w", err)
	}

	// Build the request URL: base endpoint + api-key/lang/nb-results query
	// params. api-key is appended here (not in Endpoint) so tests can point
	// Endpoint at httptest without needing a key (SPEC §1.5).
	endpoint := c.Endpoint
	if endpoint == "" {
		endpoint = defaultPlantNetEndpoint
	}
	lang := c.Lang
	if lang == "" {
		lang = "en"
	}
	nbResults := c.NbResults
	if nbResults <= 0 {
		nbResults = 5
	}
	reqURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("plantnet: parse endpoint: %w", err)
	}
	q := reqURL.Query()
	q.Set("api-key", c.APIKey)
	q.Set("lang", lang)
	q.Set("nb-results", fmt.Sprintf("%d", nbResults))
	reqURL.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL.String(), &body)
	if err != nil {
		return nil, fmt.Errorf("plantnet: build request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// Network error, timeout, or context cancel — all transient; the
		// handler advances to the Plant.id fallback.
		return nil, fmt.Errorf("%w: %v", ErrPlantNetUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		// 201 accepted for forward-compat / mock servers (parity with the
		// Plant.id client which sees 201 in prod).
		var apiResp plantNetAPIResponse
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return nil, fmt.Errorf("%w: decode: %v", ErrPlantNetBadResponse, err)
		}
		return apiResp.toIdentifyResult(), nil
	case resp.StatusCode == http.StatusNotFound:
		// Pl@ntNet "Species not found" — a VALID empty result, NOT an engine
		// failure (SPEC §1.4 / §1.5). Must NOT trigger the Plant.id fallback
		// and must not spend Plant.id credit.
		return &IdentifyResult{
			IsPlant:           true,
			IsPlantConfidence: 0,
			Suggestions:       []Suggestion{},
		}, nil
	case resp.StatusCode == http.StatusBadRequest,
		resp.StatusCode == http.StatusRequestEntityTooLarge:
		// 400 / 413 — Pl@ntNet rejected the image. Plant.id would reject the
		// same bytes, so this does NOT fall back (SPEC §1.4).
		return nil, ErrPlantNetImageRejected
	case resp.StatusCode == http.StatusUnauthorized,
		resp.StatusCode == http.StatusForbidden:
		return nil, ErrPlantNetUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrPlantNetRateLimit
	case resp.StatusCode >= 500:
		return nil, ErrPlantNetUnavailable
	default:
		return nil, fmt.Errorf("%w: status %d", ErrPlantNetBadResponse, resp.StatusCode)
	}
}
