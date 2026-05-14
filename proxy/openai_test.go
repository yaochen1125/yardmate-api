package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const cannedOpenAIOK = `{
  "id": "chatcmpl-abc",
  "object": "chat.completion",
  "created": 1700000000,
  "model": "gpt-4o-mini",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "Monstera deliciosa prefers bright indirect light..."},
      "finish_reason": "stop"
    }
  ],
  "usage": {"prompt_tokens": 50, "completion_tokens": 100, "total_tokens": 150}
}`

func newTestOpenAIClient(t *testing.T, handler http.HandlerFunc) (*OpenAIClient, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	c := &OpenAIClient{
		APIKey:      "test-key",
		Endpoint:    srv.URL,
		Model:       "gpt-4o-mini",
		MaxTokens:   500,
		Temperature: 0.7,
		HTTP:        srv.Client(),
	}
	return c, srv
}

func TestOpenAIClient_Chat_Success(t *testing.T) {
	var (
		gotAuth   string
		gotCT     string
		gotBody   openAIChatRequest
	)
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode req: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedOpenAIOK)
	})
	defer srv.Close()

	result, err := c.Chat(context.Background(),
		"Monstera deliciosa", "Monstera deliciosa",
		"How often should I water it?")
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if !strings.HasPrefix(gotCT, "application/json") {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if gotBody.Model != "gpt-4o-mini" {
		t.Errorf("model = %q, want gpt-4o-mini", gotBody.Model)
	}
	if gotBody.MaxTokens != 500 || gotBody.Temperature != 0.7 {
		t.Errorf("max_tokens/temp = %d/%v, want 500/0.7", gotBody.MaxTokens, gotBody.Temperature)
	}
	if len(gotBody.Messages) != 2 {
		t.Fatalf("messages len = %d, want 2 (system + user)", len(gotBody.Messages))
	}
	// SECURITY (SPEC §6 pitfall 10): user input NEVER in system msg.
	if gotBody.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want system", gotBody.Messages[0].Role)
	}
	if strings.Contains(gotBody.Messages[0].Content, "Monstera") ||
		strings.Contains(gotBody.Messages[0].Content, "How often") {
		t.Errorf("user input leaked into system message: %q", gotBody.Messages[0].Content)
	}
	if gotBody.Messages[1].Role != "user" {
		t.Errorf("messages[1].role = %q, want user", gotBody.Messages[1].Role)
	}
	if !strings.Contains(gotBody.Messages[1].Content, "Monstera deliciosa") ||
		!strings.Contains(gotBody.Messages[1].Content, "How often") {
		t.Errorf("user message missing inputs: %q", gotBody.Messages[1].Content)
	}
	if !strings.Contains(result.Answer, "Monstera deliciosa prefers") {
		t.Errorf("answer = %q, want assistant content", result.Answer)
	}
}

func TestOpenAIClient_Chat_OmitsScientificNameIfEmpty(t *testing.T) {
	var gotBody openAIChatRequest
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, cannedOpenAIOK)
	})
	defer srv.Close()

	_, err := c.Chat(context.Background(), "Fern", "", "Water schedule?")
	if err != nil {
		t.Fatal(err)
	}
	userMsg := gotBody.Messages[1].Content
	if strings.Contains(userMsg, "scientific name") {
		t.Errorf("user msg unexpectedly mentions scientific name when empty: %q", userMsg)
	}
}

func TestOpenAIClient_Chat_Unauthorized(t *testing.T) {
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	defer srv.Close()
	_, err := c.Chat(context.Background(), "P", "", "Q")
	if !errors.Is(err, ErrOpenAIUnauthorized) {
		t.Errorf("err = %v, want ErrOpenAIUnauthorized", err)
	}
}

func TestOpenAIClient_Chat_RateLimit(t *testing.T) {
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	defer srv.Close()
	_, err := c.Chat(context.Background(), "P", "", "Q")
	if !errors.Is(err, ErrOpenAIRateLimit) {
		t.Errorf("err = %v, want ErrOpenAIRateLimit", err)
	}
}

func TestOpenAIClient_Chat_Unavailable_5xx(t *testing.T) {
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	defer srv.Close()
	_, err := c.Chat(context.Background(), "P", "", "Q")
	if !errors.Is(err, ErrOpenAIUnavailable) {
		t.Errorf("err = %v, want ErrOpenAIUnavailable", err)
	}
}

func TestOpenAIClient_Chat_NoChoices(t *testing.T) {
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[]}`)
	})
	defer srv.Close()
	_, err := c.Chat(context.Background(), "P", "", "Q")
	if !errors.Is(err, ErrOpenAIBadResponse) {
		t.Errorf("err = %v, want ErrOpenAIBadResponse", err)
	}
}

func TestOpenAIClient_Chat_EmptyContent(t *testing.T) {
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":""}}]}`)
	})
	defer srv.Close()
	_, err := c.Chat(context.Background(), "P", "", "Q")
	if !errors.Is(err, ErrOpenAIBadResponse) {
		t.Errorf("err = %v, want ErrOpenAIBadResponse", err)
	}
}

func TestOpenAIClient_Chat_MalformedJSON(t *testing.T) {
	c, srv := newTestOpenAIClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{this is not json`)
	})
	defer srv.Close()
	_, err := c.Chat(context.Background(), "P", "", "Q")
	if !errors.Is(err, ErrOpenAIBadResponse) {
		t.Errorf("err = %v, want ErrOpenAIBadResponse", err)
	}
}
