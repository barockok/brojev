package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// keyNames are the API key variables brojev reads. The plugin passes them to
// the MCP server from userConfig (OS keychain); the MCP server forwards them
// to the long-lived daemon, which may predate this Claude session.
var keyNames = []string{"TYPESAFE_API_KEY", "JEV_API_KEY", "OPENROUTER_API_KEY", "TEXT_MODEL_API_KEY"}

// CleanKeyEnv unsets empty key variables and unexpanded "${...}" placeholders
// (an unset userConfig value), so ~/.brojev/env can still provide them.
func CleanKeyEnv() {
	for _, k := range keyNames {
		if v, ok := os.LookupEnv(k); ok && (strings.TrimSpace(v) == "" || strings.HasPrefix(v, "${")) {
			os.Unsetenv(k)
		}
	}
}

// KeyEnv returns the key variables set in this process.
func KeyEnv() map[string]string {
	out := map[string]string{}
	for _, k := range keyNames {
		if v := os.Getenv(k); v != "" && !strings.HasPrefix(v, "${") {
			out[k] = v
		}
	}
	return out
}

func setKeys(raw json.RawMessage) (any, error) {
	var in map[string]string
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("keys: bad params: %w", err)
	}
	set := 0
	for _, k := range keyNames {
		if v := in[k]; v != "" && !strings.HasPrefix(v, "${") {
			os.Setenv(k, v)
			set++
		}
	}
	return map[string]int{"set": set}, nil
}
