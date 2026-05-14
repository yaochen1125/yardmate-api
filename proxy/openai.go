package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// defaultOpenAIEndpoint is the chat completions URL.
const defaultOpenAIEndpoint = "https://api.openai.com/v1/chat/completions"

// defaultOpenAIModel + max_tokens + temperature are policy-locked per SPEC §4.3.
const (
	defaultOpenAIModel       = "gpt-4o-mini"
	defaultOpenAIMaxTokens   = 500
	defaultOpenAITemperature = 0.7
	defaultOpenAITimeout     = 30 * time.Second
)

// openAISystemPrompt is server-controlled and never settable by the client
// (SPEC §4.3 + §6 pitfall 10). User-supplied plant_name / question are
// always placed in a separate `user` message, never concatenated here.
const openAISystemPrompt = `You are a plant care assistant for YardMate. Provide concise, practical advice in 2-3 short paragraphs. Always answer in English. Use the plant context the client provides; if the question is unrelated to plant care, briefly redirect. Never include external links or step-by-step instructions for unsafe pesticide use.`

// OpenAIClient calls OpenAI's chat completions API. Construct with NewOpenAIClient.
type OpenAIClient struct {
	APIKey      string
	Endpoint    string
	Model       string
	MaxTokens   int
	Temperature float64
	HTTP        *http.Client
}

// NewOpenAIClient returns an OpenAIClient with production defaults.
// apiKey comes from secrets.Vault (env OPENAI_API_KEY) at startup —
// never exposed to clients.
func NewOpenAIClient(apiKey string) *OpenAIClient {
	return &OpenAIClient{
		APIKey:      apiKey,
		Endpoint:    defaultOpenAIEndpoint,
		Model:       defaultOpenAIModel,
		MaxTokens:   defaultOpenAIMaxTokens,
		Temperature: defaultOpenAITemperature,
		HTTP:        &http.Client{Timeout: defaultOpenAITimeout},
	}
}

// Chat posts a single (system, user) message pair to OpenAI and returns the
// assistant's answer. Caller validates input lengths; this function trusts
// the caller's bounded values.
//
// User input (plantName, plantSciName, question) is concatenated into the
// USER message ONLY — never into the SYSTEM message. See SPEC §6 pitfall 10.
func (c *OpenAIClient) Chat(ctx context.Context, plantName, plantSciName, question string) (*ChatResult, error) {
	// Build the user message — bounded, single-shot.
	userContent := "Plant: " + plantName
	if plantSciName != "" {
		userContent += ", scientific name: " + plantSciName
	}
	userContent += "\nQuestion: " + question

	body, err := json.Marshal(openAIChatRequest{
		Model: c.Model,
		Messages: []openAIMessage{
			{Role: "system", Content: openAISystemPrompt},
			{Role: "user", Content: userContent},
		},
		MaxTokens:   c.MaxTokens,
		Temperature: c.Temperature,
	})
	if err != nil {
		return nil, fmt.Errorf("openai: marshal: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("openai: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOpenAIUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusOK:
		var apiResp openAIChatResponse
		if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
			return nil, fmt.Errorf("%w: decode: %v", ErrOpenAIBadResponse, err)
		}
		if len(apiResp.Choices) == 0 || apiResp.Choices[0].Message.Content == "" {
			return nil, fmt.Errorf("%w: no choices", ErrOpenAIBadResponse)
		}
		return &ChatResult{Answer: apiResp.Choices[0].Message.Content}, nil
	case resp.StatusCode == http.StatusUnauthorized, resp.StatusCode == http.StatusForbidden:
		return nil, ErrOpenAIUnauthorized
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, ErrOpenAIRateLimit
	case resp.StatusCode >= 500:
		return nil, ErrOpenAIUnavailable
	default:
		return nil, fmt.Errorf("%w: status %d", ErrOpenAIBadResponse, resp.StatusCode)
	}
}
