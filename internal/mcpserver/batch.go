package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxBatchSteps = 50

type BatchStep struct {
	Tool string         `json:"tool" jsonschema:"navigate, click, type, select, hover, key, scroll, drag, upload, dialog, history, wait, eval or tabs"`
	Args map[string]any `json:"args,omitempty" jsonschema:"the arguments of the tool of that name, e.g. {\"ref\":\"e3\",\"text\":\"Zurich\"}"`
}

type BatchIn struct {
	Steps           []BatchStep `json:"steps" jsonschema:"actions to run in order (max 50)"`
	ContinueOnError bool        `json:"continueOnError,omitempty" jsonschema:"keep going after a failed step (default: stop at the first failure)"`
	Snapshot        *bool       `json:"snapshot,omitempty" jsonschema:"return a fresh snapshot after the last step (default true)"`
}

// stepMethod maps a batch step to its daemon method, as the single tools do.
// plain is true for tools that return data rather than an action result.
func stepMethod(s BatchStep) (method string, plain bool, err error) {
	str := func(k string) string { v, _ := s.Args[k].(string); return v }
	switch s.Tool {
	case "navigate", "click", "type", "select", "hover", "scroll", "drag", "upload":
		return s.Tool, false, nil
	case "key":
		if str("combo") != "" {
			return "key", false, nil
		}
		if str("text") != "" {
			return "keytype", false, nil
		}
		return "", false, fmt.Errorf("key: combo or text required")
	case "dialog":
		_, accept := s.Args["accept"]
		return "dialog", !accept, nil
	case "history":
		switch a := str("action"); a {
		case "back", "forward", "reload":
			return a, false, nil
		}
		return "", false, fmt.Errorf("history: action must be back, forward or reload")
	case "wait", "eval":
		return s.Tool, true, nil
	case "tabs":
		m := map[string]string{"": "tabs", "list": "tabs", "new": "tab_new", "switch": "tab_switch", "close": "tab_close"}
		if method, ok := m[str("action")]; ok {
			return method, true, nil
		}
		return "", false, fmt.Errorf("tabs: action must be list, new, switch or close")
	}
	return "", false, fmt.Errorf("unknown tool %q (batch takes navigate, click, type, select, hover, key, scroll, drag, upload, dialog, history, wait, eval, tabs)", s.Tool)
}

// batchTool runs several actions in one call: no snapshot between steps
// (refs from the last snapshot stay valid while their elements exist), one
// line per step, and one snapshot at the end.
func batchTool(ctx context.Context, _ *mcp.CallToolRequest, in BatchIn) (*mcp.CallToolResult, any, error) {
	if len(in.Steps) == 0 {
		return fail(fmt.Errorf("steps required")), nil, nil
	}
	if len(in.Steps) > maxBatchSteps {
		return fail(fmt.Errorf("at most %d steps", maxBatchSteps)), nil, nil
	}
	// Validate everything before acting, so a typo cannot leave a half-done form.
	methods := make([]string, len(in.Steps))
	plains := make([]bool, len(in.Steps))
	for i, s := range in.Steps {
		m, p, err := stepMethod(s)
		if err != nil {
			return fail(fmt.Errorf("step %d: %w; nothing was executed", i+1, err)), nil, nil
		}
		methods[i], plains[i] = m, p
	}

	var b strings.Builder
	failed := 0
	for i, s := range in.Steps {
		params := map[string]any{}
		for k, v := range s.Args {
			params[k] = v
		}
		if !plains[i] {
			params["snapshot"] = false
		}
		raw, err := call(ctx, methods[i], params)
		label := fmt.Sprintf("%d. %s", i+1, s.Tool)
		if ref, _ := s.Args["ref"].(string); ref != "" {
			label += " " + ref
		}
		if err != nil {
			failed++
			fmt.Fprintf(&b, "%s: FAILED: %v\n", label, err)
			if !in.ContinueOnError {
				if rest := len(in.Steps) - i - 1; rest > 0 {
					fmt.Fprintf(&b, "stopped; %d remaining step(s) not run\n", rest)
				}
				break
			}
			continue
		}
		if plains[i] {
			fmt.Fprintf(&b, "%s: %s\n", label, truncateText(string(raw), 2000))
		} else {
			// "ok: click (12 ms)\nnote: ..." -> "2. click e5 (12 ms)\n   note: ..."
			out := strings.TrimPrefix(renderAction(methods[i], raw), "ok: "+methods[i])
			out = strings.ReplaceAll(strings.TrimSuffix(out, "\n"), "\n", "\n   ")
			fmt.Fprintf(&b, "%s%s\n", label, out)
		}
	}

	if in.Snapshot == nil || *in.Snapshot {
		raw, err := call(ctx, "snapshot", map[string]any{})
		if err != nil {
			fmt.Fprintf(&b, "snapshot failed: %v\n", err)
		} else {
			var s struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(raw, &s) == nil {
				b.WriteString(s.Text)
			}
		}
	}
	r := text(b.String())
	r.IsError = failed > 0 && !in.ContinueOnError
	return r, nil, nil
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("... (%d more bytes)", len(s)-n)
}
