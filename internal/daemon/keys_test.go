package daemon

import (
	"encoding/json"
	"os"
	"testing"
)

func TestKeys(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "${user_config.typesafe_api_key}")
	t.Setenv("OPENROUTER_API_KEY", " ")
	t.Setenv("JEV_API_KEY", "")
	t.Setenv("JEV_PROVIDER", "${user_config.jev_provider}")
	CleanKeyEnv()
	for _, k := range []string{"TYPESAFE_API_KEY", "OPENROUTER_API_KEY", "JEV_API_KEY"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s should be unset", k)
		}
	}
	if got := KeyEnv(); got["JEV_PROVIDER"] != "auto" || got["TYPESAFE_API_KEY"] != "" || len(got) != len(keyNames) {
		t.Fatalf("KeyEnv: %v", KeyEnv())
	}
	raw, _ := json.Marshal(map[string]string{"TYPESAFE_API_KEY": "ts-1", "PATH": "/evil", "OPENROUTER_API_KEY": "${x}"})
	path := os.Getenv("PATH")
	if _, err := setKeys(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("TYPESAFE_API_KEY") })
	raw, _ = json.Marshal(map[string]string{"JEV_PROVIDER": "openrouter"})
	if _, err := setKeys(raw); err != nil || os.Getenv("JEV_PROVIDER") != "openrouter" {
		t.Fatalf("setKeys provider: %v %q", err, os.Getenv("JEV_PROVIDER"))
	}
	t.Cleanup(func() { os.Unsetenv("JEV_PROVIDER") })
	if os.Getenv("TYPESAFE_API_KEY") != "ts-1" || os.Getenv("PATH") != path || os.Getenv("OPENROUTER_API_KEY") != "" {
		t.Fatalf("setKeys: ts=%q or=%q path changed=%v", os.Getenv("TYPESAFE_API_KEY"), os.Getenv("OPENROUTER_API_KEY"), os.Getenv("PATH") != path)
	}
	if got := KeyEnv(); got["TYPESAFE_API_KEY"] != "ts-1" || got["JEV_PROVIDER"] != "openrouter" || got["OPENROUTER_API_KEY"] != "" {
		t.Fatalf("KeyEnv: %v", got)
	}
	// Sent empty: unset. Not sent: kept.
	raw, _ = json.Marshal(map[string]string{"TYPESAFE_API_KEY": ""})
	if _, err := setKeys(raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := os.LookupEnv("TYPESAFE_API_KEY"); ok || os.Getenv("JEV_PROVIDER") != "openrouter" {
		t.Fatalf("after clear: ts set=%v provider=%q", ok, os.Getenv("JEV_PROVIDER"))
	}
}
