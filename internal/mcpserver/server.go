// Package mcpserver exposes brojev to Claude Code over MCP (stdio). It is a
// thin layer: every tool forwards to brojevd, which owns the browser.
package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/barockok/brojev/internal/daemon"
)

const Version = "0.1.0"

// Run serves MCP on stdin/stdout until the client disconnects.
func Run(ctx context.Context) error {
	return New().Run(ctx, &mcp.StdioTransport{})
}

func New() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "brojev", Version: Version}, &mcp.ServerOptions{Instructions: instructions})
	register(s)
	return s
}

const instructions = `brojev drives a dedicated Chrome profile (logins persist there).
Fast path: call run with a concrete goal (and values you already know); it executes the whole sub-task in a few seconds and returns the final page.
Use the primitive tools (snapshot, click, type, ...) when run returns needs_claude/blocked, for canvas or unusual widgets, or for exact control.
Refs like e12 / f2e5 come from the latest snapshot; take a new one if a ref is stale.`

type empty struct{}

func call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	if err := daemon.Ensure(ctx); err != nil {
		return nil, err
	}
	var out json.RawMessage
	err := daemon.Call(ctx, method, params, &out)
	return out, err
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func fail(err error) *mcp.CallToolResult {
	r := text("error: " + err.Error())
	r.IsError = true
	return r
}

// action forwards an input-producing tool and renders its reply.
func action[In any](method string) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		params, err := toParams(in)
		if err != nil {
			return fail(err), nil, nil
		}
		if _, set := params["snapshot"]; !set {
			params["snapshot"] = true
		}
		raw, err := call(ctx, method, params)
		if err != nil {
			return fail(err), nil, nil
		}
		return text(renderAction(method, raw)), nil, nil
	}
}

// plain forwards a tool and returns its JSON reply as text.
func plain[In any](method string) mcp.ToolHandlerFor[In, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		params, err := toParams(in)
		if err != nil {
			return fail(err), nil, nil
		}
		raw, err := call(ctx, method, params)
		if err != nil {
			return fail(err), nil, nil
		}
		return text(string(raw)), nil, nil
	}
}

func toParams(in any) (map[string]any, error) {
	b, err := json.Marshal(in)
	if err != nil {
		return nil, err
	}
	m := map[string]any{}
	return m, json.Unmarshal(b, &m)
}

