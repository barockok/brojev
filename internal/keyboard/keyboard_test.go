package keyboard

import (
	"context"
	jsonv2 "github.com/chromedp/cdproto/cdp/jsonv2"
	"reflect"
	"strings"
	"testing"

	"github.com/chromedp/cdproto/input"
)

type call struct {
	Method string
	Params any
}

type fakeSession struct{ calls []call }

func (f *fakeSession) Call(_ context.Context, method string, params, _ any) error {
	f.calls = append(f.calls, call{method, params})
	return nil
}

func (f *fakeSession) Subscribe(string) (<-chan jsonv2.Value, func()) {
	return nil, func() {}
}

func (f *fakeSession) keyEvents(t *testing.T) []input.DispatchKeyEventParams {
	t.Helper()
	var out []input.DispatchKeyEventParams
	for _, c := range f.calls {
		if c.Method != input.CommandDispatchKeyEvent {
			t.Fatalf("unexpected method %q", c.Method)
		}
		out = append(out, c.Params.(input.DispatchKeyEventParams))
	}
	return out
}

func newKB(mac bool) (*Keyboard, *fakeSession) {
	f := &fakeSession{}
	k := New(f)
	k.Mac = mac
	return k, f
}

func TestModifierBits(t *testing.T) {
	if ModifierAlt != 1 || ModifierControl != 2 || ModifierMeta != 4 || ModifierShift != 8 {
		t.Fatal("modifier bitmask values do not match CDP")
	}
	k, f := newKB(false)
	ctx := context.Background()
	for _, m := range []string{"Alt", "Control", "Meta", "Shift"} {
		if err := k.Down(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	ev := f.keyEvents(t)
	want := []int64{1, 3, 7, 15}
	for i, e := range ev {
		if e.Modifiers != want[i] {
			t.Errorf("event %d (%s): modifiers=%d, want %d", i, e.Key, e.Modifiers, want[i])
		}
	}
	if k.Modifiers() != 15 {
		t.Errorf("Modifiers()=%d, want 15", k.Modifiers())
	}
}

func TestPressMetaAMac(t *testing.T) {
	k, f := newKB(true)
	if err := k.Press(context.Background(), "Meta+A"); err != nil {
		t.Fatal(err)
	}
	ev := f.keyEvents(t)
	if len(ev) != 4 {
		t.Fatalf("got %d events, want 4", len(ev))
	}
	metaDown, aDown, aUp, metaUp := ev[0], ev[1], ev[2], ev[3]

	if metaDown.Type != input.DispatchKeyEventTypeRawKeyDown || metaDown.Key != "Meta" || metaDown.Code != "MetaLeft" ||
		metaDown.Modifiers != 4 || metaDown.WindowsVirtualKeyCode != 91 || metaDown.Location != 1 {
		t.Errorf("meta down: %+v", metaDown)
	}
	// "A" is the shifted key of KeyA; with Meta held there is no text.
	if aDown.Type != input.DispatchKeyEventTypeRawKeyDown || aDown.Code != "KeyA" || aDown.Key != "A" ||
		aDown.Modifiers != 4 || aDown.Text != "" || aDown.WindowsVirtualKeyCode != 65 {
		t.Errorf("a down: %+v", aDown)
	}
	if !reflect.DeepEqual(aDown.Commands, []string{"selectAll"}) {
		t.Errorf("a down commands=%v, want [selectAll]", aDown.Commands)
	}
	if aUp.Type != input.DispatchKeyEventTypeKeyUp || aUp.Code != "KeyA" || aUp.Modifiers != 4 {
		t.Errorf("a up: %+v", aUp)
	}
	if metaUp.Type != input.DispatchKeyEventTypeKeyUp || metaUp.Key != "Meta" || metaUp.Modifiers != 0 {
		t.Errorf("meta up: %+v", metaUp)
	}
	if k.Modifiers() != 0 {
		t.Errorf("modifiers left held: %d", k.Modifiers())
	}
}

func TestPressMetaANoCommandsOffMac(t *testing.T) {
	k, f := newKB(false)
	if err := k.Press(context.Background(), "Meta+a"); err != nil {
		t.Fatal(err)
	}
	if c := f.keyEvents(t)[1].Commands; c != nil {
		t.Errorf("commands=%v, want none", c)
	}
}

func TestControlOrMeta(t *testing.T) {
	for _, tc := range []struct {
		mac  bool
		key  string
		mods int64
	}{{true, "Meta", 4}, {false, "Control", 2}} {
		k, f := newKB(tc.mac)
		if err := k.Press(context.Background(), "ControlOrMeta+C"); err != nil {
			t.Fatal(err)
		}
		ev := f.keyEvents(t)
		if ev[0].Key != tc.key || ev[1].Modifiers != tc.mods {
			t.Errorf("mac=%v: first key %q mods %d", tc.mac, ev[0].Key, ev[1].Modifiers)
		}
		wantCmds := []string(nil)
		if tc.mac {
			wantCmds = []string{"copy"}
		}
		if !reflect.DeepEqual(ev[1].Commands, wantCmds) {
			t.Errorf("mac=%v: commands=%v, want %v", tc.mac, ev[1].Commands, wantCmds)
		}
	}
}

func TestPressShiftTab(t *testing.T) {
	k, f := newKB(true)
	if err := k.Press(context.Background(), "Shift+Tab"); err != nil {
		t.Fatal(err)
	}
	ev := f.keyEvents(t)
	if len(ev) != 4 {
		t.Fatalf("got %d events", len(ev))
	}
	if ev[0].Key != "Shift" || ev[0].Modifiers != 8 || ev[0].WindowsVirtualKeyCode != 16 || ev[0].Code != "ShiftLeft" {
		t.Errorf("shift down: %+v", ev[0])
	}
	if ev[1].Key != "Tab" || ev[1].Code != "Tab" || ev[1].Modifiers != 8 || ev[1].WindowsVirtualKeyCode != 9 ||
		ev[1].Type != input.DispatchKeyEventTypeRawKeyDown {
		t.Errorf("tab down: %+v", ev[1])
	}
	if ev[1].Commands != nil {
		t.Errorf("Shift+Tab has no mac editing command, got %v", ev[1].Commands)
	}
	if ev[2].Type != input.DispatchKeyEventTypeKeyUp || ev[2].Key != "Tab" || ev[2].Modifiers != 8 {
		t.Errorf("tab up: %+v", ev[2])
	}
	if ev[3].Type != input.DispatchKeyEventTypeKeyUp || ev[3].Key != "Shift" || ev[3].Modifiers != 0 {
		t.Errorf("shift up: %+v", ev[3])
	}
}

func TestPressEnter(t *testing.T) {
	k, f := newKB(true)
	if err := k.Press(context.Background(), "Enter"); err != nil {
		t.Fatal(err)
	}
	ev := f.keyEvents(t)
	d := ev[0]
	if d.Type != input.DispatchKeyEventTypeKeyDown || d.Text != "\r" || d.UnmodifiedText != "\r" ||
		d.Key != "Enter" || d.Code != "Enter" || d.WindowsVirtualKeyCode != 13 {
		t.Errorf("enter down: %+v", d)
	}
	if d.Commands != nil {
		t.Errorf("insertNewline must be filtered, got %v", d.Commands)
	}
	if d.AutoRepeat == nil || *d.AutoRepeat || d.IsKeypad == nil || *d.IsKeypad {
		t.Errorf("autoRepeat/isKeypad should be explicit false: %+v", d)
	}
}

func TestShiftedText(t *testing.T) {
	k, f := newKB(false)
	ctx := context.Background()
	if err := k.Press(ctx, "Shift+Digit1"); err != nil {
		t.Fatal(err)
	}
	d := f.keyEvents(t)[1]
	if d.Key != "!" || d.Text != "!" || d.Type != input.DispatchKeyEventTypeKeyDown || d.Modifiers != 8 {
		t.Errorf("shift+digit1: %+v", d)
	}
}

func TestArrowDownAndKeypad(t *testing.T) {
	k, f := newKB(true)
	ctx := context.Background()
	if err := k.Press(ctx, "ArrowDown"); err != nil {
		t.Fatal(err)
	}
	if err := k.Press(ctx, "Numpad2"); err != nil {
		t.Fatal(err)
	}
	ev := f.keyEvents(t)
	if ev[0].Key != "ArrowDown" || ev[0].WindowsVirtualKeyCode != 40 || !reflect.DeepEqual(ev[0].Commands, []string{"moveDown"}) {
		t.Errorf("arrowdown: %+v", ev[0])
	}
	if ev[2].Key != "ArrowDown" || ev[2].Location != 3 || ev[2].IsKeypad == nil || !*ev[2].IsKeypad {
		t.Errorf("numpad2: %+v", ev[2])
	}
}

func TestAutoRepeat(t *testing.T) {
	k, f := newKB(false)
	ctx := context.Background()
	_ = k.Down(ctx, "a")
	_ = k.Down(ctx, "a")
	ev := f.keyEvents(t)
	if *ev[0].AutoRepeat || !*ev[1].AutoRepeat {
		t.Errorf("autoRepeat: %v %v", *ev[0].AutoRepeat, *ev[1].AutoRepeat)
	}
}

func TestPlusKey(t *testing.T) {
	k, f := newKB(false)
	if err := k.Press(context.Background(), "Shift++"); err != nil {
		t.Fatal(err)
	}
	ev := f.keyEvents(t)
	if len(ev) != 4 || ev[1].Key != "+" || ev[1].Code != "Equal" {
		t.Errorf("shift++: %+v", ev)
	}
}

func TestTypeInsertTextFallback(t *testing.T) {
	k, f := newKB(false)
	if err := k.Type(context.Background(), "aéB"); err != nil {
		t.Fatal(err)
	}
	var methods []string
	for _, c := range f.calls {
		methods = append(methods, c.Method)
	}
	want := []string{
		input.CommandDispatchKeyEvent, input.CommandDispatchKeyEvent,
		input.CommandInsertText,
		input.CommandDispatchKeyEvent, input.CommandDispatchKeyEvent,
	}
	if !reflect.DeepEqual(methods, want) {
		t.Fatalf("methods=%v", methods)
	}
	if p := f.calls[2].Params.(input.InsertTextParams); p.Text != "é" {
		t.Errorf("insertText=%q", p.Text)
	}
	// "B" resolves straight to the shifted KeyB description, without Shift held.
	if p := f.calls[3].Params.(input.DispatchKeyEventParams); p.Key != "B" || p.Text != "B" || p.Code != "KeyB" || p.Modifiers != 0 {
		t.Errorf("B down: %+v", p)
	}
}

func TestInsertText(t *testing.T) {
	k, f := newKB(false)
	if err := k.InsertText(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || f.calls[0].Method != "Input.insertText" || f.calls[0].Params.(input.InsertTextParams).Text != "hello" {
		t.Errorf("calls=%+v", f.calls)
	}
}

func TestUnknownKey(t *testing.T) {
	k, f := newKB(false)
	err := k.Press(context.Background(), "Shift+NoSuchKey")
	if err == nil || !strings.Contains(err.Error(), `unknown key: "NoSuchKey"`) {
		t.Fatalf("err=%v", err)
	}
	// Shift went down before the error, as in Playwright.
	if len(f.calls) != 1 {
		t.Errorf("calls=%d", len(f.calls))
	}
	if err := k.Up(context.Background(), "Bogus"); err == nil {
		t.Error("Up with unknown key should fail")
	}
}

func TestLayoutTableSize(t *testing.T) {
	if len(usKeyboardLayout) != 111 {
		t.Errorf("layout entries=%d, want 111", len(usKeyboardLayout))
	}
	if len(macEditingCommands) != 114 {
		t.Errorf("mac commands=%d, want 114", len(macEditingCommands))
	}
}
