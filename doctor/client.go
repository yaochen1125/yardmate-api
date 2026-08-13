package doctor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors. The handler maps them to SSE error codes (SPEC §3);
// none of them ever carries upstream response bodies to the client.
var (
	// ErrUpstreamUnavailable — network failure, non-200, or malformed SSE
	// from OpenAI. Maps to `bad_upstream`.
	ErrUpstreamUnavailable = errors.New("doctor: upstream unavailable")
	// ErrUpstreamTimeout — the stream exceeded its total deadline. Maps to
	// `upstream_timeout`.
	ErrUpstreamTimeout = errors.New("doctor: upstream timeout")
	// ErrBadReply — upstream finished but the accumulated document is not
	// the JSON object the schema promised. Maps to `bad_reply`.
	ErrBadReply = errors.New("doctor: bad reply")
)

const (
	defaultEndpoint = "https://api.openai.com/v1/chat/completions"

	// responseHeaderTimeout caps the wait for OpenAI's response HEADERS.
	// First content token typically arrives 1–3 s in; 20 s means a hung
	// upstream fails fast enough that the client's spinner is still honest.
	responseHeaderTimeout = 20 * time.Second

	// streamTimeout caps the WHOLE generation. A reply is ~300–700 output
	// tokens; even a slow model finishes far inside 180 s. The handler
	// derives its request context from this.
	streamTimeout = 180 * time.Second

	// scannerBuffer sizes bufio.Scanner for upstream SSE lines. A single
	// data: line carries one chunk (tiny), but the final usage chunk and
	// any error body can be larger; 1 MB is far above anything real.
	scannerBuffer = 1 << 20
)

// Usage is the token accounting from the final upstream chunk
// (stream_options.include_usage). Streaming does not change billing; this
// is surfaced to the client so the debug screen can show real numbers.
type Usage struct {
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Model            string `json:"model"`
}

// HistoryTurn is one prior exchange, replayed for context. Reply is the
// assistant's structured JSON from that turn, passed back verbatim —
// the first turn's images are never re-sent (SPEC §2).
type HistoryTurn struct {
	User      string          `json:"user"`
	HadImages bool            `json:"had_images"`
	Reply     json.RawMessage `json:"reply"`
}

// StreamRequest is one upstream generation.
type StreamRequest struct {
	Model    string
	Language string
	Units    string
	History  []HistoryTurn
	UserText string
	Images   [][]byte // sniffed jpeg/png/webp, ≤3 (validated by the handler)
}

// Client streams chat/completions. The zero value is not usable; NewClient
// wires the transport whose ResponseHeaderTimeout replaces a hard
// http.Client.Timeout (a hard timeout would kill the stream mid-generation —
// the same trap VisionClient documents for its identify client).
type Client struct {
	apiKey   string
	endpoint string
	http     *http.Client
}

// NewClient builds the streaming client. endpoint == "" → OpenAI.
func NewClient(apiKey, endpoint string) *Client {
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return &Client{
		apiKey:   apiKey,
		endpoint: endpoint,
		http: &http.Client{
			// No Timeout: the context caps total duration instead.
			Transport: &http.Transport{
				ResponseHeaderTimeout: responseHeaderTimeout,
				MaxIdleConnsPerHost:   4,
			},
		},
	}
}

