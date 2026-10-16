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
	sp := buildSpace(manyElements(400), task, true, maxOptions)
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
	sp := buildSpace(s, Task{Goal: "x"}, false, maxOptions)
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
