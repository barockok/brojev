// Package jev is a minimal client for TypeSafe's Jev (System One) API,
// called directly or through OpenRouter.
package jev

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
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

// Provider is JEV_PROVIDER: "typesafe", "openrouter", or "auto" (the
// default: TypeSafe when its key is set, otherwise OpenRouter).
func Provider() string {
	p := strings.ToLower(strings.TrimSpace(os.Getenv("JEV_PROVIDER")))
	if p == "" || strings.HasPrefix(p, "${") {
		return "auto"
	}
	return p
}

// KeyFromEnv picks the key and endpoint for Provider. TypeSafe uses
// TYPESAFE_API_KEY (or JEV_API_KEY) at api.typesafe.ai; OpenRouter uses
// OPENROUTER_API_KEY and serves the same API at the same token price (about
// 200 ms more per request). JEV_ENDPOINT overrides the URL. name is the key
// variable used; all empty when the provider has no key or is unknown.
func KeyFromEnv() (key, endpoint, name string) {
	p := Provider()
	if p == "auto" || p == "typesafe" {
		for _, n := range []string{"TYPESAFE_API_KEY", "JEV_API_KEY"} {
			if v := os.Getenv(n); v != "" {
				key, endpoint, name = v, DefaultEndpoint, n
				break
			}
		}
	}
	if key == "" && (p == "auto" || p == "openrouter") {
		if v := os.Getenv("OPENROUTER_API_KEY"); v != "" {
			key, endpoint, name = v, OpenRouterEndpoint, "OPENROUTER_API_KEY"
		}
	}
	if e := os.Getenv("JEV_ENDPOINT"); e != "" && key != "" {
		endpoint = e
	}
	return key, endpoint, name
}

// KeyError explains why KeyFromEnv found no key; nil when it did.
func KeyError() error {
	if key, _, _ := KeyFromEnv(); key != "" {
		return nil
	}
	switch p := Provider(); p {
	case "typesafe":
		return errors.New("jev: JEV_PROVIDER=typesafe but TYPESAFE_API_KEY is not set")
	case "openrouter":
		return errors.New("jev: JEV_PROVIDER=openrouter but OPENROUTER_API_KEY is not set")
	case "auto":
		return errors.New("jev: no API key: set OPENROUTER_API_KEY (or TYPESAFE_API_KEY to call TypeSafe directly)")
	default:
		return fmt.Errorf("jev: JEV_PROVIDER=%q: use auto, typesafe or openrouter", p)
	}
}

// FromEnv builds a client from KeyFromEnv and TYPESAFE_MODEL.
func FromEnv() (*Client, error) {
	if err := KeyError(); err != nil {
		return nil, err
	}
	key, endpoint, _ := KeyFromEnv()
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

// Asset is an image referenced from state or a question by its map key.
type Asset struct {
	Type   string `json:"type"`
	Format string `json:"format"`
	Data   string `json:"data_base64"`
}

type Request struct {
	Model     string           `json:"model"`
	State     any              `json:"state"`
	Questions map[string]any   `json:"questions"`
	Assets    map[string]Asset `json:"assets,omitempty"`
}

// ErrImagesRejected: the endpoint refused a request that carried an image
// (images not enabled for the key or route). Retry without the image.
var ErrImagesRejected = errors.New("jev: images rejected")

// Image wraps JPEG or PNG bytes as an Asset.
func Image(data []byte) Asset {
	format := "jpg"
	if len(data) > 4 && string(data[1:4]) == "PNG" {
		format = "png"
	}
	return Asset{Type: "image", Format: format, Data: base64.StdEncoding.EncodeToString(data)}
}

// Noul is a yes/no question; the answer is the probability of yes.
type Noul struct {
	Instructions any `json:"instructions"`
}

func (n Noul) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type         string `json:"type"`
		Instructions any    `json:"instructions"`
	}{"noul", n.Instructions})
}

type Answer struct {
	Noul          *float64           `json:"noul,omitempty"`
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

// ErrTooManyOptions: the API rejected a question for having more criteria than
// it allows (HTTP 422 "at most 255 items").
var ErrTooManyOptions = errors.New("jev: too many options in a question")

// Ask sends all questions in one request; they run in parallel server-side.
func (c *Client) Ask(ctx context.Context, state any, qs map[string]Choice) (*Response, error) {
	return c.AskWith(ctx, state, qs, nil, nil)
}

// AskWith is Ask with images: each key of assets is a reference string that
// state (or a question) mentions where the image belongs.
func (c *Client) AskWith(ctx context.Context, state any, qs map[string]Choice, assets map[string]Asset, nouls map[string]Noul) (*Response, error) {
	all := make(map[string]any, len(qs)+len(nouls))
	for k, v := range qs {
		all[k] = v
	}
	for k, v := range nouls {
		all[k] = v
	}
	body, err := json.Marshal(Request{Model: c.Model, State: state, Questions: all, Assets: assets})
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
		if len(assets) > 0 {
			req.Header.Set("TypeSafe-Danger-Zone-Images", "true")
		}
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
		case len(assets) > 0 && (resp.StatusCode == 400 || resp.StatusCode == 413 || resp.StatusCode == 403):
			return nil, fmt.Errorf("%w: HTTP %d: %s", ErrImagesRejected, resp.StatusCode, truncate(string(data), 200))
		case resp.StatusCode == 422 && strings.Contains(string(data), "at most"):
			return nil, fmt.Errorf("%w: %s", ErrTooManyOptions, truncate(string(data), 200))
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