// Stream runs one generation. onObservation fires once per observation
// string, in order, as each completes inside the partial document — this is
// the live-progress feed. Returns the full validated reply JSON and usage
// (nil if upstream omitted it).
//
// ctx should carry the caller's deadline (streamTimeout) AND the client
// disconnect (r.Context()) so an abandoned phone screen stops the paid
// generation immediately.
func (c *Client) Stream(
	ctx context.Context,
	req StreamRequest,
	onObservation func(index int, text string),
) (json.RawMessage, *Usage, error) {
	body, err := buildBody(req)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}

	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	hr.Header.Set("Authorization", "Bearer "+c.apiKey)
	hr.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(hr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ErrUpstreamTimeout
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Read a bounded slice for the server log; never forwarded.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		return nil, nil, fmt.Errorf("%w: status %d: %s", ErrUpstreamUnavailable, resp.StatusCode, snippet)
	}

	var (
		accumulated []byte
		emitted     int
		usage       *Usage
	)

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), scannerBuffer)
	for sc.Scan() {
		line := sc.Bytes()
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(line[5:])
		if bytes.Equal(payload, []byte("[DONE]")) {
			break
		}
		var chunk oaChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			continue // one mangled keep-alive line must not kill the stream
		}
		if chunk.Usage != nil {
			usage = &Usage{
				PromptTokens:     chunk.Usage.PromptTokens,
				CompletionTokens: chunk.Usage.CompletionTokens,
				Model:            req.Model,
			}
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		piece := chunk.Choices[0].Delta.Content
		if piece == "" {
			continue
		}
		accumulated = append(accumulated, piece...)

		// Rescanning is O(len(accumulated)); doing it on every chunk would be
		// O(n²) over the stream. Only a chunk containing a closing character
		// can complete a new string.
		if !strings.ContainsAny(piece, `"]`) {
			continue
		}
		done := CompletedStrings("observations", accumulated)
		for emitted < len(done) {
			onObservation(emitted, done[emitted])
			emitted++
		}
	}
	if err := sc.Err(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ErrUpstreamTimeout
		}
		return nil, nil, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, err)
	}

	// The reply must be a complete JSON object. Strict schema mode makes
	// this near-certain; the guard catches truncation (upstream died between
	// chunks without an error) and refusals.
	trimmed := bytes.TrimSpace(accumulated)
	if len(trimmed) == 0 || trimmed[0] != '{' || !json.Valid(trimmed) {
		return nil, usage, ErrBadReply
	}
	return json.RawMessage(trimmed), usage, nil
}

// ---- wire format ----

type oaChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type oaMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string, or []oaPart for the multimodal user turn
}

type oaPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *oaImageURL `json:"image_url,omitempty"`
}

type oaImageURL struct {
	URL string `json:"url"`
}

type oaRequest struct {
	Model           string          `json:"model"`
	Stream          bool            `json:"stream"`
	StreamOptions   oaStreamOpts    `json:"stream_options"`
	ResponseFormat  json.RawMessage `json:"response_format"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	Messages        []oaMessage     `json:"messages"`
}

type oaStreamOpts struct {
	IncludeUsage bool `json:"include_usage"`
}

func buildBody(req StreamRequest) ([]byte, error) {
	messages := []oaMessage{
		{Role: "system", Content: SystemPrompt(req.Language, req.Units)},
	}
	for _, h := range req.History {
		line := h.User
		if h.HadImages {
			if line == "" {
				line = "[sent a photo]"
			} else {
				line = "[sent a photo] " + line
			}
		}
		messages = append(messages, oaMessage{Role: "user", Content: line})
		messages = append(messages, oaMessage{Role: "assistant", Content: string(h.Reply)})
	}

	var parts []oaPart
	if len(req.History) > 0 {
		parts = append(parts, oaPart{Type: "text", Text: followUpInstruction})
	}
	if req.UserText != "" {
		parts = append(parts, oaPart{Type: "text", Text: req.UserText})
	}
	for _, img := range req.Images {
		mime := http.DetectContentType(img)
		parts = append(parts, oaPart{Type: "image_url", ImageURL: &oaImageURL{
			URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img),
		}})
	}
	if len(parts) == 0 {
		// Handler validation guarantees text or images; belt and braces for
		// direct library use.
		parts = append(parts, oaPart{Type: "text", Text: "Here is the plant."})
	}
	messages = append(messages, oaMessage{Role: "user", Content: parts})

	body := oaRequest{
		Model:          req.Model,
		Stream:         true,
		StreamOptions:  oaStreamOpts{IncludeUsage: true},
		ResponseFormat: json.RawMessage(responseFormat),
		Messages:       messages,
	}
	// Only the gpt-5 family accepts reasoning_effort; other models 400 on it.
	// The task needs no long-chain reasoning, and minimal halves latency
	// (14.9 s → 7.7 s measured on gpt-5 in the PoC).
	if strings.HasPrefix(req.Model, "gpt-5") {
		body.ReasoningEffort = "minimal"
	}
	return json.Marshal(body)
}
