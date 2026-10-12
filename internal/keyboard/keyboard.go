// Ported from Playwright's packages/playwright-core/src/server/input.ts
// (Keyboard) and packages/playwright-core/src/server/chromium/crInput.ts
// (RawKeyboardImpl).
//
// Copyright 2017 Google Inc. All rights reserved.
// Modifications copyright (c) Microsoft Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package keyboard emulates a US keyboard over the Chrome DevTools Protocol,
// with the same key names, combo syntax and event shapes as Playwright.
package keyboard

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	cdproto "github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
)

// Modifier bits, as used by the CDP "modifiers" field.
const (
	ModifierAlt     int64 = 1
	ModifierControl int64 = 2
	ModifierMeta    int64 = 4
	ModifierShift   int64 = 8
)

// modifierBits maps a modifier key name to its bit.
var modifierBits = map[string]int64{
	"Alt":     ModifierAlt,
	"Control": ModifierControl,
	"Meta":    ModifierMeta,
	"Shift":   ModifierShift,
}

// keyDescription is a resolved key, as in Playwright's KeyDescription.
type keyDescription struct {
	KeyCode                int64
	KeyCodeWithoutLocation int64
	Key                    string
	Text                   string
	Code                   string
	Location               int64
	Shifted                *keyDescription
}

// aliases are extra names that resolve to the unshifted description of a code.
var aliases = map[string][]string{
	"ShiftLeft":   {"Shift"},
	"ControlLeft": {"Control"},
	"AltLeft":     {"Alt"},
	"MetaLeft":    {"Meta"},
	"Enter":       {"\n", "\r"},
}

// layout maps every accepted key name (code, alias, or single character) to
// its description.
var layout = buildLayout(usKeyboardLayout)

func buildLayout(entries []layoutEntry) map[string]keyDescription {
	result := make(map[string]keyDescription)
	for _, e := range entries {
		def := e.Def
		desc := keyDescription{
			Key:                    def.Key,
			KeyCode:                def.KeyCode,
			KeyCodeWithoutLocation: def.KeyCodeWithoutLocation,
			Code:                   e.Code,
			Text:                   def.Text,
			Location:               def.Location,
		}
		if desc.KeyCodeWithoutLocation == 0 {
			desc.KeyCodeWithoutLocation = def.KeyCode
		}
		if runeLen(def.Key) == 1 {
			desc.Text = desc.Key
		}

		var shifted *keyDescription
		if def.ShiftKey != "" {
			s := desc
			s.Key = def.ShiftKey
			s.Text = def.ShiftKey
			if def.ShiftKeyCode != 0 {
				s.KeyCode = def.ShiftKeyCode
				s.KeyCodeWithoutLocation = def.ShiftKeyCode
			}
			shifted = &s
		}

		withShift := desc
		withShift.Shifted = shifted
		result[e.Code] = withShift

		for _, alias := range aliases[e.Code] {
			result[alias] = desc
		}

		// Do not use numpad when converting keys to codes.
		if def.Location != 0 {
			continue
		}
		if runeLen(desc.Key) == 1 {
			result[desc.Key] = desc
		}
		if shifted != nil {
			result[shifted.Key] = *shifted
		}
	}
	return result
}

func runeLen(s string) int { return len([]rune(s)) }

// Keyboard sends key events to one CDP session and tracks which keys and
// modifiers are held. It is not safe for concurrent use.
type Keyboard struct {
	sess cdproto.Session

	// Mac selects macOS behavior: "ControlOrMeta" resolves to Meta, and key
	// down events carry the editing commands that macOS binds to the
	// shortcut. New sets it to runtime.GOOS == "darwin".
	Mac bool

	modifiers   int64
	pressedKeys map[string]bool
}

// New returns a Keyboard bound to sess.
func New(sess cdproto.Session) *Keyboard {
	return &Keyboard{
		sess:        sess,
		Mac:         runtime.GOOS == "darwin",
		pressedKeys: make(map[string]bool),
	}
}

// Modifiers returns the bitmask of held modifiers (Alt=1, Control=2, Meta=4,
// Shift=8), for use in mouse events.
func (k *Keyboard) Modifiers() int64 { return k.modifiers }

func (k *Keyboard) resolveSmartModifier(key string) string {
	if key == "ControlOrMeta" {
		if k.Mac {
			return "Meta"
		}
		return "Control"
	}
	return key
}

func (k *Keyboard) describe(key string) (keyDescription, error) {
	name := k.resolveSmartModifier(key)
	desc, ok := layout[name]
	if !ok {
		return keyDescription{}, fmt.Errorf("keyboard: unknown key: %q", name)
	}
	if k.modifiers&ModifierShift != 0 && desc.Shifted != nil {
		desc = *desc.Shifted
	}
	// If any modifier other than Shift is held, no text is sent.
	if k.modifiers&^ModifierShift != 0 {
		desc.Text = ""
	}
	return desc, nil
}

