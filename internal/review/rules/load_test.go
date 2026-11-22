package rules

import (
	"slices"
	"strings"
	"testing"
)

func TestTheBuiltInRulesLoad(t *testing.T) {
	s, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Rules) < 300 {
		t.Errorf("only %d rules loaded", len(s.Rules))
	}
	if a := s.ActionFor(Critical); a != DenyAndStop {
		t.Errorf("critical: %s", a)
	}
	r := s.Rule("destroy.recursive-delete.outside")
	if r == nil || r.Severity != Critical || r.On != OnCommand || !slices.Contains(r.match.program, "remove-item") {
		t.Fatalf("rule: %+v", r)
	}
	if len(r.Examples.Hit) == 0 || r.Examples.Hit[0].Kind != "bash" {
		t.Errorf("a plain example is a bash command: %+v", r.Examples.Hit)
	}
	if h := s.Rule("content.honeytoken"); h == nil || h.Disabled() == "" {
		t.Errorf("without a honeytoken its rule cannot fire: %+v", h)
	}
}

func TestAListReferenceIsExpanded(t *testing.T) {
	s, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Rules {
		for _, p := range r.match.program {
			if strings.HasPrefix(p, "@") {
				t.Errorf("%s: %s was not expanded", r.ID, p)
			}
		}
	}
}

const head = "version: 1\nactions: {info: record, suspicious: ask, harmful: deny, critical: deny-and-stop}\n" +
	"categories: {destruction: x}\nlists: {dels: [rm, del]}\n"

func TestAMistakeInTheRulesIsAnError(t *testing.T) {
	cases := map[string]string{
		"unknown key":           "rules:\n- {id: a, category: destruction, severity: harmful, on: command, reason: r, match: {program: [rm], flag_any: [-r]}}\n",
		"key of another target": "rules:\n- {id: a, category: destruction, severity: harmful, on: path, reason: r, match: {program: [rm]}}\n",
		"unknown zone":          "rules:\n- {id: a, category: destruction, severity: harmful, on: path, reason: r, match: {zone: [moon]}}\n",
		"unknown list":          "rules:\n- {id: a, category: destruction, severity: harmful, on: command, reason: r, match: {program: ['@nope']}}\n",
		"bad regex":             "rules:\n- {id: a, category: destruction, severity: harmful, on: input, reason: r, match: {regex_any: ['(?=x)']}}\n",
		"unknown category":      "rules:\n- {id: a, category: fun, severity: harmful, on: input, reason: r, match: {regex_any: [x]}}\n",
		"no reason":             "rules:\n- {id: a, category: destruction, severity: harmful, on: input, match: {regex_any: [x]}}\n",
		"empty match":           "rules:\n- {id: a, category: destruction, severity: harmful, on: input, reason: r, match: {}}\n",
		"twice": "rules:\n- {id: a, category: destruction, severity: harmful, on: input, reason: r, match: {regex_any: [x]}}\n" +
			"- {id: a, category: destruction, severity: harmful, on: input, reason: r, match: {regex_any: [y]}}\n",
		"step names no rule": "rules:\n- {id: a, category: destruction, severity: harmful, on: sequence, reason: r, match: {steps: [{rule: [b]}], within_calls: 3}}\n",
	}
	for name, rules := range cases {
		if _, err := Parse([]byte(head+rules), Options{}); err == nil {
			t.Errorf("%s: loaded", name)
		}
	}
	ok := "rules:\n- {id: a, category: destruction, severity: harmful, on: command, reason: r, match: {program: ['@dels']}}\n"
	s, err := Parse([]byte(head+ok), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Rule("a").match.program; len(got) != 2 || got[1] != "del" {
		t.Errorf("program: %v", got)
	}
}