func renderAction(method string, raw json.RawMessage) string {
	var r struct {
		Navigated bool   `json:"navigated"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		Note      string `json:"note"`
		MS        int64  `json:"ms"`
		Dialog    *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"dialog"`
		NewTabs []struct {
			TargetID string `json:"targetId"`
			URL      string `json:"url"`
			Title    string `json:"title"`
		} `json:"newTabs"`
		Snapshot string `json:"snapshot"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return string(raw)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "ok: %s (%d ms)", method, r.MS)
	if r.Navigated {
		fmt.Fprintf(&b, ", navigated to %s", r.URL)
	}
	b.WriteByte('\n')
	if r.Dialog != nil {
		fmt.Fprintf(&b, "DIALOG OPEN (%s): %q. Call dialog with accept=true or false next.\n", r.Dialog.Type, r.Dialog.Message)
	}
	for _, t := range r.NewTabs {
		fmt.Fprintf(&b, "new tab opened: %s %q %s (tabs action=switch targetId=%s to drive it)\n", t.TargetID, t.Title, t.URL, t.TargetID)
	}
	if r.Note != "" {
		fmt.Fprintf(&b, "note: %s\n", r.Note)
	}
	if r.Snapshot != "" {
		b.WriteString(r.Snapshot)
	}
	return b.String()
}

// ---- inputs ----

type RunIn struct {
	Goal          string            `json:"goal" jsonschema:"one concrete sub-task in natural language, e.g. 'Search flights Zurich to London one-way on 20 November 2026 and stop when results show'"`
	URL           string            `json:"url,omitempty" jsonschema:"start URL; omit to continue on the current page"`
	Values        map[string]string `json:"values,omitempty" jsonschema:"field values you already know, e.g. {\"origin\":\"Zurich\",\"date\":\"2026-11-20\"}; makes typing exact and faster"`
	Files         []string          `json:"files,omitempty" jsonschema:"absolute paths of files to upload if the goal needs it"`
	Expect        string            `json:"expect,omitempty" jsonschema:"text that must be visible when done; DONE without it is reported as needs_claude"`
	MaxSteps      int               `json:"maxSteps,omitempty" jsonschema:"max executed actions (default 40)"`
	MinConfidence float64           `json:"minConfidence,omitempty" jsonschema:"stop and hand back when decision confidence stays below this (default 0.2)"`
	Answer        bool              `json:"answer,omitempty" jsonschema:"also extract the requested information as text (slower; the final snapshot is always returned)"`
}

type SnapshotIn struct {
	MaxElements  int `json:"maxElements,omitempty" jsonschema:"cap on listed elements (default 600)"`
	MaxOffscreen int `json:"maxOffscreen,omitempty" jsonschema:"cap on offscreen elements (default 150)"`
	MaxText      int `json:"maxText,omitempty" jsonschema:"cap on visible text characters (default 6000, -1 for none)"`
}

type NavigateIn struct {
	URL string `json:"url" jsonschema:"URL to load in the current tab"`
}

type ClickIn struct {
	Ref       string   `json:"ref,omitempty" jsonschema:"element ref from the snapshot, e.g. e12 or f2e5"`
	X         *float64 `json:"x,omitempty" jsonschema:"viewport x (CSS px) when there is no ref, e.g. canvas"`
	Y         *float64 `json:"y,omitempty" jsonschema:"viewport y (CSS px)"`
	Button    string   `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	Count     int      `json:"count,omitempty" jsonschema:"2 for double click"`
	Modifiers []string `json:"modifiers,omitempty" jsonschema:"keys held during the click: Shift, Alt, Control, Meta, ControlOrMeta"`
	Snapshot  *bool    `json:"snapshot,omitempty" jsonschema:"return a fresh snapshot (default true)"`
}

type TypeIn struct {
	Ref      string `json:"ref" jsonschema:"text field ref"`
	Text     string `json:"text" jsonschema:"text to enter; replaces the field content unless append is set. Date/time/color inputs take their native format, e.g. 2026-11-20"`
	Append   bool   `json:"append,omitempty" jsonschema:"keep existing content"`
	Submit   bool   `json:"submit,omitempty" jsonschema:"press Enter afterwards"`
	Snapshot *bool  `json:"snapshot,omitempty" jsonschema:"return a fresh snapshot (default true)"`
}

type SelectIn struct {
	Ref      string   `json:"ref" jsonschema:"native <select> ref"`
	Values   []string `json:"values" jsonschema:"option values or labels"`
	Snapshot *bool    `json:"snapshot,omitempty"`
}

type RefIn struct {
	Ref      string `json:"ref" jsonschema:"element ref"`
	Snapshot *bool  `json:"snapshot,omitempty"`
}

type KeyIn struct {
	Combo    string `json:"combo,omitempty" jsonschema:"key or combo for the focused element, e.g. Enter, Escape, Tab, ArrowDown, Shift+Tab, ControlOrMeta+A"`
	Text     string `json:"text,omitempty" jsonschema:"instead of combo: type this text key by key into the focused element"`
	Snapshot *bool  `json:"snapshot,omitempty"`
}

type ScrollIn struct {
	Ref      string   `json:"ref,omitempty" jsonschema:"scroll inside this element (nested scroller); omit for the page"`
	X        *float64 `json:"x,omitempty"`
	Y        *float64 `json:"y,omitempty"`
	DX       float64  `json:"dx,omitempty" jsonschema:"horizontal pixels"`
	DY       float64  `json:"dy,omitempty" jsonschema:"vertical pixels, negative scrolls up (default 600)"`
	Snapshot *bool    `json:"snapshot,omitempty"`
}

type DragIn struct {
	From     string   `json:"from,omitempty" jsonschema:"ref to drag"`
	To       string   `json:"to,omitempty" jsonschema:"ref to drop onto"`
	X        *float64 `json:"x,omitempty" jsonschema:"start x when not using refs"`
	Y        *float64 `json:"y,omitempty"`
	X2       *float64 `json:"x2,omitempty" jsonschema:"end x"`
	Y2       *float64 `json:"y2,omitempty"`
	Snapshot *bool    `json:"snapshot,omitempty"`
}

type UploadIn struct {
	Ref      string   `json:"ref" jsonschema:"file input, or a button that opens a file chooser"`
	Files    []string `json:"files" jsonschema:"absolute file paths"`
	Snapshot *bool    `json:"snapshot,omitempty"`
}

type DialogIn struct {
	Accept *bool  `json:"accept,omitempty" jsonschema:"true = OK, false = Cancel; omit to just read the open dialog"`
	Text   string `json:"text,omitempty" jsonschema:"answer for a prompt() dialog"`
}

type WaitIn struct {
	Text      string `json:"text" jsonschema:"text to wait for (any frame)"`
	Gone      bool   `json:"gone,omitempty" jsonschema:"wait for the text to disappear instead"`
	TimeoutMS int    `json:"timeoutMs,omitempty" jsonschema:"default 10000"`
}

type EvalIn struct {
	JS string `json:"js" jsonschema:"JavaScript expression evaluated in the page (promises are awaited); returns JSON"`
}

type TabsIn struct {
	Action   string `json:"action,omitempty" jsonschema:"list (default), new, switch, close"`
	TargetID string `json:"targetId,omitempty" jsonschema:"tab id for switch/close (close defaults to the current tab)"`
	URL      string `json:"url,omitempty" jsonschema:"URL for new"`
}

type HistoryIn struct {
	Action   string `json:"action" jsonschema:"back, forward or reload"`
	Snapshot *bool  `json:"snapshot,omitempty"`
}

type ScreenshotIn struct {
	Full bool `json:"full,omitempty" jsonschema:"capture the full page instead of the viewport"`
}

type EmulateIn struct {
	Device      string   `json:"device,omitempty" jsonschema:"preset: iphone-15, pixel-8, ipad, desktop, desktop-hd"`
	Reset       bool     `json:"reset,omitempty" jsonschema:"clear all overrides first"`
	Width       int      `json:"width,omitempty"`
	Height      int      `json:"height,omitempty"`
	Scale       float64  `json:"scale,omitempty" jsonschema:"device pixel ratio"`
	Mobile      bool     `json:"mobile,omitempty"`
	Touch       bool     `json:"touch,omitempty"`
	Latitude    *float64 `json:"latitude,omitempty"`
	Longitude   *float64 `json:"longitude,omitempty"`
	Timezone    string   `json:"timezone,omitempty" jsonschema:"IANA zone, e.g. Europe/Zurich"`
	Locale      string   `json:"locale,omitempty" jsonschema:"e.g. de-CH"`
	ColorScheme string   `json:"colorScheme,omitempty" jsonschema:"light or dark"`
	Offline     *bool    `json:"offline,omitempty"`
	Permissions []string `json:"permissions,omitempty" jsonschema:"pre-grant for the current origin: geolocation, notifications, clipboardReadWrite, camera, microphone"`
}

type ProfileIn struct {
	Action        string `json:"action,omitempty" jsonschema:"list (default), current, chrome-profiles, add, use, remove"`
	Name          string `json:"name,omitempty" jsonschema:"profile name; built-ins: default (dedicated ~/.brojev/profile), chrome (the user's real Chrome, attach mode)"`
	Dir           string `json:"dir,omitempty" jsonschema:"add: Chrome user data dir (default ~/.brojev/profiles/<name>); chrome-profiles: dir to inspect"`
	ChromeProfile string `json:"chromeProfile,omitempty" jsonschema:"add: person inside the data dir, e.g. Default or Profile 3 (list them with action=chrome-profiles)"`
	Attach        bool   `json:"attach,omitempty" jsonschema:"add: connect via chrome://inspect remote debugging (needed for the real Chrome dir; avoids navigator.webdriver)"`
	Quit          bool   `json:"quit,omitempty" jsonschema:"use: also close the previous profile's Chrome"`
}

type StateIn struct {
	Action string `json:"action" jsonschema:"save or load"`
	Path   string `json:"path,omitempty" jsonschema:"JSON file (default ~/.brojev/state.json)"`
}

func register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{Name: "run", Description: "Fast path: complete one browser sub-task autonomously (Jev decides each step in ~200 ms). " +
		"Returns status (done, blocked, needs_claude, budget, error), the steps taken, and the final page snapshot. " +
		"Give a concrete goal and any values you already know. On needs_claude/blocked, continue with the primitive tools.",
	}, runTool)
	mcp.AddTool(s, &mcp.Tool{Name: "snapshot", Description: "List the current page's actionable elements (refs like e12, f2e5 for iframes) and visible text. In-view elements first, then offscreen ones (they can still be used directly)."}, snapshotTool)
	mcp.AddTool(s, &mcp.Tool{Name: "navigate", Description: "Load a URL in the current tab and return the new snapshot."}, action[NavigateIn]("navigate"))
	mcp.AddTool(s, &mcp.Tool{Name: "click", Description: "Click an element by ref (scrolls into view, waits until stable, refuses if covered) or at viewport coordinates. Supports right/double/modifier clicks."}, action[ClickIn]("click"))
	mcp.AddTool(s, &mcp.Tool{Name: "type", Description: "Focus a text field and enter text (replaces content by default). Works for inputs, textareas, contenteditable editors and native date/time inputs."}, action[TypeIn]("type"))
	mcp.AddTool(s, &mcp.Tool{Name: "select", Description: "Choose options of a native <select> by value or label. For custom dropdowns, click instead."}, action[SelectIn]("select"))
	mcp.AddTool(s, &mcp.Tool{Name: "hover", Description: "Move the mouse over an element (reveals hover menus and tooltips)."}, action[RefIn]("hover"))
	mcp.AddTool(s, &mcp.Tool{Name: "key", Description: "Press a key combo on the focused element (Enter, Escape, Tab, arrows, ControlOrMeta+A...), or type text key by key."}, keyTool)
	mcp.AddTool(s, &mcp.Tool{Name: "scroll", Description: "Wheel-scroll the page or a nested scroll container."}, action[ScrollIn]("scroll"))
	mcp.AddTool(s, &mcp.Tool{Name: "drag", Description: "Drag from one element to another (HTML5 drag-and-drop and pointer-based), or between coordinates."}, action[DragIn]("drag"))
	mcp.AddTool(s, &mcp.Tool{Name: "upload", Description: "Attach files to a file input, or to a button that opens a file chooser (the OS dialog is never shown)."}, action[UploadIn]("upload"))
	mcp.AddTool(s, &mcp.Tool{Name: "dialog", Description: "Read or handle an open alert/confirm/prompt/beforeunload dialog. While one is open, other page actions fail."}, dialogTool)
	mcp.AddTool(s, &mcp.Tool{Name: "history", Description: "Go back, forward, or reload the current tab."}, historyTool)
	mcp.AddTool(s, &mcp.Tool{Name: "wait", Description: "Wait until text appears (or disappears) in the page, including iframes."}, plain[WaitIn]("wait"))
	mcp.AddTool(s, &mcp.Tool{Name: "screenshot", Description: "Capture the viewport (or full page) as an image, for canvas, maps, charts or visual checks."}, screenshotTool)
	mcp.AddTool(s, &mcp.Tool{Name: "eval", Description: "Evaluate JavaScript in the page's main world and return the JSON result. Use sparingly; prefer the other tools."}, plain[EvalIn]("eval"))
	mcp.AddTool(s, &mcp.Tool{Name: "tabs", Description: "List, open, switch to, or close tabs. New tabs opened by the page are reported after actions."}, tabsTool)
	mcp.AddTool(s, &mcp.Tool{Name: "downloads", Description: "List downloads (saved to ~/.brojev/downloads under their real names) with state and path."}, plain[empty]("downloads"))
	mcp.AddTool(s, &mcp.Tool{Name: "emulate", Description: "Emulate a device or environment: viewport/mobile/touch, geolocation, timezone, locale, color scheme, offline, and pre-granted permissions."}, emulateTool)
	mcp.AddTool(s, &mcp.Tool{Name: "profile", Description: "List, inspect, add or switch the Chrome profile brojev drives. Each profile keeps its own logins. " +
		"Switching only with the user's consent: the real Chrome profile exposes their personal sessions and sends page text to the Jev API."}, plain[ProfileIn]("profile"))
	mcp.AddTool(s, &mcp.Tool{Name: "state", Description: "Save or load login state (all cookies plus localStorage of the current origin) to a JSON file."}, stateTool)
}

