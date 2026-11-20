package turnstile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultEndpoint = "https://api.capsolver.com"

// CapSolver is the CapSolver Turnstile client.
type CapSolver struct {
	Key      string
	Endpoint string
	HTTP     *http.Client
	Poll     time.Duration
}

func NewCapSolver(key string) *CapSolver {
	return &CapSolver{
		Key: key, Endpoint: DefaultEndpoint,
		HTTP: &http.Client{Timeout: 30 * time.Second},
		Poll: 2 * time.Second,
	}
}

type capTask struct {
	Type       string            `json:"type"`
	WebsiteURL string            `json:"websiteURL"`
	WebsiteKey string            `json:"websiteKey"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type capCreate struct {
	ClientKey string  `json:"clientKey"`
	Task      capTask `json:"task"`
}

type capResult struct {
	ClientKey string `json:"clientKey"`
	TaskID    string `json:"taskId"`
}

type capResp struct {
	ErrorID          int    `json:"errorId"`
	ErrorCode        string `json:"errorCode"`
	ErrorDescription string `json:"errorDescription"`
	Status           string `json:"status"`
	TaskID           string `json:"taskId"`
	Solution         *struct {
		Token string `json:"token"`
	} `json:"solution"`
}

// Solve creates one AntiTurnstileTaskProxyLess task and polls until it is
// ready or ctx ends.
func (c *CapSolver) Solve(ctx context.Context, t Target) (string, error) {
	if strings.TrimSpace(c.Key) == "" {
		return "", errors.New("capsolver: API key missing")
	}
	if t.URL == "" || t.Sitekey == "" {
		return "", errors.New("capsolver: websiteURL and websiteKey are required")
	}
	md := map[string]string{}
	if t.Action != "" {
		md["action"] = t.Action
	}
	if t.CData != "" {
		md["cdata"] = t.CData
	}
	var created capResp
	if err := c.post(ctx, "/createTask", capCreate{ClientKey: c.Key, Task: capTask{
		Type: "AntiTurnstileTaskProxyLess", WebsiteURL: t.URL, WebsiteKey: t.Sitekey, Metadata: md,
	}}, &created); err != nil {
		return "", err
	}
	if created.ErrorID != 0 {
		return "", apiError("createTask", created)
	}
	if created.TaskID == "" {
		return "", errors.New("capsolver: createTask returned no taskId")
	}
	poll := c.Poll
	if poll <= 0 {
		poll = 2 * time.Second
	}
	for {
		var res capResp
		if err := c.post(ctx, "/getTaskResult", capResult{ClientKey: c.Key, TaskID: created.TaskID}, &res); err != nil {
			return "", err
		}
		if res.ErrorID != 0 {
			return "", apiError("getTaskResult", res)
		}
		if res.Status == "ready" {
			if res.Solution == nil || res.Solution.Token == "" {
				return "", errors.New("capsolver: ready without a token")
			}
			return res.Solution.Token, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

func (c *CapSolver) post(ctx context.Context, path string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	url := strings.TrimRight(c.Endpoint, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("capsolver %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("capsolver %s: HTTP %d", path, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return fmt.Errorf("capsolver %s: bad reply: %w", path, err)
	}
	return nil
}

func apiError(op string, r capResp) error {
	if r.ErrorCode == "" && r.ErrorDescription == "" {
		return fmt.Errorf("capsolver %s: errorId %d", op, r.ErrorID)
	}
	if r.ErrorDescription == "" {
		return fmt.Errorf("capsolver %s: %s", op, r.ErrorCode)
	}
	return fmt.Errorf("capsolver %s: %s (%s)", op, r.ErrorCode, r.ErrorDescription)
}
