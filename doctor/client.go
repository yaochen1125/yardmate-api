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
	// SentAt：该轮发出的 epoch 秒。0 = 旧客户端没带，不标时间。
	// 「一周后观察新芽」这类医嘱，模型必须知道用户是第二天回来的
	// 还是十天后回来的 —— 相对时间由服务端换算后注入（见 relativeAge）。
	SentAt int64 `json:"sent_at"`
}

// StreamRequest is one upstream generation.
type StreamRequest struct {
	Model    string
	// gpt-5 系列的思考档位。minimal 的产出是模板腔（真机对比 ChatGPT 实锤：
	// 无机理、无重构、行动放之四海皆准），默认提到 low；vault DOCTOR_REASONING_EFFORT 可调。
	ReasoningEffort string
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
	now := time.Now()
	var lastSent time.Time
	for _, h := range req.History {
		line := h.User
		if h.HadImages {
			if line == "" {
				line = "[sent a photo]"
			} else {
				line = "[sent a photo] " + line
			}
		}
		// 历史行前缀相对时间（[5 days ago]），最后一轮的时刻留给续问指令
		if h.SentAt > 0 {
			ts := time.Unix(h.SentAt, 0)
			if ts.Before(now) {
				line = "[" + relativeAge(now.Sub(ts)) + "] " + line
				lastSent = ts
			}
		}
		messages = append(messages, oaMessage{Role: "user", Content: line})
		messages = append(messages, oaMessage{Role: "assistant", Content: string(h.Reply)})
	}

	var parts []oaPart
	if len(req.History) > 0 {
		inst := followUpInstruction
		if !lastSent.IsZero() {
			inst += "\nIt is now " + relativeAge(now.Sub(lastSent)) + " since the previous exchange. Weigh your earlier timeline against this: enough time may (or may not) have passed for the changes you told them to watch for."
		}
		if len(req.Images) > 0 {
			inst += "\n" + followUpWithPhoto
		} else {
			inst += "\n" + followUpTextOnly
		}
		parts = append(parts, oaPart{Type: "text", Text: inst})
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
	// 语言钉子放在整个上下文的最末：4o-mini 对远处的 system 指令服从性差，
	// 用户英文输入会把早期字段拽成英文（staging 实测），最近的指令最有效。
	parts = append(parts, oaPart{Type: "text",
		Text: fmt.Sprintf("Reply entirely in %s. Every field, including observations.", languageName(req.Language))})
	messages = append(messages, oaMessage{Role: "user", Content: parts})

	body := oaRequest{
		Model:          req.Model,
		Stream:         true,
		StreamOptions:  oaStreamOpts{IncludeUsage: true},
		ResponseFormat: json.RawMessage(responseFormat),
		Messages:       messages,
	}
	// Only the gpt-5 family accepts reasoning_effort; other models 400 on it.
	// 默认 low：minimal 虽再省一半延迟，但产出是模板腔 —— 无机理、无重构、
	// 看不出「无根插穗养一大冠叶子」这类照片里的显性问题（真机对比实锤）。
	if strings.HasPrefix(req.Model, "gpt-5") {
		body.ReasoningEffort = req.ReasoningEffort
		if body.ReasoningEffort == "" {
			body.ReasoningEffort = "low"
		}
	}
	return json.Marshal(body)
}

// relativeAge renders a duration for the prompt ("3 hours ago" / "5 days ago").
// 粒度到天就够：医嘱的时间尺度是天/周，分钟级精度只添噪音。
func relativeAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments ago"
	case d < 90*time.Minute:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 36*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}
