// Package jev is a minimal client for TypeSafe's Jev (System One) API,
// called directly or through OpenRouter.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"time"
)

const (
	DefaultEndpoint    = "https://api.typesafe.ai/v1/systemone"
	OpenRouterEndpoint = "https://openrouter.ai/api/v1/systemone"
)

// PricePerMTokInput is USD per million input tokens (output is free).
const PricePerMTokInput = 0.042

type Client struct {
	Key      string
	Model    string
	Endpoint string
	HTTP     *http.Client
}

// KeyFromEnv picks the key and endpoint. TYPESAFE_API_KEY (or JEV_API_KEY)
// calls TypeSafe directly; otherwise OPENROUTER_API_KEY calls Jev through
// OpenRouter, which serves the same API at the same token price (about 200 ms
// more per request). JEV_ENDPOINT overrides the URL. name is the variable used.
func KeyFromEnv() (key, endpoint, name string) {
	for _, n := range []string{"TYPESAFE_API_KEY", "JEV_API_KEY"} {
		if v := os.Getenv(n); v != "" {
			key, endpoint, name = v, DefaultEndpoint, n
			break
		}
	}
	if key == "" {
		if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
			key, endpoint, name = v, OpenRouterEndpoint, "OPENROUTER_API_KEY"
		}
	}
	if e := os.Getenv("JEV_ENDPOINT"); e != "" && key != "" {
		endpoint = e
	}
	return key, endpoint, name
}

// FromEnv builds a client from KeyFromEnv and TYPESAFE_MODEL.
func FromEnv() (*Client, error) {
	key, endpoint, _ := KeyFromEnv()
	if key == "" {
		return nil, errors.New("jev: no API key: set OPENROUTER_API_KEY (or TYPESAFE_API_KEY to call TypeSafe directly)")
	}
	model := os.Getenv("TYPESAFE_MODEL")
	if model == "" {
		model = "jev-latest"
	}
	return &Client{Key: key, Model: model, Endpoint: endpoint, HTTP: &http.Client{Timeout: 25 * time.Second}}, nil
}

// Choice is a question whose answer is one of Criteria's keys.
type Choice struct {
	Instructions any            `json:"instructions"`
	Criteria     map[string]any `json:"criteria"`
}

func (c Choice) MarshalJSON() ([]byte, error) {
	type alias Choice
	return json.Marshal(struct {
		Type string `json:"type"`
		alias
	}{"choice", alias(c)})
}

type Request struct {
	Model     string            `json:"model"`
	State     any               `json:"state"`
	Questions map[string]Choice `json:"questions"`
}

type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   Usage             `json:"usage"`
	Latency time.Duration     `json:"-"`
}

// Ask sends all questions in one request; they run in parallel server-side.
func (c *Client) Ask(ctx context.Context, state any, qs map[string]Choice) (*Response, error) {
	body, err := json.Marshal(Request{Model: c.Model, State: state, Questions: qs})
	if err != nil {
		return nil, err
	}
	start := time.Now()
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.Key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("jev: request failed; no action executed: %w", err)
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		switch {
		case (resp.StatusCode == 429 || resp.StatusCode == 529 || resp.StatusCode == 503) && attempt < 2:
			select {
			case <-time.After(time.Duration(500<<attempt) * time.Millisecond):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		case resp.StatusCode >= 300:
			return nil, fmt.Errorf("jev: HTTP %d: %s", resp.StatusCode, truncate(string(data), 300))
		}
		var r Response
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("jev: bad response: %w", err)
		}
		r.Latency = time.Since(start)
		return &r, nil
	}
}

// Valid checks an answer is on-contract for the offered option keys:
// choice offered, probabilities over exactly those keys, finite, summing to
// ~1, and the choice is the argmax.
func Valid(a Answer, keys map[string]bool) error {
	if !keys[a.Choice] {
		return fmt.Errorf("choice %q was not offered", a.Choice)
	}
	if len(a.Probabilities) != len(keys) {
		return fmt.Errorf("probabilities cover %d options, offered %d", len(a.Probabilities), len(keys))
	}
	sum, best := 0.0, 0.0
	for k, p := range a.Probabilities {
		if !keys[k] || math.IsNaN(p) || p < 0 || p > 1 {
			return fmt.Errorf("bad probability %q=%v", k, p)
		}
		sum += p
		best = max(best, p)
	}
	if math.Abs(sum-1) > 0.02 {
		return fmt.Errorf("probabilities sum to %.3f", sum)
	}
	if a.Probabilities[a.Choice] < best-1e-6 {
		return errors.New("choice is not the most probable option")
	}
	return nil
}

func Cost(u Usage) float64 { return float64(u.InputTokens) * PricePerMTokInput / 1e6 }

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
