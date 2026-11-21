package mcpserver

import (
	"strings"
	"testing"
)

func TestApplyPin(t *testing.T) {
	// No pin: params pass through untouched, the agent picks freely.
	in := NavigateIn{Sess: Sess{Profile: "work"}, URL: "x"}
	if got, err := applyPin("", "navigate", in); err != nil || got.(NavigateIn) != in {
		t.Fatalf("no pin: %v %v", got, err)
	}

	// Pinned: every call runs on the pin, named or not.
	for _, params := range []any{
		NavigateIn{URL: "x"},
		NavigateIn{Sess: Sess{Session: "a", Profile: "work"}, URL: "x"},
		map[string]any{"url": "x"},
		nil,
	} {
		got, err := applyPin("work", "navigate", params)
		if err != nil {
			t.Fatalf("pinned %+v: %v", params, err)
		}
		if m := got.(map[string]any); m["profile"] != "work" {
			t.Fatalf("pinned %+v: profile = %v", params, m["profile"])
		}
	}
	// Pinned: another profile cannot be chosen.
	_, err := applyPin("work", "navigate", NavigateIn{Sess: Sess{Profile: "play"}, URL: "x"})
	if err == nil || !strings.Contains(err.Error(), "pinned") || !strings.Contains(err.Error(), "work") {
		t.Fatalf("override: err = %v", err)
	}

	// The profile tool: reading is fine and reports the pin, changing is not.
	got, err := applyPin("work", "profile", ProfileIn{Action: "current"})
	if err != nil || got.(map[string]any)["name"] != "work" {
		t.Fatalf("profile current: %v %v", got, err)
	}
	if _, err := applyPin("work", "profile", ProfileIn{}); err != nil {
		t.Fatalf("profile list: %v", err)
	}
	for _, action := range []string{"use", "add", "remove"} {
		if _, err := applyPin("work", "profile", ProfileIn{Action: action, Name: "play"}); err == nil || !strings.Contains(err.Error(), "pinned") {
			t.Fatalf("profile %s while pinned: err = %v", action, err)
		}
	}
	// Calls that do not touch a browser are left alone.
	if got, err := applyPin("work", "keys", map[string]string{"A": "b"}); err != nil || got.(map[string]string)["A"] != "b" {
		t.Fatalf("keys: %v %v", got, err)
	}
}
