// Ported from Playwright's usKeyboardLayout.ts (data converted mechanically from the TS).
//
// Source: https://github.com/microsoft/playwright/blob/main/packages/playwright-core/src/server/usKeyboardLayout.ts
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

package keyboard

// keypadLocation is the DOM KeyboardEvent location value for numpad keys.
const keypadLocation = 3

// keyDefinition is one physical key of a layout, as in Playwright's
// KeyDefinition type. Zero values mean "not set".
type keyDefinition struct {
	Key                    string
	KeyCode                int64
	KeyCodeWithoutLocation int64
	ShiftKey               string
	ShiftKeyCode           int64
	Text                   string
	Location               int64
}

// layoutEntry pairs a DOM code with its definition. The table is a slice, not
// a map, because the order of entries decides which code wins when two keys
// produce the same character, as it does in Playwright.
type layoutEntry struct {
	Code string
	Def  keyDefinition
}

// usKeyboardLayout is the US keyboard layout, in Playwright's source order.
var usKeyboardLayout = []layoutEntry{
	{"Escape", keyDefinition{Key: "Escape", KeyCode: 27}},
	{"F1", keyDefinition{Key: "F1", KeyCode: 112}},
	{"F2", keyDefinition{Key: "F2", KeyCode: 113}},
	{"F3", keyDefinition{Key: "F3", KeyCode: 114}},
	{"F4", keyDefinition{Key: "F4", KeyCode: 115}},
	{"F5", keyDefinition{Key: "F5", KeyCode: 116}},
	{"F6", keyDefinition{Key: "F6", KeyCode: 117}},
	{"F7", keyDefinition{Key: "F7", KeyCode: 118}},
	{"F8", keyDefinition{Key: "F8", KeyCode: 119}},
	{"F9", keyDefinition{Key: "F9", KeyCode: 120}},
	{"F10", keyDefinition{Key: "F10", KeyCode: 121}},
	{"F11", keyDefinition{Key: "F11", KeyCode: 122}},
	{"F12", keyDefinition{Key: "F12", KeyCode: 123}},
	{"Backquote", keyDefinition{Key: "`", KeyCode: 192, ShiftKey: "~"}},
	{"Digit1", keyDefinition{Key: "1", KeyCode: 49, ShiftKey: "!"}},
	{"Digit2", keyDefinition{Key: "2", KeyCode: 50, ShiftKey: "@"}},
	{"Digit3", keyDefinition{Key: "3", KeyCode: 51, ShiftKey: "#"}},
	{"Digit4", keyDefinition{Key: "4", KeyCode: 52, ShiftKey: "$"}},
	{"Digit5", keyDefinition{Key: "5", KeyCode: 53, ShiftKey: "%"}},
	{"Digit6", keyDefinition{Key: "6", KeyCode: 54, ShiftKey: "^"}},
	{"Digit7", keyDefinition{Key: "7", KeyCode: 55, ShiftKey: "&"}},
	{"Digit8", keyDefinition{Key: "8", KeyCode: 56, ShiftKey: "*"}},
	{"Digit9", keyDefinition{Key: "9", KeyCode: 57, ShiftKey: "("}},
	{"Digit0", keyDefinition{Key: "0", KeyCode: 48, ShiftKey: ")"}},
	{"Minus", keyDefinition{Key: "-", KeyCode: 189, ShiftKey: "_"}},
	{"Equal", keyDefinition{Key: "=", KeyCode: 187, ShiftKey: "+"}},
	{"Backslash", keyDefinition{Key: "\\", KeyCode: 220, ShiftKey: "|"}},
	{"Backspace", keyDefinition{Key: "Backspace", KeyCode: 8}},
	{"Tab", keyDefinition{Key: "Tab", KeyCode: 9}},
	{"KeyQ", keyDefinition{Key: "q", KeyCode: 81, ShiftKey: "Q"}},
	{"KeyW", keyDefinition{Key: "w", KeyCode: 87, ShiftKey: "W"}},
	{"KeyE", keyDefinition{Key: "e", KeyCode: 69, ShiftKey: "E"}},
	{"KeyR", keyDefinition{Key: "r", KeyCode: 82, ShiftKey: "R"}},
	{"KeyT", keyDefinition{Key: "t", KeyCode: 84, ShiftKey: "T"}},
	{"KeyY", keyDefinition{Key: "y", KeyCode: 89, ShiftKey: "Y"}},
	{"KeyU", keyDefinition{Key: "u", KeyCode: 85, ShiftKey: "U"}},
	{"KeyI", keyDefinition{Key: "i", KeyCode: 73, ShiftKey: "I"}},
	{"KeyO", keyDefinition{Key: "o", KeyCode: 79, ShiftKey: "O"}},
	{"KeyP", keyDefinition{Key: "p", KeyCode: 80, ShiftKey: "P"}},
	{"BracketLeft", keyDefinition{Key: "[", KeyCode: 219, ShiftKey: "{"}},
	{"BracketRight", keyDefinition{Key: "]", KeyCode: 221, ShiftKey: "}"}},
	{"CapsLock", keyDefinition{Key: "CapsLock", KeyCode: 20}},
	{"KeyA", keyDefinition{Key: "a", KeyCode: 65, ShiftKey: "A"}},
	{"KeyS", keyDefinition{Key: "s", KeyCode: 83, ShiftKey: "S"}},
	{"KeyD", keyDefinition{Key: "d", KeyCode: 68, ShiftKey: "D"}},
	{"KeyF", keyDefinition{Key: "f", KeyCode: 70, ShiftKey: "F"}},
	{"KeyG", keyDefinition{Key: "g", KeyCode: 71, ShiftKey: "G"}},
	{"KeyH", keyDefinition{Key: "h", KeyCode: 72, ShiftKey: "H"}},
	{"KeyJ", keyDefinition{Key: "j", KeyCode: 74, ShiftKey: "J"}},
	{"KeyK", keyDefinition{Key: "k", KeyCode: 75, ShiftKey: "K"}},
	{"KeyL", keyDefinition{Key: "l", KeyCode: 76, ShiftKey: "L"}},
	{"Semicolon", keyDefinition{Key: ";", KeyCode: 186, ShiftKey: ":"}},
	{"Quote", keyDefinition{Key: "'", KeyCode: 222, ShiftKey: "\""}},
	{"Enter", keyDefinition{Key: "Enter", KeyCode: 13, Text: "\r"}},
	{"ShiftLeft", keyDefinition{Key: "Shift", KeyCode: 160, KeyCodeWithoutLocation: 16, Location: 1}},
	{"KeyZ", keyDefinition{Key: "z", KeyCode: 90, ShiftKey: "Z"}},
	{"KeyX", keyDefinition{Key: "x", KeyCode: 88, ShiftKey: "X"}},
	{"KeyC", keyDefinition{Key: "c", KeyCode: 67, ShiftKey: "C"}},
	{"KeyV", keyDefinition{Key: "v", KeyCode: 86, ShiftKey: "V"}},
	{"KeyB", keyDefinition{Key: "b", KeyCode: 66, ShiftKey: "B"}},
	{"KeyN", keyDefinition{Key: "n", KeyCode: 78, ShiftKey: "N"}},
	{"KeyM", keyDefinition{Key: "m", KeyCode: 77, ShiftKey: "M"}},
	{"Comma", keyDefinition{Key: ",", KeyCode: 188, ShiftKey: "<"}},
	{"Period", keyDefinition{Key: ".", KeyCode: 190, ShiftKey: ">"}},
	{"Slash", keyDefinition{Key: "/", KeyCode: 191, ShiftKey: "?"}},
	{"ShiftRight", keyDefinition{Key: "Shift", KeyCode: 161, KeyCodeWithoutLocation: 16, Location: 2}},
	{"ControlLeft", keyDefinition{Key: "Control", KeyCode: 162, KeyCodeWithoutLocation: 17, Location: 1}},
	{"MetaLeft", keyDefinition{Key: "Meta", KeyCode: 91, Location: 1}},
	{"AltLeft", keyDefinition{Key: "Alt", KeyCode: 164, KeyCodeWithoutLocation: 18, Location: 1}},
	{"Space", keyDefinition{Key: " ", KeyCode: 32}},
	{"AltRight", keyDefinition{Key: "Alt", KeyCode: 165, KeyCodeWithoutLocation: 18, Location: 2}},
	{"AltGraph", keyDefinition{Key: "AltGraph", KeyCode: 225}},
	{"MetaRight", keyDefinition{Key: "Meta", KeyCode: 92, Location: 2}},
	{"ContextMenu", keyDefinition{Key: "ContextMenu", KeyCode: 93}},
	{"ControlRight", keyDefinition{Key: "Control", KeyCode: 163, KeyCodeWithoutLocation: 17, Location: 2}},
	{"PrintScreen", keyDefinition{Key: "PrintScreen", KeyCode: 44}},
	{"ScrollLock", keyDefinition{Key: "ScrollLock", KeyCode: 145}},
	{"Pause", keyDefinition{Key: "Pause", KeyCode: 19}},
	{"PageUp", keyDefinition{Key: "PageUp", KeyCode: 33}},
	{"PageDown", keyDefinition{Key: "PageDown", KeyCode: 34}},
	{"Insert", keyDefinition{Key: "Insert", KeyCode: 45}},
	{"Delete", keyDefinition{Key: "Delete", KeyCode: 46}},
	{"Home", keyDefinition{Key: "Home", KeyCode: 36}},
	{"End", keyDefinition{Key: "End", KeyCode: 35}},
	{"ArrowLeft", keyDefinition{Key: "ArrowLeft", KeyCode: 37}},
	{"ArrowUp", keyDefinition{Key: "ArrowUp", KeyCode: 38}},
	{"ArrowRight", keyDefinition{Key: "ArrowRight", KeyCode: 39}},
	{"ArrowDown", keyDefinition{Key: "ArrowDown", KeyCode: 40}},
	{"AudioVolumeMute", keyDefinition{Key: "AudioVolumeMute", KeyCode: 173}},
	{"AudioVolumeDown", keyDefinition{Key: "AudioVolumeDown", KeyCode: 174}},
	{"AudioVolumeUp", keyDefinition{Key: "AudioVolumeUp", KeyCode: 175}},
	{"MediaTrackNext", keyDefinition{Key: "MediaTrackNext", KeyCode: 176}},
	{"MediaTrackPrevious", keyDefinition{Key: "MediaTrackPrevious", KeyCode: 177}},
	{"MediaPlayPause", keyDefinition{Key: "MediaPlayPause", KeyCode: 179}},
	{"NumLock", keyDefinition{Key: "NumLock", KeyCode: 144}},
	{"NumpadDivide", keyDefinition{Key: "/", KeyCode: 111, Location: 3}},
	{"NumpadMultiply", keyDefinition{Key: "*", KeyCode: 106, Location: 3}},
	{"NumpadSubtract", keyDefinition{Key: "-", KeyCode: 109, Location: 3}},
	{"Numpad7", keyDefinition{Key: "Home", KeyCode: 36, ShiftKey: "7", ShiftKeyCode: 103, Location: 3}},
	{"Numpad8", keyDefinition{Key: "ArrowUp", KeyCode: 38, ShiftKey: "8", ShiftKeyCode: 104, Location: 3}},
	{"Numpad9", keyDefinition{Key: "PageUp", KeyCode: 33, ShiftKey: "9", ShiftKeyCode: 105, Location: 3}},
	{"Numpad4", keyDefinition{Key: "ArrowLeft", KeyCode: 37, ShiftKey: "4", ShiftKeyCode: 100, Location: 3}},
	{"Numpad5", keyDefinition{Key: "Clear", KeyCode: 12, ShiftKey: "5", ShiftKeyCode: 101, Location: 3}},
	{"Numpad6", keyDefinition{Key: "ArrowRight", KeyCode: 39, ShiftKey: "6", ShiftKeyCode: 102, Location: 3}},
	{"NumpadAdd", keyDefinition{Key: "+", KeyCode: 107, Location: 3}},
	{"Numpad1", keyDefinition{Key: "End", KeyCode: 35, ShiftKey: "1", ShiftKeyCode: 97, Location: 3}},
	{"Numpad2", keyDefinition{Key: "ArrowDown", KeyCode: 40, ShiftKey: "2", ShiftKeyCode: 98, Location: 3}},
	{"Numpad3", keyDefinition{Key: "PageDown", KeyCode: 34, ShiftKey: "3", ShiftKeyCode: 99, Location: 3}},
	{"Numpad0", keyDefinition{Key: "Insert", KeyCode: 45, ShiftKey: "0", ShiftKeyCode: 96, Location: 3}},
	{"NumpadDecimal", keyDefinition{Key: "Delete", KeyCode: 46, ShiftKey: ".", ShiftKeyCode: 110, Location: 3}},
	{"NumpadEnter", keyDefinition{Key: "Enter", KeyCode: 13, Text: "\r", Location: 3}},
}
