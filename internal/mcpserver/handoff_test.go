package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func toolNames(t *testing.T) map[string]bool {
	t.Helper()
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	go New().Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	return names
}

// The handoff tool is opt-in: without BROJEV_HUMAN_CDP_URL the tool list is
// exactly what it was before handoff existed.
func TestHandoffToolOptIn(t *testing.T) {
	t.Setenv("BROJEV_HUMAN_CDP_URL", "")
	off := toolNames(t)
	if off["handoff"] {
		t.Fatal("handoff listed without BROJEV_HUMAN_CDP_URL")
	}
	t.Setenv("BROJEV_HUMAN_CDP_URL", "http://127.0.0.1:9223")
	on := toolNames(t)
	if !on["handoff"] || len(on) != len(off)+1 {
		t.Fatalf("with BROJEV_HUMAN_CDP_URL: %d tools (handoff=%v), without: %d", len(on), on["handoff"], len(off))
	}
}
