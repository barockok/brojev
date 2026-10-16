package agent

import (
	"testing"

	"github.com/barockok/brojev/internal/page"
)

func field(ref, name string) page.Element {
	return page.Element{Ref: ref, Role: "textbox", Name: name, Tag: "input", Type: "text", InView: true}
}

func snapOf(els ...page.Element) *page.Snapshot { return &page.Snapshot{Elements: els} }

func TestPrefillUnambiguous(t *testing.T) {
	s := snapOf(field("e1", "To recipients"), field("e2", "Subject"), field("e3", "Message Body"))
	got := prefills(s, map[string]string{"to": "a@b.c", "subject": "Hi", "body": "Hello"}, map[string]bool{})
	if len(got) != 3 {
		t.Fatalf("want 3 fills, got %+v", got)
	}
	want := map[string]string{"e1": "to", "e2": "subject", "e3": "body"}
	for _, f := range got {
		if want[f.el.Ref] != f.key {
			t.Fatalf("%s paired with %s", f.el.Ref, f.key)
		}
	}
}

func TestPrefillSkipsAmbiguousAndFilled(t *testing.T) {
	filled := field("e3", "Email")
	v := "x@y.z"
	filled.Value = &v
	s := snapOf(
		field("e1", "Email address"), field("e2", "Email address confirm"), // key "email" fits both
		filled,
		field("e4", "Phone"), field("e5", "Name"), // "name" fits e5 only; "phone" fits e4 only
	)
	got := prefills(s, map[string]string{"email": "a@b.c", "phone": "123", "name": "Ada"}, map[string]bool{})
	if len(got) != 2 || got[0].key != "phone" || got[1].key != "name" {
		t.Fatalf("got %+v", got)
	}
}

func TestPrefillOncePerField(t *testing.T) {
	s := snapOf(field("e1", "Name"))
	done := map[string]bool{"Name|name": true}
	if got := prefills(s, map[string]string{"name": "Ada"}, done); len(got) != 0 {
		t.Fatalf("typed twice: %+v", got)
	}
}

func TestWordsSplitsCamelAndSnake(t *testing.T) {
	for in, want := range map[string]string{"firstName": "first name", "user_name": "user name", "E-mail": "e mail"} {
		if got := join(words(in)); got != want {
			t.Fatalf("%q -> %q, want %q", in, got, want)
		}
	}
}

func join(ws []string) string {
	s := ""
	for i, w := range ws {
		if i > 0 {
			s += " "
		}
		s += w
	}
	return s
}
