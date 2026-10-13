package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// keyNames are the API key variables brojev reads, plus JEV_PROVIDER. The plugin passes them to
// the MCP server from userConfig (OS keychain); the MCP server forwards them
// to the long-lived daemon, which may predate this Claude session.
var keyNames = []string{"TYPESAFE_API_KEY", "JEV_API_KEY", "OPENROUTER_API_KEY", "TEXT_MODEL_API_KEY", "JEV_PROVIDER"}

// settingPrefix marks plugin settings in the MCP server's environment. The
// plugin passes them under their own names (BROJEV_SETTING_OPENROUTER_API_KEY)
// because an unset setting would otherwise replace a key exported in the
// user's shell with an empty value.
const settingPrefix = "BROJEV_SETTING_"

// CleanKeyEnv applies plugin settings over the inherited environment, then
// unsets empty key variables and unexpanded "${...}" placeholders, so the
// shell environment and then ~/.brojev/env can still provide them.
// Precedence: plugin setting, environment, ~/.brojev/env.
func CleanKeyEnv() {
	for _, k := range keyNames {
		if v, ok := os.LookupEnv(settingPrefix + k); ok {
			os.Unsetenv(settingPrefix + k)
			if strings.TrimSpace(v) != "" && !strings.HasPrefix(v, "${") {
				os.Setenv(k, v)
			}
		}
	}
	for _, k := range keyNames {
		if v, ok := os.LookupEnv(k); ok && (strings.TrimSpace(v) == "" || strings.HasPrefix(v, "${")) {
			os.Unsetenv(k)
		}
	}
}

// KeyEnv returns every key variable, "" when unset (JEV_PROVIDER "auto"),
// so the daemon drops a key or setting this session no longer has.
func KeyEnv() map[string]string {
	out := map[string]string{}
	for _, k := range keyNames {
		out[k] = ""
		if v := os.Getenv(k); v != "" && !strings.HasPrefix(v, "${") {
			out[k] = v
		}
	}
	if out["JEV_PROVIDER"] == "" {
		out["JEV_PROVIDER"] = "auto"
	}
	return out
}

func setKeys(raw json.RawMessage) (any, error) {
	var in map[string]string
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, fmt.Errorf("keys: bad params: %w", err)
	}
	// A name sent empty is unset; a name not sent (older clients) is kept.
	set := 0
	for _, k := range keyNames {
		v, ok := in[k]
		switch {
		case !ok:
		case v != "" && !strings.HasPrefix(v, "${"):
			os.Setenv(k, v)
			set++
		default:
			os.Unsetenv(k)
		}
	}
	return map[string]int{"set": set}, nil
}
