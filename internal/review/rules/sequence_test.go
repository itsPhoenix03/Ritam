package rules

import (
	"encoding/json"
	"slices"
	"testing"

	"ritam/internal/review/facts"
)

// turn replays calls through Check and CheckSequence the way the review
// does, deciding each by its worst severity, and returns the last call's
// rule ids.
func turn(t *testing.T, s *Set, env facts.Env, calls ...*facts.Call) []string {
	t.Helper()
	var h History
	var ids []string
	for _, c := range calls {
		f := s.Check(c, env)
		decision := "pass"
		if len(f) > 0 && f[0].Rule.Severity >= Harmful {
			decision = "deny"
		}
		cur := StepOf(c, f, decision)
		seq := s.CheckSequence(cur, &h, env)
		f = append(f, seq...)
		cur = StepOf(c, f, decision)
		h.Add(cur)
		ids = nil
		for _, x := range f {
			ids = append(ids, x.Rule.ID)
		}
	}
	return ids
}

func write(env facts.Env, path, content string) *facts.Call {
	data, _ := json.Marshal(map[string]string{"file_path": path, "content": content})
	return facts.Build(facts.Input{Provider: "claude-code", Tool: "Write", ToolInput: data}, env)
}

func TestRunningAScriptWrittenThisTurnIsNoticed(t *testing.T) {
	s, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	env := exampleEnv("linux")
	ids := turn(t, s, env, write(env, "/home/me/proj/x.sh", "echo hi"), facts.Shell(facts.Bash, "bash x.sh", "", env))
	if !slices.Contains(ids, "seq.write-then-run") {
		t.Errorf("ids: %v", ids)
	}
	ids = turn(t, s, env, write(env, "/home/me/proj/x.sh", "echo hi"), facts.Shell(facts.Bash, "bash other.sh", "", env))
	if slices.Contains(ids, "seq.write-then-run") {
		t.Errorf("another script: %v", ids)
	}
}

func TestRepeatedRefusalsStopTheTurn(t *testing.T) {
	s, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	env := exampleEnv("linux")
	bad := func() *facts.Call { return facts.Shell(facts.Bash, "git push --force origin main", "", env) }
	ids := turn(t, s, env, bad(), bad(), bad())
	if !slices.Contains(ids, "seq.repeated-denials") {
		t.Errorf("three refusals: %v", ids)
	}
	if ids := turn(t, s, env, bad(), facts.Shell(facts.Bash, "git push -f origin main", "", env)); !slices.Contains(ids, "seq.retry-after-deny") {
		t.Errorf("trying again another way: %v", ids)
	}
}
