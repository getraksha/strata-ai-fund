package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

// OpenAI calls the Chat Completions API with strict JSON-schema output.
// Plain net/http: one endpoint, no SDK, request and response fully visible.
type OpenAI struct {
	APIKey     string
	BaseURL    string // default https://api.openai.com/v1
	Model      string
	HTTP       *http.Client
	MaxRetries int // retries on 429 / 5xx / network errors
}

// NewOpenAIFromEnv reads OPENAI_API_KEY, OPENAI_BASE_URL (optional) and,
// if model is empty, OPENAI_MODEL.
func NewOpenAIFromEnv(model string) (*OpenAI, error) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		return nil, errors.New("OPENAI_API_KEY is not set")
	}
	if model == "" {
		model = os.Getenv("OPENAI_MODEL")
	}
	if model == "" {
		return nil, errors.New("no model: pass -model or set OPENAI_MODEL")
	}
	base := os.Getenv("OPENAI_BASE_URL")
	if base == "" {
		base = "https://api.openai.com/v1"
	}
	return &OpenAI{
		APIKey:     key,
		BaseURL:    strings.TrimRight(base, "/"),
		Model:      model,
		HTTP:       &http.Client{Timeout: 120 * time.Second},
		MaxRetries: 2,
	}, nil
}

func (o *OpenAI) Name() string { return "openai/" + o.Model }

func (o *OpenAI) Complete(ctx context.Context, req Request) (json.RawMessage, error) {
	payload := map[string]any{
		"model": o.Model,
		"messages": []map[string]string{
			{"role": "system", "content": req.System},
			{"role": "user", "content": req.User},
		},
		"response_format": map[string]any{
			"type": "json_schema",
			"json_schema": map[string]any{
				"name":   req.Name,
				"strict": true,
				"schema": req.Schema,
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var lastErr error
	for attempt := 0; attempt <= o.MaxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * time.Second):
			}
		}
		out, retry, err := o.do(ctx, body)
		if err == nil {
			return out, nil
		}
		lastErr = err
		if !retry {
			break
		}
	}
	return nil, lastErr
}

// do performs one HTTP call. retry reports whether the failure is transient.
func (o *OpenAI) do(ctx context.Context, body []byte) (out json.RawMessage, retry bool, err error) {
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	hreq.Header.Set("Authorization", "Bearer "+o.APIKey)
	hreq.Header.Set("Content-Type", "application/json")

	client := o.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(hreq)
	if err != nil {
		return nil, true, fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, true, fmt.Errorf("openai: reading response: %w", err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		return nil, true, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, clip(data))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("openai: HTTP %d: %s", resp.StatusCode, clip(data))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
				Refusal string `json:"refusal"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, false, fmt.Errorf("openai: decoding response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return nil, false, errors.New("openai: response has no choices")
	}
	c := parsed.Choices[0]
	switch {
	case c.Message.Refusal != "":
		return nil, false, fmt.Errorf("openai: model refused: %s", c.Message.Refusal)
	case c.FinishReason == "length":
		return nil, false, errors.New("openai: output truncated (finish_reason=length)")
	case !json.Valid([]byte(c.Message.Content)):
		return nil, false, fmt.Errorf("openai: content is not valid JSON: %s", clip([]byte(c.Message.Content)))
	}
	return json.RawMessage(c.Message.Content), false, nil
}

func clip(b []byte) string {
	const n = 300
	s := strings.TrimSpace(string(b))
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
