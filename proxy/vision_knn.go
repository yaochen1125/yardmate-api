package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

// VisionKNNClient calls the L1 catalog-native vision-kNN microservice
// (vision/README.md) over localhost. It embeds the user photo with BioCLIP-2
// and returns nearest-neighbour catalog candidates + an in/out-of-catalog
// signal, resolved against a REAL-photo reference index (never AI-generated
// catalog art — see vision/README.md invariant #1).
//
// This is a NEW, uncalibrated signal: it is OFF by default (VISION_KNN_ENABLED),
// and even when on it is FAIL-OPEN — any unreachable / slow / malformed response
// yields no signal and leaves the main cascade untouched. The service runs
// same-host (Go → 127.0.0.1:8099), so there is no third-party dependency or key.
type VisionKNNClient struct {
	Endpoint string       // base URL, e.g. http://127.0.0.1:8099
	HTTP     *http.Client // hard timeout cap; ctx deadline still applies
}

const defaultVisionKNNEndpoint = "http://127.0.0.1:8099"

// NewVisionKNNClient builds a client. endpoint == "" falls back to the
// same-host default. The 6 s hard cap keeps a hung microservice from eating
// identify's wall-clock budget; the caller's ctx deadline bounds it further.
func NewVisionKNNClient(endpoint string) *VisionKNNClient {
	if endpoint == "" {
		endpoint = defaultVisionKNNEndpoint
	}
	return &VisionKNNClient{
		Endpoint: endpoint,
		HTTP:     &http.Client{Timeout: 6 * time.Second},
	}
}

// VisionKNNCandidate is one catalog match ranked by visual cosine similarity.
type VisionKNNCandidate struct {
	CatalogID string  `json:"catalog_id"` // AAA id
	VisionSim float64 `json:"vision_sim"` // cosine [0,1], higher = closer
}

// VisionKNNResponse mirrors the microservice JSON (vision/app.py).
type VisionKNNResponse struct {
	Candidates          []VisionKNNCandidate `json:"candidates"`
	NNSim               float64              `json:"nn_sim"`
	InCatalog           bool                 `json:"in_catalog"`
	InCatalogConfidence float64              `json:"in_catalog_confidence"`
	Model               string               `json:"model"`
}

// visionKNNResult carries the parallel-goroutine outcome (mirrors
// visionArbiterResult for the GPT arbiter).
type visionKNNResult struct {
	resp *VisionKNNResponse
	err  error
}

// Identify POSTs the photo as multipart/form-data (field "image") and decodes
// the response. Non-2xx or decode failure returns an error so the caller can
// fail open. imgBytes is the same buffer the engine cascade + GPT arbiter use.
func (c *VisionKNNClient) Identify(ctx context.Context, imgBytes []byte, _mime string) (*VisionKNNResponse, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("image", "photo")
	if err != nil {
		return nil, fmt.Errorf("vision-knn form: %w", err)
	}
	if _, err = fw.Write(imgBytes); err != nil {
		return nil, fmt.Errorf("vision-knn write: %w", err)
	}
	if err = mw.Close(); err != nil {
		return nil, fmt.Errorf("vision-knn close: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint+"/v1/vision/identify", &buf)
	if err != nil {
		return nil, fmt.Errorf("vision-knn req: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vision-knn do: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("vision-knn status %d: %s", resp.StatusCode, body)
	}
	var out VisionKNNResponse
	if err = json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("vision-knn decode: %w", err)
	}
	return &out, nil
}

// visionKNNAgreesWithDecision reports whether the vision-kNN top candidate
// CORROBORATES the current in-catalog decision: the decision resolved to a
// catalog id, vision independently says in-catalog, and vision's #1 nearest
// neighbour is that SAME catalog id. Pure (no I/O) for unit testing. This is
// the ONLY condition under which v1 acts (a raise-only confidence boost) — it
// never changes which plant is returned. Disagreement is logged, not acted on,
// until staging calibration (vision/README.md, P0_CONCLUSION low-strength start).
func visionKNNAgreesWithDecision(s0 *Suggestion, resp *VisionKNNResponse) bool {
	if s0 == nil || s0.PlantID == nil || resp == nil || !resp.InCatalog || len(resp.Candidates) == 0 {
		return false
	}
	return resp.Candidates[0].CatalogID == *s0.PlantID
}
