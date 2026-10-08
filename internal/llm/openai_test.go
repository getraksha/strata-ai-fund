package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newTestClient(url string) *OpenAI {
	return &OpenAI{APIKey: "test-key", BaseURL: url, Model: "test-model",
		HTTP: &http.Client{Timeout: 5 * time.Second}, MaxRetries: 1}
}

func chatResponse(content string) string {
	b, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": content},
			"finish_reason": "stop",
		}},
	})
	return string(b)
}

func TestSendsStrictSchemaAndReturnsContent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("bad request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		var body struct {
			Model          string `json:"model"`
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Name   string `json:"name"`
					Strict bool   `json:"strict"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		data, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(data, &body); err != nil {
			t.Errorf("request body is not JSON: %v", err)
			return
		}
		if body.Model != "test-model" || body.ResponseFormat.Type != "json_schema" ||
			!body.ResponseFormat.JSONSchema.Strict || body.ResponseFormat.JSONSchema.Name != "x" {
			t.Errorf("unexpected body: %s", data)
		}
		io.WriteString(w, chatResponse(`{"ok":true}`))
	}))
	defer srv.Close()

	out, err := newTestClient(srv.URL).Complete(context.Background(), Request{Name: "x", Schema: map[string]any{"type": "object"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"ok":true}` {
		t.Fatalf("got %s", out)
	}
}

func TestRetriesTransientErrors(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		io.WriteString(w, chatResponse(`{}`))
	}))
	defer srv.Close()

	if _, err := newTestClient(srv.URL).Complete(context.Background(), Request{Name: "x"}); err != nil {
		t.Fatalf("should succeed after one retry: %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

func TestFailsOnRefusalAndBadRequest(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"refusal": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, `{"choices":[{"message":{"content":null,"refusal":"no"},"finish_reason":"stop"}]}`)
		},
		"not json": func(w http.ResponseWriter, r *http.Request) {
			io.WriteString(w, chatResponse("this is not json"))
		},
		"400": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"error":"bad schema"}`, http.StatusBadRequest)
		},
	}
	for name, h := range cases {
		srv := httptest.NewServer(h)
		_, err := newTestClient(srv.URL).Complete(context.Background(), Request{Name: "x"})
		srv.Close()
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestFromEnvRequiresKeyAndModel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := NewOpenAIFromEnv("m"); err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY") {
		t.Errorf("missing key: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "k")
	t.Setenv("OPENAI_MODEL", "")
	if _, err := NewOpenAIFromEnv(""); err == nil || !strings.Contains(err.Error(), "model") {
		t.Errorf("missing model: %v", err)
	}
}
