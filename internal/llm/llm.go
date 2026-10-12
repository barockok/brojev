// Package llm is the small text helper: an OpenAI-compatible chat model
// that writes field values and final answers. Jev picks; this writes.
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

type Client struct {
	Key, BaseURL, Model string
	NoReasoning         bool
	HTTP                *http.Client
}

// FromEnv reads TEXT_MODEL_API_KEY (or OPENROUTER_API_KEY), TEXT_MODEL_BASE_URL,
// TEXT_MODEL, TEXT_MODEL_REASONING. Defaults: OpenRouter, inception/mercury-2.5.
func FromEnv() (*Client, error) {
	key := os.Getenv("TEXT_MODEL_API_KEY")
	if key == "" {
		key = os.Getenv("OPENROUTER_API_KEY")
	}
	if key == "" {
		return nil, errors.New("llm: TEXT_MODEL_API_KEY or OPENROUTER_API_KEY not set")
	}
	c := &Client{
		Key:         key,
		BaseURL:     strings.TrimRight(env("TEXT_MODEL_BASE_URL", "https://openrouter.ai/api/v1"), "/"),
		Model:       env("TEXT_MODEL", "inception/mercury-2.5"),
		NoReasoning: env("TEXT_MODEL_REASONING", "none") == "none",
		HTTP:        &http.Client{Timeout: 20 * time.Second},
	}
	return c, nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// JSON asks for a JSON object and decodes it into out.
func (c *Client) JSON(ctx context.Context, system string, user any, out any) (time.Duration, error) {
	content, err := json.Marshal(user)
	if err != nil {
		return 0, err
	}
	body := map[string]any{
		"model":           c.Model,
		"max_tokens":      1024,
		"response_format": map[string]string{"type": "json_object"},
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": string(content)},
		},
	}
	if c.NoReasoning {
		body["reasoning"] = map[string]bool{"enabled": false}
	}
	b, _ := json.Marshal(body)
	start := time.Now()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("llm: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return 0, fmt.Errorf("llm: HTTP %d: %.300s", resp.StatusCode, data)
	}
	var r struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(data, &r); err != nil || len(r.Choices) == 0 {
		return 0, fmt.Errorf("llm: bad response: %.300s", data)
	}
	text := strings.TrimSpace(r.Choices[0].Message.Content)
	text = strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(text, "```json"), "```"), "```")
	if err := json.Unmarshal([]byte(text), out); err != nil {
		return 0, fmt.Errorf("llm: not JSON: %.200s", text)
	}
	return time.Since(start), nil
}
