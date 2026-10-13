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
	CleanKeyEnv()
	for _, k := range []string{"TYPESAFE_API_KEY", "OPENROUTER_API_KEY", "JEV_API_KEY"} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s should be unset", k)
		}
	}
	if len(KeyEnv()) != 0 {
		t.Fatalf("KeyEnv: %v", KeyEnv())
	}
	raw, _ := json.Marshal(map[string]string{"TYPESAFE_API_KEY": "ts-1", "PATH": "/evil", "OPENROUTER_API_KEY": "${x}"})
	path := os.Getenv("PATH")
	if _, err := setKeys(raw); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Unsetenv("TYPESAFE_API_KEY") })
	if os.Getenv("TYPESAFE_API_KEY") != "ts-1" || os.Getenv("PATH") != path || os.Getenv("OPENROUTER_API_KEY") != "" {
		t.Fatalf("setKeys: ts=%q or=%q path changed=%v", os.Getenv("TYPESAFE_API_KEY"), os.Getenv("OPENROUTER_API_KEY"), os.Getenv("PATH") != path)
	}
	if got := KeyEnv(); got["TYPESAFE_API_KEY"] != "ts-1" || len(got) != 1 {
		t.Fatalf("KeyEnv: %v", got)
	}
}
