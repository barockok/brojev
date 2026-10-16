package agent

import (
	"sort"
	"strings"
	"unicode"

	"github.com/barockok/brojev/internal/page"
)

// fill is one deterministic field/value pairing found without asking Jev.
type fill struct {
	el   page.Element
	key  string
	text string
}

// words lowercases s and splits it into alphanumeric words, also splitting
// snake_case, kebab-case and camelCase ("firstName" -> first, name).
func words(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	prevLower := false
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if unicode.IsUpper(r) && prevLower {
				flush()
			}
			cur = append(cur, r)
			prevLower = unicode.IsLower(r)
		default:
			flush()
			prevLower = false
		}
	}
	flush()
	return out
}

func containsAll(have map[string]bool, want []string) bool {
	if len(want) == 0 {
		return false
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}

// prefills pairs empty, visible, editable fields with provided values when the
// pairing is unambiguous both ways: every word of the value's key appears in
// the field's accessible name, exactly one key fits that field, and exactly
// one field fits that key. Anything else is left to Jev, which can see the
// goal. done holds "name|key" pairs already typed so a field is filled once.
func prefills(s *page.Snapshot, values map[string]string, done map[string]bool) []fill {
	if s.Dialog != nil || len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var fields []page.Element
	for _, e := range s.Elements {
		if !e.InView || !editable(e) || e.Name == "" {
			continue
		}
		if e.Value != nil && *e.Value != "" {
			continue
		}
		fields = append(fields, e)
	}
	byField := map[int][]string{}
	byKey := map[string][]int{}
	for i, f := range fields {
		have := map[string]bool{}
		for _, w := range words(f.Name) {
			have[w] = true
		}
		for _, k := range keys {
			if containsAll(have, words(k)) {
				byField[i] = append(byField[i], k)
				byKey[k] = append(byKey[k], i)
			}
		}
	}
	var out []fill
	for i, f := range fields {
		ks := byField[i]
		if len(ks) != 1 || len(byKey[ks[0]]) != 1 || done[f.Name+"|"+ks[0]] || values[ks[0]] == "" {
			continue
		}
		out = append(out, fill{el: f, key: ks[0], text: values[ks[0]]})
	}
	return out
}