// Down dispatches a key down event for key, a key name such as "a", "Enter",
// "Shift" or "ControlOrMeta". A modifier key stays held until Up.
func (k *Keyboard) Down(ctx context.Context, key string) error {
	desc, err := k.describe(key)
	if err != nil {
		return err
	}
	if k.pressedKeys == nil {
		k.pressedKeys = make(map[string]bool)
	}
	autoRepeat := k.pressedKeys[desc.Code]
	k.pressedKeys[desc.Code] = true
	k.modifiers |= modifierBits[desc.Key]

	typ := input.DispatchKeyEventTypeKeyDown
	if desc.Text == "" {
		typ = input.DispatchKeyEventTypeRawKeyDown
	}
	isKeypad := desc.Location == keypadLocation
	_, err = cdproto.Call(ctx, k.sess, input.DispatchKeyEvent, input.DispatchKeyEventParams{
		Type:                  typ,
		Modifiers:             k.modifiers,
		WindowsVirtualKeyCode: desc.KeyCodeWithoutLocation,
		Code:                  desc.Code,
		Commands:              k.commandsFor(desc.Code),
		Key:                   desc.Key,
		Text:                  desc.Text,
		UnmodifiedText:        desc.Text,
		AutoRepeat:            &autoRepeat,
		Location:              desc.Location,
		IsKeypad:              &isKeypad,
	})
	return err
}

// Up dispatches a key up event for key and releases it.
func (k *Keyboard) Up(ctx context.Context, key string) error {
	desc, err := k.describe(key)
	if err != nil {
		return err
	}
	k.modifiers &^= modifierBits[desc.Key]
	delete(k.pressedKeys, desc.Code)

	isKeypad := desc.Location == keypadLocation
	_, err = cdproto.Call(ctx, k.sess, input.DispatchKeyEvent, input.DispatchKeyEventParams{
		Type:                  input.DispatchKeyEventTypeKeyUp,
		Modifiers:             k.modifiers,
		Key:                   desc.Key,
		WindowsVirtualKeyCode: desc.KeyCodeWithoutLocation,
		Code:                  desc.Code,
		Location:              desc.Location,
		IsKeypad:              &isKeypad,
	})
	return err
}

// Press presses and releases a combo in Playwright syntax, such as "Enter",
// "Shift+Tab", "ControlOrMeta+C" or "+". Leading keys are held down in order,
// the last key is pressed, and then the held keys are released in reverse.
func (k *Keyboard) Press(ctx context.Context, combo string) error {
	tokens := splitCombo(combo)
	last := tokens[len(tokens)-1]
	for _, t := range tokens[:len(tokens)-1] {
		if err := k.Down(ctx, t); err != nil {
			return err
		}
	}
	if err := k.Down(ctx, last); err != nil {
		return err
	}
	if err := k.Up(ctx, last); err != nil {
		return err
	}
	for i := len(tokens) - 2; i >= 0; i-- {
		if err := k.Up(ctx, tokens[i]); err != nil {
			return err
		}
	}
	return nil
}

// splitCombo splits on "+", but a "+" that starts a token is part of the key,
// so "+" and "Shift++" work as in Playwright.
func splitCombo(s string) []string {
	var keys []string
	var b strings.Builder
	for _, r := range s {
		if r == '+' && b.Len() > 0 {
			keys = append(keys, b.String())
			b.Reset()
			continue
		}
		b.WriteRune(r)
	}
	return append(keys, b.String())
}

// InsertText inserts text with Input.insertText, without key events.
func (k *Keyboard) InsertText(ctx context.Context, text string) error {
	_, err := cdproto.Call(ctx, k.sess, input.InsertText, input.InsertTextParams{Text: text})
	return err
}

// Type presses a key for every character of text that is on the layout, and
// inserts the other characters with Input.insertText.
func (k *Keyboard) Type(ctx context.Context, text string) error {
	for _, r := range text {
		ch := string(r)
		var err error
		if _, ok := layout[ch]; ok {
			err = k.Press(ctx, ch)
		} else {
			err = k.InsertText(ctx, ch)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// commandsFor returns the Chromium editing commands for code with the held
// modifiers on macOS, or nil elsewhere.
func (k *Keyboard) commandsFor(code string) []string {
	if !k.Mac {
		return nil
	}
	var parts []string
	for _, m := range []string{"Shift", "Control", "Alt", "Meta"} {
		if k.modifiers&modifierBits[m] != 0 {
			parts = append(parts, m)
		}
	}
	parts = append(parts, code)
	var out []string
	for _, c := range macEditingCommands[strings.Join(parts, "+")] {
		// Commands that insert text are not supported.
		if strings.HasPrefix(c, "insert") {
			continue
		}
		// Drop the trailing ":" to match the Chromium command names.
		out = append(out, strings.TrimSuffix(c, ":"))
	}
	return out
}
