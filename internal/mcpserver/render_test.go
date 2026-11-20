package mcpserver

import (
	"strings"
	"testing"
)

func TestRenderTurnstileLine(t *testing.T) {
	raw := []byte(`{"ms":1,"turnstile":{"detected":true,"state":"solved"}}`)
	out := renderAction("click", raw)
	if !strings.Contains(out, "TURNSTILE: solved") {
		t.Fatalf("render:\n%s", out)
	}

	raw = []byte(`{"ms":1,"turnstile":{"detected":true,"state":"unresolved","reason":"CapSolver not configured"}}`)
	out = renderAction("click", raw)
	if !strings.Contains(out, "TURNSTILE: unresolved (CapSolver not configured)") {
		t.Fatalf("render:\n%s", out)
	}

	raw = []byte(`{"ms":1,"turnstile":{"detected":false,"state":"absent"}}`)
	if out = renderAction("click", raw); strings.Contains(out, "TURNSTILE") {
		t.Fatalf("absent widget rendered:\n%s", out)
	}
}
