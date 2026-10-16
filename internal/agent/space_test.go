package agent

import (
	"fmt"
	"testing"

	"github.com/barockok/brojev/internal/page"
)

func manyElements(n int) *page.Snapshot {
	s := &page.Snapshot{URL: "https://mail.example/", Title: "Inbox", Viewport: [2]float64{1000, 800}, Scroll: [4]float64{0, 0, 0, 800}}
	for i := 0; i < n; i++ {
		s.Elements = append(s.Elements, page.Element{Ref: fmt.Sprintf("e%d", i+1), Role: "checkbox", Name: fmt.Sprintf("row %d", i), Tag: "div", InView: i < n-30})
	}
	s.Elements = append(s.Elements,
		page.Element{Ref: "e9001", Role: "button", Name: "Send", Tag: "div", InView: false},
		page.Element{Ref: "e9002", Role: "textbox", Name: "Subject", Tag: "input", Type: "text", InView: false},
	)
	return s
}

// Jev rejects questions with more than 255 criteria (HTTP 422). A page with
// 300+ elements must be trimmed, keeping goal-relevant ones even offscreen.
func TestSpaceStaysUnderCriteriaLimit(t *testing.T) {
	task := Task{Goal: "Fill the subject and click Send", Values: map[string]string{"subject": "hi", "to": "a@b.c", "body": "x"}}
	sp := buildSpace(manyElements(400), task, true, maxOptions, window{})
	for op, h := range sp.heads {
		if len(h.options) > 255 || len(h.criteria) > 255 {
			t.Fatalf("%s head has %d options, %d criteria", op, len(h.options), len(h.criteria))
		}
	}
	if len(sp.table) > maxOptions {
		t.Fatalf("table has %d rows", len(sp.table))
	}
	var send, subject bool
	for _, o := range sp.heads["CLICK"].options {
		send = send || o.ref == "e9001"
	}
	for _, o := range sp.heads["TYPE_TEXT"].options {
		subject = subject || o.ref == "e9002"
	}
	if !send || !subject {
		t.Fatalf("goal-relevant offscreen elements were trimmed: send=%v subject=%v", send, subject)
	}
}

func TestSmallPagesKeepDomOrder(t *testing.T) {
	s := manyElements(5)
	sp := buildSpace(s, Task{Goal: "x"}, false, maxOptions, window{})
	if len(sp.table) != len(s.Elements) || sp.table[0]["label"] != "[1] checkbox \"row 0\"" {
		t.Fatalf("small page was reordered or trimmed: %v", sp.table[0])
	}
}

func TestLooksUnrendered(t *testing.T) {
	if !looksUnrendered(&page.Snapshot{Text: "If you're having trouble loading, visit the help center", Elements: make([]page.Element, 18)}) {
		t.Fatal("loading text on a tiny page should look unrendered")
	}
	if !looksUnrendered(&page.Snapshot{Elements: make([]page.Element, 2)}) {
		t.Fatal("two elements should look unrendered")
	}
	if looksUnrendered(&page.Snapshot{Text: "Inbox", Elements: make([]page.Element, 50)}) {
		t.Fatal("a populated page is rendered")
	}
}

func TestGoalKeys(t *testing.T) {
	for goal, want := range map[string][]string{
		"Press the letter A key in the input field.": {"A"},
		"hit F5 then press PageDown":                 {"F5", "PageDown"},
		"Click the button":                           nil,
	} {
		got := goalKeys(goal)
		if len(got) != len(want) {
			t.Fatalf("%q -> %v, want %v", goal, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q -> %v, want %v", goal, got, want)
			}
		}
	}
}

func TestQuoted(t *testing.T) {
	if got := quoted(`Add 'buy milk' and "write code" about Gödel's theorems`); len(got) != 2 || got[0] != "buy milk" || got[1] != "write code" {
		t.Fatalf("got %v", got)
	}
}

func TestPromptDialogOffersValues(t *testing.T) {
	s := &page.Snapshot{Dialog: &page.Dialog{Type: "prompt", Message: "enter"}}
	sp := buildSpace(s, Task{Goal: "answer the prompt", Values: map[string]string{"text": "hi"}}, false, maxOptions, window{})
	if sp.heads["DIALOG_PROMPT"] == nil || len(sp.heads["DIALOG_PROMPT"].options) != 1 {
		t.Fatalf("heads: %v ops: %v", sp.heads, sp.ops)
	}
	alert := buildSpace(&page.Snapshot{Dialog: &page.Dialog{Type: "alert"}}, Task{Goal: "x", Values: map[string]string{"t": "v"}}, false, maxOptions, window{})
	if alert.heads["DIALOG_PROMPT"] != nil {
		t.Fatal("prompt text offered for a non-prompt dialog")
	}
}

