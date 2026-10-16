package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestKeyFromEnv(t *testing.T) {
	both := map[string]string{"OPENROUTER_API_KEY": "or", "TYPESAFE_API_KEY": "ts"}
	with := func(m map[string]string, k, v string) map[string]string {
		out := map[string]string{k: v}
		for a, b := range m {
			out[a] = b
		}
		return out
	}
	cases := []struct {
		env           map[string]string
		key, endpoint string
		err           string
	}{
		{map[string]string{}, "", "", "no API key"},
		{map[string]string{"OPENROUTER_API_KEY": "or"}, "or", OpenRouterEndpoint, ""},
		{both, "ts", DefaultEndpoint, ""},
		{map[string]string{"OPENROUTER_API_KEY": "or", "JEV_API_KEY": "jv"}, "jv", DefaultEndpoint, ""},
		{map[string]string{"OPENROUTER_API_KEY": "or", "JEV_ENDPOINT": "http://x"}, "or", "http://x", ""},
		{map[string]string{"JEV_ENDPOINT": "http://x"}, "", "", "no API key"},
		{with(both, "JEV_PROVIDER", "openrouter"), "or", OpenRouterEndpoint, ""},
		{with(both, "JEV_PROVIDER", " TypeSafe "), "ts", DefaultEndpoint, ""},
		{with(both, "JEV_PROVIDER", "${user_config.jev_provider}"), "ts", DefaultEndpoint, ""},
		{map[string]string{"OPENROUTER_API_KEY": "or", "JEV_PROVIDER": "typesafe"}, "", "", "TYPESAFE_API_KEY is not set"},
		{map[string]string{"TYPESAFE_API_KEY": "ts", "JEV_PROVIDER": "openrouter"}, "", "", "OPENROUTER_API_KEY is not set"},
		{with(both, "JEV_PROVIDER", "bogus"), "", "", "use auto, typesafe or openrouter"},
	}
	for _, c := range cases {
		for _, n := range []string{"TYPESAFE_API_KEY", "JEV_API_KEY", "OPENROUTER_API_KEY", "JEV_ENDPOINT", "JEV_PROVIDER"} {
			t.Setenv(n, c.env[n])
		}
		key, endpoint, _ := KeyFromEnv()
		if key != c.key || endpoint != c.endpoint {
			t.Errorf("%v: got %q %q, want %q %q", c.env, key, endpoint, c.key, c.endpoint)
		}
		err := KeyError()
		if (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%v: KeyError %v, want %q", c.env, err, c.err)
		}
	}
}

func TestAsk422TooManyOptions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(422)
		w.Write([]byte(`{"detail":[{"type":"too_long","msg":"Dictionary should have at most 255 items after validation, not 323"}]}`))
	}))
	defer srv.Close()
	c := &Client{Key: "k", Model: "m", Endpoint: srv.URL, HTTP: srv.Client()}
	_, err := c.Ask(context.Background(), map[string]any{}, map[string]Choice{})
	if !errors.Is(err, ErrTooManyOptions) {
		t.Fatalf("got %v", err)
	}
}

func TestAskWithSendsImagesAndHeader(t *testing.T) {
	var gotHeader string
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("TypeSafe-Danger-Zone-Images")
		json.NewDecoder(r.Body).Decode(&body)
		w.Write([]byte(`{"model":"m","answers":{},"usage":{}}`))
	}))
	defer srv.Close()
	c := &Client{Key: "k", Model: "m", Endpoint: srv.URL, HTTP: srv.Client()}
	img := []byte{0xFF, 0xD8, 0xFF, 0xE0, 1, 2, 3}
	if _, err := c.AskWith(context.Background(), map[string]any{"screenshot": "ref1"}, map[string]Choice{}, map[string]Asset{"ref1": Image(img)}, nil); err != nil {
		t.Fatal(err)
	}
	a := body["assets"].(map[string]any)["ref1"].(map[string]any)
	if gotHeader != "true" || a["type"] != "image" || a["format"] != "jpg" || a["data_base64"] == "" {
		t.Fatalf("header=%q asset=%v", gotHeader, a)
	}
	if _, err := c.Ask(context.Background(), map[string]any{}, map[string]Choice{}); err != nil || gotHeader != "" {
		t.Fatalf("plain Ask must not send the images header (%q, %v)", gotHeader, err)
	}
}

func TestAskWithImageRejectedIsDistinct(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(400) }))
	defer srv.Close()
	c := &Client{Key: "k", Model: "m", Endpoint: srv.URL, HTTP: srv.Client()}
	_, err := c.AskWith(context.Background(), map[string]any{}, map[string]Choice{}, map[string]Asset{"r": Image([]byte{0xFF, 0xD8, 0xFF})}, nil)
	if !errors.Is(err, ErrImagesRejected) {
		t.Fatalf("got %v", err)
	}
}
