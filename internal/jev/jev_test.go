package jev

import "testing"

func TestKeyFromEnv(t *testing.T) {
	cases := []struct {
		env           map[string]string
		key, endpoint string
	}{
		{map[string]string{}, "", ""},
		{map[string]string{"OPENROUTER_API_KEY": "or"}, "or", OpenRouterEndpoint},
		{map[string]string{"OPENROUTER_API_KEY": "or", "TYPESAFE_API_KEY": "ts"}, "ts", DefaultEndpoint},
		{map[string]string{"OPENROUTER_API_KEY": "or", "JEV_API_KEY": "jv"}, "jv", DefaultEndpoint},
		{map[string]string{"OPENROUTER_API_KEY": "or", "JEV_ENDPOINT": "http://x"}, "or", "http://x"},
		{map[string]string{"JEV_ENDPOINT": "http://x"}, "", ""},
	}
	for _, c := range cases {
		for _, n := range []string{"TYPESAFE_API_KEY", "JEV_API_KEY", "OPENROUTER_API_KEY", "JEV_ENDPOINT"} {
			t.Setenv(n, c.env[n])
		}
		key, endpoint, _ := KeyFromEnv()
		if key != c.key || endpoint != c.endpoint {
			t.Errorf("%v: got %q %q, want %q %q", c.env, key, endpoint, c.key, c.endpoint)
		}
	}
}
