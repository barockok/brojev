package jev

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAskQuestionsWithImage(t *testing.T) {
	var got map[string]any
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("TypeSafe-Danger-Zone-Images")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_, _ = w.Write([]byte(`{"model":"fake","answers":{
			"checkbox":{"type":"box","confidence":0.82,"box":{"top":0.43,"bottom":0.56,"left":0.12,"right":0.19}},
			"present":{"type":"noul","noul":0.99},
			"state":{"type":"choice","choice":"checkbox","probabilities":{"checkbox":1},"confidence":1}
		},"usage":{"input_tokens":1250}}`))
	}))
	defer srv.Close()
	c := &Client{Key: "k", Model: "jev-latest", Endpoint: srv.URL, HTTP: srv.Client()}
	resp, err := c.AskQuestions(context.Background(),
		map[string]any{"shot": "shot"},
		map[string]Asset{"shot": {Type: "image", Format: "png", DataBase64: "abc"}},
		map[string]Question{
			"checkbox": {Type: "box", Instructions: "find the checkbox"},
			"present":  {Type: "noul", Instructions: "is it visible?"},
			"state":    {Type: "choice", Instructions: "widget state", Criteria: map[string]any{"checkbox": "empty checkbox"}},
		})
	if err != nil {
		t.Fatal(err)
	}
	if header != "true" {
		t.Fatalf("image header = %q", header)
	}
	assets := got["assets"].(map[string]any)
	shot := assets["shot"].(map[string]any)
	if shot["type"] != "image" || shot["format"] != "png" || shot["data_base64"] != "abc" {
		t.Fatalf("asset = %#v", shot)
	}
	qs := got["questions"].(map[string]any)
	for id, typ := range map[string]string{"checkbox": "box", "present": "noul", "state": "choice"} {
		if qs[id].(map[string]any)["type"] != typ {
			t.Fatalf("%s question = %#v", id, qs[id])
		}
	}
	box := resp.Answers["checkbox"]
	if box.Box == nil || math.Abs(box.Box.Left-0.12) > 1e-9 || math.Abs(box.Confidence-0.82) > 1e-9 {
		t.Fatalf("box answer = %+v", box)
	}
	if resp.Answers["present"].Noul != 0.99 || resp.Answers["state"].Choice != "checkbox" {
		t.Fatalf("answers = %+v", resp.Answers)
	}
	if math.Abs(Cost(resp.Usage)-0.0000525) > 1e-9 {
		t.Fatalf("cost = %v", Cost(resp.Usage))
	}
}

func TestAskStillSendsChoices(t *testing.T) {
	var got map[string]any
	var header string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("TypeSafe-Danger-Zone-Images")
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"answers":{"op":{"type":"choice","choice":"A","probabilities":{"A":1},"confidence":1}}}`))
	}))
	defer srv.Close()
	c := &Client{Key: "k", Model: "jev-latest", Endpoint: srv.URL, HTTP: srv.Client()}
	if _, err := c.Ask(context.Background(), map[string]any{"page": "x"}, map[string]Choice{
		"op": {Instructions: "pick", Criteria: map[string]any{"A": "a", "B": "b"}},
	}); err != nil {
		t.Fatal(err)
	}
	if header != "" {
		t.Fatalf("unexpected image header %q", header)
	}
	op := got["questions"].(map[string]any)["op"].(map[string]any)
	if op["type"] != "choice" || op["criteria"].(map[string]any)["A"] != "a" {
		t.Fatalf("question = %#v", op)
	}
	if _, ok := got["assets"]; ok {
		t.Fatalf("assets present: %#v", got["assets"])
	}
}

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
