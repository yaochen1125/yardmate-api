package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// VisionClient wraps OpenAI's chat/completions endpoint for vision-capable
// model calls. Two use cases today:
//
//  1. RerankIdentify (commit-3 path) — given the uploaded image + Plant.id top-N
//     candidates, return the one the model judges most likely.
//  2. DisambiguateDiseaseName — text-only call mapping a Plant.id disease name
//     to one of the 70 YardMate catalog ids when normalization missed.
//
// The API key never leaves the server. All errors are returned to callers
// for them to decide whether to fall back gracefully (RerankIdentify →
// AIEnhancedAt=null + Plant.id raw result; DisambiguateDiseaseName →
// CatalogID=null + generic Leaf-spot fallback).
type VisionClient struct {
	APIKey   string
	Endpoint string
	Model    string
	HTTP     *http.Client
}

const (
	// defaultVisionEndpoint is the OpenAI chat completions URL.
	defaultVisionEndpoint = "https://api.openai.com/v1/chat/completions"

	// defaultVisionModel is GPT-4o (Aug 2024 snapshot) — vision-capable +
	// cheaper than the latest Sonnet for the rerank workload (≈$0.005 per
	// request). Override via NewVisionClient if a future model is needed.
	defaultVisionModel = "gpt-4o-2024-08-06"

	// defaultVisionTimeout — server-side cap on the LLM call. 8 s leaves
	// ~7 s headroom inside the 15-s end-to-end client timeout after the
	// ≈4-s Plant.id call.
	defaultVisionTimeout = 8 * time.Second
)

// NewVisionClient builds a client with production defaults. apiKey from
// secrets.Vault (env OPENAI_API_KEY).
func NewVisionClient(apiKey string) *VisionClient {
	return &VisionClient{
		APIKey:   apiKey,
		Endpoint: defaultVisionEndpoint,
		Model:    defaultVisionModel,
		HTTP:     &http.Client{Timeout: defaultVisionTimeout},
	}
}

// openAIChatRequest is the subset of the OpenAI chat-completion request body
// we use. The user-message `content` is encoded as `any` so the same struct
// handles both plain-text (DisambiguateDiseaseName) and multimodal
// (RerankIdentify) shapes.
type openAIChatRequest struct {
	Model     string                  `json:"model"`
	MaxTokens int                     `json:"max_tokens"`
	Messages  []openAIChatRequestMsg  `json:"messages"`
}

type openAIChatRequestMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type openAIChatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// RerankIdentify takes the uploaded image bytes plus the Plant.id top-N
// candidate suggestions and asks the model to choose the most likely one.
// Reply is constrained to a single name verbatim; we return the matching
// candidate's Name. On any error / ambiguous reply, returns ("", err) and
// the handler leaves the suggestions ordered as Plant.id ranked them.
//
// SPEC §2.1 ai_enhance — only used when the request sets ai_enhance=true.
// AIEnhancedAt on the response is set by the handler iff this call returns
// successfully (RerankIdentify err == nil).
func (c *VisionClient) RerankIdentify(ctx context.Context, image []byte, mime string, candidates []Suggestion) (string, error) {
	if c == nil {
		return "", fmt.Errorf("vision: nil client")
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("vision: no candidates")
	}

	var b strings.Builder
	for i, s := range candidates {
		b.WriteString(fmt.Sprintf("%d. %s (scientific: %s)\n", i+1, s.Name, s.ScientificName))
	}

	sys := "You are a botanical identification assistant. Given a plant photo and a list of candidate names from a vision model, reply with ONLY the exact name of the candidate you judge most likely. Reply with one of the candidate names verbatim (the part before the parenthesis) — no commentary, no explanation, no rank prefix."
	user := "Candidates:\n" + b.String() + "\nReply with the exact name of the most likely match."

	body := openAIChatRequest{
		Model:     c.Model,
		MaxTokens: 80,
		Messages: []openAIChatRequestMsg{
			{Role: "system", Content: sys},
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "text", "text": user},
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": dataURL(mime, image)}},
				},
			},
		},
	}

	pick, err := c.post(ctx, body)
	if err != nil {
		return "", err
	}
	pick = strings.TrimSpace(pick)
	// Strip any leading "1. " / "Top: " style prefix the model occasionally
	// adds despite the system prompt.
	if i := strings.Index(pick, ". "); i >= 0 && i <= 3 {
		pick = pick[i+2:]
	}
	pick = strings.Trim(pick, "\"' ")

	// Match exact-name first, then case-insensitive contains.
	for _, s := range candidates {
		if pick == s.Name {
			return s.Name, nil
		}
	}
	lowPick := strings.ToLower(pick)
	for _, s := range candidates {
		if strings.EqualFold(pick, s.Name) ||
			strings.Contains(lowPick, strings.ToLower(s.Name)) ||
			strings.Contains(strings.ToLower(s.Name), lowPick) {
			return s.Name, nil
		}
	}
	return "", fmt.Errorf("vision rerank: pick %q not in candidate list", pick)
}