func runTool(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, any, error) {
	if strings.TrimSpace(in.Goal) == "" {
		return fail(fmt.Errorf("goal required")), nil, nil
	}
	raw, err := call(ctx, "run", in)
	if err != nil {
		return fail(err), nil, nil
	}
	var o struct {
		Status   string  `json:"status"`
		Reason   string  `json:"reason"`
		Answer   string  `json:"answer"`
		Verified *bool   `json:"verified"`
		URL      string  `json:"url"`
		Title    string  `json:"title"`
		Snapshot string  `json:"snapshot"`
		TotalMS  int64   `json:"totalMs"`
		JevCalls int     `json:"jevCalls"`
		CostUSD  float64 `json:"costUsd"`
		Steps    []struct {
			Op     string  `json:"op"`
			Target string  `json:"target"`
			Text   string  `json:"text"`
			Conf   float64 `json:"confidence"`
			Error  string  `json:"error"`
			Note   string  `json:"note"`
		} `json:"steps"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return text(string(raw)), nil, nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "status: %s", o.Status)
	if o.Reason != "" {
		fmt.Fprintf(&b, " (%s)", o.Reason)
	}
	if o.Verified != nil {
		fmt.Fprintf(&b, ", expected text visible: %v", *o.Verified)
	}
	fmt.Fprintf(&b, "\n%d steps in %d ms, %d Jev calls, $%.5f\n", len(o.Steps), o.TotalMS, o.JevCalls, o.CostUSD)
	for i, s := range o.Steps {
		fmt.Fprintf(&b, "%d. %s %s", i+1, s.Op, s.Target)
		if s.Text != "" {
			fmt.Fprintf(&b, " <- %q", s.Text)
		}
		if s.Error != "" {
			fmt.Fprintf(&b, " FAILED: %s", s.Error)
		}
		if s.Note != "" {
			fmt.Fprintf(&b, " [%s]", s.Note)
		}
		b.WriteByte('\n')
	}
	if o.Answer != "" {
		fmt.Fprintf(&b, "answer: %s\n", o.Answer)
	}
	if o.Status == "done" {
		b.WriteString("Verify the outcome from the page below; DONE is Jev's judgment, not proof.\n")
	}
	b.WriteString("--- final page ---\n")
	b.WriteString(o.Snapshot)
	return text(b.String()), nil, nil
}

func snapshotTool(ctx context.Context, _ *mcp.CallToolRequest, in SnapshotIn) (*mcp.CallToolResult, any, error) {
	raw, err := call(ctx, "snapshot", in)
	if err != nil {
		return fail(err), nil, nil
	}
	var s struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &s) != nil {
		return text(string(raw)), nil, nil
	}
	return text(s.Text), nil, nil
}

func keyTool(ctx context.Context, req *mcp.CallToolRequest, in KeyIn) (*mcp.CallToolResult, any, error) {
	if in.Combo == "" && in.Text == "" {
		return fail(fmt.Errorf("combo or text required")), nil, nil
	}
	if in.Combo != "" {
		return action[KeyIn]("key")(ctx, req, in)
	}
	return action[KeyIn]("keytype")(ctx, req, in)
}

func dialogTool(ctx context.Context, req *mcp.CallToolRequest, in DialogIn) (*mcp.CallToolResult, any, error) {
	if in.Accept == nil {
		return plain[DialogIn]("dialog")(ctx, req, in)
	}
	return action[DialogIn]("dialog")(ctx, req, in)
}

func historyTool(ctx context.Context, req *mcp.CallToolRequest, in HistoryIn) (*mcp.CallToolResult, any, error) {
	switch in.Action {
	case "back", "forward", "reload":
		return action[HistoryIn](in.Action)(ctx, req, in)
	}
	return fail(fmt.Errorf("action must be back, forward or reload")), nil, nil
}

func tabsTool(ctx context.Context, req *mcp.CallToolRequest, in TabsIn) (*mcp.CallToolResult, any, error) {
	m := map[string]string{"": "tabs", "list": "tabs", "new": "tab_new", "switch": "tab_switch", "close": "tab_close"}
	method, ok := m[in.Action]
	if !ok {
		return fail(fmt.Errorf("action must be list, new, switch or close")), nil, nil
	}
	return plain[TabsIn](method)(ctx, req, in)
}

func screenshotTool(ctx context.Context, _ *mcp.CallToolRequest, in ScreenshotIn) (*mcp.CallToolResult, any, error) {
	raw, err := call(ctx, "screenshot", in)
	if err != nil {
		return fail(err), nil, nil
	}
	var s struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return fail(err), nil, nil
	}
	img, err := os.ReadFile(s.Path)
	if err != nil {
		return fail(err), nil, nil
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: img, MIMEType: "image/jpeg"},
		&mcp.TextContent{Text: "saved: " + s.Path},
	}}, nil, nil
}

func emulateTool(ctx context.Context, req *mcp.CallToolRequest, in EmulateIn) (*mcp.CallToolResult, any, error) {
	if len(in.Permissions) > 0 {
		if _, err := call(ctx, "grant", map[string]any{"permissions": in.Permissions}); err != nil {
			return fail(err), nil, nil
		}
	}
	return plain[EmulateIn]("emulate")(ctx, req, in)
}

func stateTool(ctx context.Context, req *mcp.CallToolRequest, in StateIn) (*mcp.CallToolResult, any, error) {
	switch in.Action {
	case "save":
		return plain[StateIn]("state_save")(ctx, req, in)
	case "load":
		return plain[StateIn]("state_load")(ctx, req, in)
	}
	return fail(fmt.Errorf("action must be save or load")), nil, nil
}
