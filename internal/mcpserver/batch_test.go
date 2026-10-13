package mcpserver

import "testing"

func TestStepMethod(t *testing.T) {
	cases := []struct {
		step   BatchStep
		method string
		plain  bool
		bad    bool
	}{
		{BatchStep{Tool: "click"}, "click", false, false},
		{BatchStep{Tool: "key", Args: map[string]any{"combo": "Enter"}}, "key", false, false},
		{BatchStep{Tool: "key", Args: map[string]any{"text": "hi"}}, "keytype", false, false},
		{BatchStep{Tool: "key"}, "", false, true},
		{BatchStep{Tool: "dialog"}, "dialog", true, false},
		{BatchStep{Tool: "dialog", Args: map[string]any{"accept": true}}, "dialog", false, false},
		{BatchStep{Tool: "history", Args: map[string]any{"action": "back"}}, "back", false, false},
		{BatchStep{Tool: "history", Args: map[string]any{"action": "up"}}, "", false, true},
		{BatchStep{Tool: "tabs", Args: map[string]any{"action": "new"}}, "tab_new", true, false},
		{BatchStep{Tool: "wait"}, "wait", true, false},
		{BatchStep{Tool: "run"}, "", false, true},
		{BatchStep{Tool: "screenshot"}, "", false, true},
	}
	for _, c := range cases {
		m, p, err := stepMethod(c.step)
		if (err != nil) != c.bad || m != c.method || p != c.plain {
			t.Errorf("%+v: got %q %v %v", c.step, m, p, err)
		}
	}
}
