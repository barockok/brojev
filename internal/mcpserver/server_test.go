package mcpserver

// End-to-end over MCP: in-memory client -> brojev MCP server -> brojevd ->
// headless Chrome. Run: BROJEV_E2E=1 go test ./internal/mcpserver/

import (
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/barockok/brojev/internal/daemon"
)

func TestMCPEndToEnd(t *testing.T) {
	if os.Getenv("BROJEV_E2E") != "1" {
		t.Skip("set BROJEV_E2E=1")
	}
	// A real daemon from a freshly built binary, isolated home, headless.
	home := t.TempDir()
	bin := filepath.Join(home, "brojev")
	if out, err := exec.Command("go", "build", "-o", bin, "../../cmd/brojev").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	t.Setenv("BROJEV_HOME", home)
	t.Setenv("BROJEV_HEADLESS", "1")
	cmd := exec.Command(bin, "daemon", "--headless")
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	t.Cleanup(func() {
		daemon.Call(context.Background(), "shutdown", nil, nil)
		cmd.Wait()
		exec.Command("pkill", "-f", "user-data-dir="+home).Run()
		for i := 0; i < 50 && exec.Command("pgrep", "-f", "user-data-dir="+home).Run() == nil; i++ {
			time.Sleep(100 * time.Millisecond) // let Chrome exit before TempDir cleanup
		}
	})
	for i := 0; i < 50 && daemon.Call(ctx, "ping", nil, nil) != nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}

	ct, st := mcp.NewInMemoryTransports()
	go New().Run(ctx, st)
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) < 21 {
		t.Fatalf("only %d tools", len(tools.Tools))
	}

	callText := func(name string, args map[string]any) string {
		t.Helper()
		res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var b strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				b.WriteString(tc.Text)
			}
		}
		if res.IsError {
			t.Fatalf("%s failed: %s", name, b.String())
		}
		return b.String()
	}

	page := "data:text/html," + url.PathEscape(`<title>mcp</title><input aria-label="Query"><button onclick="out.textContent='got '+document.querySelector('input').value">Go</button><p id=out></p>`)
	snap := callText("navigate", map[string]any{"url": page})
	if !strings.Contains(snap, `textbox "Query"`) || !strings.Contains(snap, `button "Go"`) {
		t.Fatalf("navigate snapshot:\n%s", snap)
	}
	ref := func(snap, needle string) string {
		for _, l := range strings.Split(snap, "\n") {
			if strings.Contains(l, needle) && strings.HasPrefix(l, "[") {
				return strings.Trim(strings.Fields(l)[0], "[]")
			}
		}
		t.Fatalf("no %q in\n%s", needle, snap)
		return ""
	}
	snap = callText("type", map[string]any{"ref": ref(snap, `"Query"`), "text": "brojev"})
	snap = callText("click", map[string]any{"ref": ref(snap, `"Go"`)})
	if !strings.Contains(snap, "got brojev") {
		t.Fatalf("click result:\n%s", snap)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "screenshot", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("screenshot: %v %+v", err, res)
	}
	if img, ok := res.Content[0].(*mcp.ImageContent); !ok || len(img.Data) < 1000 || img.MIMEType != "image/jpeg" {
		t.Fatalf("screenshot content: %T", res.Content[0])
	}
	if out := callText("profile", map[string]any{"action": "add", "name": "second"}); !strings.Contains(out, `"second"`) {
		t.Fatalf("profile add: %s", out)
	}
	callText("profile", map[string]any{"action": "use", "name": "second"})
	if out := callText("navigate", map[string]any{"url": page}); !strings.Contains(out, `"Go"`) {
		t.Fatalf("navigate on second profile: %s", out)
	}
	if out := callText("profile", map[string]any{}); !strings.Contains(out, `"connected":true`) || !strings.Contains(out, "profiles/second") {
		t.Fatalf("profile list: %s", out)
	}
	callText("profile", map[string]any{"action": "use", "name": "default", "quit": true})

	// batch: several actions, one call, one final snapshot.
	page2 := "data:text/html," + url.PathEscape(`<title>batch</title><input aria-label="First"><input aria-label="Last"><select aria-label="Size"><option>S</option><option>M</option></select><button onclick="setTimeout(()=>out.textContent='saved '+[...document.querySelectorAll('input,select')].map(e=>e.value).join(' '),300)">Save</button><p id=out></p>`)
	snap = callText("navigate", map[string]any{"url": page2})
	out := callText("batch", map[string]any{"steps": []any{
		map[string]any{"tool": "type", "args": map[string]any{"ref": ref(snap, `"First"`), "text": "Ada"}},
		map[string]any{"tool": "type", "args": map[string]any{"ref": ref(snap, `"Last"`), "text": "Lovelace"}},
		map[string]any{"tool": "select", "args": map[string]any{"ref": ref(snap, `"Size"`), "values": []string{"M"}}},
		map[string]any{"tool": "click", "args": map[string]any{"ref": ref(snap, `"Save"`)}},
		map[string]any{"tool": "wait", "args": map[string]any{"text": "saved"}},
		map[string]any{"tool": "eval", "args": map[string]any{"js": "out.textContent"}},
	}})
	if !strings.Contains(out, "6. eval") || !strings.Contains(out, "saved Ada Lovelace M") || strings.Count(out, `button "Save"`) != 1 {
		t.Fatalf("batch:\n%s", out)
	}
	// A failing step stops the batch and is a tool error; later steps do not run.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "batch", Arguments: map[string]any{"steps": []any{
		map[string]any{"tool": "click", "args": map[string]any{"ref": "e9999"}},
		map[string]any{"tool": "type", "args": map[string]any{"ref": ref(snap, `"First"`), "text": "never"}},
	}}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "1 remaining step(s) not run") {
		t.Fatalf("batch failure: %v %+v", err, res)
	}
	// Invalid steps are rejected before anything runs.
	res, _ = cs.CallTool(ctx, &mcp.CallToolParams{Name: "batch", Arguments: map[string]any{"steps": []any{
		map[string]any{"tool": "type", "args": map[string]any{"ref": ref(snap, `"First"`), "text": "never"}},
		map[string]any{"tool": "teleport"},
	}}})
	if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "nothing was executed") {
		t.Fatalf("batch validation: %+v", res)
	}
	if v := callText("eval", map[string]any{"js": "document.querySelector('input').value"}); strings.Contains(v, "never") {
		t.Fatalf("a rejected batch typed: %s", v)
	}

	// Errors come back as tool errors, not protocol failures.
	res, err = cs.CallTool(ctx, &mcp.CallToolParams{Name: "click", Arguments: map[string]any{"ref": "e9999"}})
	if err != nil || !res.IsError {
		t.Fatalf("stale ref should be a tool error: %v %+v", err, res)
	}
}