// DisambiguateDiseaseName asks the model (text-only) to pick the catalog id
// whose name best matches plantIDName from refs. Reply is constrained to a
// single id token like "L20", "P05", or "NONE" when nothing is close. Returns
// ("", nil) on a "NONE" reply or any malformed answer — callers should
// fall back to the generic catalog (L06 "Leaf spot").
//
// All errors (network, non-200, JSON decode) are returned to the caller so
// the diagnose handler can log + degrade gracefully.
func (c *VisionClient) DisambiguateDiseaseName(ctx context.Context, plantIDName string, refs []DiseaseNameRef) (string, error) {
	if c == nil {
		return "", fmt.Errorf("vision: nil client")
	}
	if len(refs) == 0 {
		return "", fmt.Errorf("vision: empty refs")
	}

	var b strings.Builder
	for _, r := range refs {
		b.WriteString(r.ID)
		b.WriteString(": ")
		b.WriteString(r.Name)
		b.WriteByte('\n')
	}

	sys := "You map plant disease names to a fixed catalog. Reply ONLY with the catalog id (like 'L20' or 'P05') that best matches the input. If nothing in the catalog is close, reply with 'NONE'. No commentary."
	user := "Input disease name: " + plantIDName + "\n\nCatalog:\n" + b.String() + "\nReply with the best-matching catalog id, or NONE."

	body := openAIChatRequest{
		Model:     c.Model,
		MaxTokens: 10,
		Messages: []openAIChatRequestMsg{
			{Role: "system", Content: sys},
			{Role: "user", Content: user},
		},
	}

	pick, err := c.post(ctx, body)
	if err != nil {
		return "", err
	}
	pick = strings.TrimSpace(strings.ToUpper(pick))
	// Take only the first whitespace-delimited token (the model occasionally
	// adds trailing prose like "L20 — Powdery mildew").
	if i := strings.IndexAny(pick, " \t\n,.;"); i > 0 {
		pick = pick[:i]
	}
	if pick == "" || pick == "NONE" {
		return "", nil
	}
	for _, r := range refs {
		if r.ID == pick {
			return pick, nil
		}
	}
	// LLM hallucinated an id outside the catalog — treat as miss.
	return "", nil
}

// post serializes body, POSTs to c.Endpoint, and returns the first choice's
// message content. Any non-200 status or decode failure is returned as an
// error.
func (c *VisionClient) post(ctx context.Context, body any) (string, error) {
	bs, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("vision: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(bs))
	if err != nil {
		return "", fmt.Errorf("vision: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("vision: http: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("vision: status %d body=%s", resp.StatusCode, raw)
	}
	var apiResp openAIChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return "", fmt.Errorf("vision: decode: %w", err)
	}
	if len(apiResp.Choices) == 0 {
		return "", fmt.Errorf("vision: no choices")
	}
	return apiResp.Choices[0].Message.Content, nil
}

// dataURL formats an image into the data: URL form OpenAI expects in the
// `image_url` content part. Used by RerankIdentify (commit-3).
func dataURL(mime string, image []byte) string {
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)
}
