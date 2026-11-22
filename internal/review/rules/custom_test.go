package rules

import (
	"encoding/json"
	"slices"
	"testing"

	"ritam/internal/review/facts"
)

func ids(s *Set, call *facts.Call, env facts.Env) []string {
	var out []string
	for _, f := range s.Check(call, env) {
		out = append(out, f.Rule.ID)
	}
	return out
}

func TestYourOwnRulesAreReadLikeCommands(t *testing.T) {
	s, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Extend(Custom{Deny: []string{"terraform apply", "rm -rf"}, Ask: []string{"npm publish"},
		Protect: []string{"infra/prod/**"}}); err != nil {
		t.Fatal(err)
	}
	env := exampleEnv("linux")
	cases := []struct {
		code string
		want string
		hit  bool
	}{
		{"terraform apply -auto-approve", "custom.deny.1", true},
		{"terraform -chdir=infra apply", "custom.deny.1", true},
		{"sudo terraform apply", "custom.deny.1", true},
		{"terraform plan", "custom.deny.1", false},
		{"rm -r -f build/x", "custom.deny.2", true},
		{"rm -f x", "custom.deny.2", false},
		{"npm publish --access public", "custom.ask.1", true},
		{"echo hi > infra/prod/main.tf", "custom.protect.1", true},
		{"cat infra/prod/main.tf", "custom.protect.1", false},
	}
	for _, tc := range cases {
		got := ids(s, facts.Shell(facts.Bash, tc.code, "", env), env)
		if slices.Contains(got, tc.want) != tc.hit {
			t.Errorf("%q: %v (want %s: %v)", tc.code, got, tc.want, tc.hit)
		}
	}
	data, _ := json.Marshal(map[string]string{"file_path": "/home/me/proj/infra/prod/x.tf", "content": "x"})
	call := facts.Build(facts.Input{Tool: "Write", ToolInput: data}, env)
	if got := ids(s, call, env); !slices.Contains(got, "custom.protect.1") {
		t.Errorf("a file tool's write: %v", got)
	}
	if r := s.Rule("custom.deny.1"); r == nil || r.Severity != Harmful || r.Reason != "Your settings deny `terraform apply`." {
		t.Errorf("rule: %+v", r)
	}
	if err := s.Extend(Custom{Deny: []string{"echo 'unclosed"}}); err == nil {
		t.Error("a pattern that does not parse is an error")
	}
}