func bigPage() *page.Snapshot {
	s := &page.Snapshot{URL: "https://mail.example/", Title: "Inbox", Viewport: [2]float64{1000, 800}, Scroll: [4]float64{0, 0, 0, 5000}}
	for i := 0; i < 600; i++ {
		name := fmt.Sprintf("item %d", i)
		if i >= 586 && i <= 590 {
			name = fmt.Sprintf("Archive thread %d", i) // e587..e591; e591 is the target
		}
		s.Elements = append(s.Elements, page.Element{Ref: fmt.Sprintf("e%d", i+1), Role: "button", Name: name, Tag: "button", InView: i < 100})
	}
	return s
}

func offers(sp *space, ref string) bool {
	for _, o := range sp.heads["CLICK"].options {
		if o.ref == ref {
			return true
		}
	}
	return false
}

// The target is far below the fold and shares no word with the goal: it is
// not offered by default, but both FIND and MORE_OPTIONS reach it.
func TestTailReachableByFindAndMore(t *testing.T) {
	task := Task{Goal: "Open the old conversation"}
	s := bigPage()

	def := buildSpace(s, task, false, maxOptions, window{})
	if offers(def, "e591") {
		t.Fatal("target should be outside the default view")
	}
	if def.ops["FIND"] == "" || def.ops["MORE_OPTIONS"] == "" {
		t.Fatalf("navigation ops missing: %v", def.ops)
	}
	phrase := ""
	for _, o := range def.heads["FIND"].options {
		if o.value == "archive" {
			phrase = o.value
		}
	}
	if phrase == "" {
		t.Fatalf("FIND does not offer the recurring word of the hidden group: %v", def.heads["FIND"].criteria)
	}

	found := buildSpace(s, task, false, maxOptions, window{query: phrase})
	if !offers(found, "e591") || len(found.heads["CLICK"].options) != 5 {
		t.Fatalf("FIND view: %d options, target offered=%v", len(found.heads["CLICK"].options), offers(found, "e591"))
	}
	if found.ops["FIND"] != "" {
		t.Fatal("FIND must not be offered inside a phrase view")
	}

	reached := false
	for pg := 1; pg < def.view.pages; pg++ {
		if offers(buildSpace(s, task, false, maxOptions, window{page: pg}), "e591") {
			reached = true
		}
	}
	if !reached {
		t.Fatalf("no MORE_OPTIONS group contains the target (pages=%d)", def.view.pages)
	}
	last := buildSpace(s, task, false, maxOptions, window{page: def.view.pages - 1})
	if last.ops["MORE_OPTIONS"] != "" {
		t.Fatal("MORE_OPTIONS offered on the last group")
	}
	for pg := 0; pg < def.view.pages; pg++ {
		sp := buildSpace(s, task, false, maxOptions, window{page: pg})
		for op, h := range sp.heads {
			if len(h.options) > 255 {
				t.Fatalf("page %d %s has %d options", pg, op, len(h.options))
			}
		}
	}
}

func TestSmallPageHasNoNavigationOps(t *testing.T) {
	sp := buildSpace(manyElements(5), Task{Goal: "x"}, false, maxOptions, window{})
	if sp.ops["FIND"] != "" || sp.ops["MORE_OPTIONS"] != "" {
		t.Fatalf("ops: %v", sp.ops)
	}
}

func TestFindCandidatesTakeValidatedHints(t *testing.T) {
	hidden := []page.Element{
		{Ref: "e1", Name: "Quarterly report"}, {Ref: "e2", Name: "Invoice 2026"}, {Ref: "e3", Name: "Invoice 2025"},
	}
	crit, opts := findCandidates(Task{Goal: "Open the bill"}, hidden, []string{"invoice", "nonexistent phrase"})
	var got []string
	for _, o := range opts {
		got = append(got, o.value)
	}
	if len(opts) != 1 || got[0] != "invoice" {
		t.Fatalf("want only the validated hint, got %v", got)
	}
	for k := range opts {
		if n := crit[k].(map[string]any)["matching_elements_not_offered"]; n != 2 {
			t.Fatalf("match count %v", n)
		}
	}
}

func TestUsedCanvasCellsAreNotOfferedAgain(t *testing.T) {
	canvas := page.Element{Ref: "e1", Role: "canvas", Tag: "canvas", InView: true, Rect: [4]float64{0, 0, 1200, 750}}
	s := &page.Snapshot{Viewport: [2]float64{1280, 800}, Elements: []page.Element{canvas}}
	used := usedCells([]hist{{Kind: "CANVAS_DRAG", Action: "drag on canvas r2c2 to r4c3"}})
	for _, k := range []string{"r2c2", "r3c2", "r4c3", "r3c3"} {
		if !used[k] {
			t.Fatalf("%s should be used: %v", k, used)
		}
	}
	sp := buildSpace(s, Task{Goal: "draw"}, false, maxOptions, window{used: used})
	for k := range sp.heads["CANVAS_DRAG"].options {
		if used[k] {
			t.Fatalf("used cell %s offered", k)
		}
	}
	if len(sp.heads["CANVAS_DRAG"].options) != 30-6 {
		t.Fatalf("free cells: %d", len(sp.heads["CANVAS_DRAG"].options))
	}
}
