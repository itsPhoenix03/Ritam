package review

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/review/facts"
)

// TestHelperHook is not a test: run with "-- <mode>", the test binary acts
// as a hook of the human's own, so the hook tests need no script.
func TestHelperHook(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 || i+1 >= len(os.Args) {
		return
	}
	in, _ := io.ReadAll(os.Stdin)
	switch os.Args[i+1] {
	case "deny":
		fmt.Fprintln(os.Stderr, "no pushes from agents")
		os.Exit(2)
	case "ask":
		fmt.Print(`{"decision":"ask","reason":"check the target"}`)
	case "claude-deny":
		fmt.Print(`{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny","permissionDecisionReason":"not on main"}}`)
	case "allow":
		fmt.Print(`{"hookSpecificOutput":{"permissionDecision":"allow"}}`)
	case "fail":
		fmt.Fprintln(os.Stderr, "crashed")
		os.Exit(1)
	case "sleep":
		time.Sleep(5 * time.Second)
	case "capture":
		_ = os.WriteFile(os.Args[i+2], in, 0o600)
	}
	os.Exit(0)
}

func helperHook(name, mode string, extra ...string) Hook {
	return Hook{Name: name, Run: append([]string{os.Args[0], "-test.run=^TestHelperHook$", "--", mode}, extra...),
		Timeout: 3 * time.Second, Block: true}
}

func withHooks(r *policyRig, hooks ...Hook) {
	r.srv.Policy.Hooks = func() []Hook { return hooks }
}

func TestYourHookCanRefuseACall(t *testing.T) {
	r := newPolicyRig(t, Settings{}, &fakeAsker{})
	withHooks(r, helperHook("no-push", "deny"))
	code, msg := r.run(t, "Bash", bash("go test ./..."))
	if code != blockExit || !strings.Contains(msg, "refused by your hook no-push: no pushes from agents") {
		t.Fatalf("exit %d: %s", code, msg)
	}
	if c := r.last(); c.Severity != "harmful" || !slices.ContainsFunc(c.Findings, func(f Finding) bool { return f.Rule == "hook.no-push" }) {
		t.Errorf("record: %+v", c)
	}
	withHooks(r, helperHook("main-guard", "claude-deny"))
	if code, msg := r.run(t, "Bash", bash("go test ./...")); code != blockExit || !strings.Contains(msg, "not on main") {
		t.Errorf("Claude Code's form: exit %d: %s", code, msg)
	}
}

func TestYourHookCanAskButNeverApprove(t *testing.T) {
	asker := &fakeAsker{answer: Answer{Allow: false}}
	r := newPolicyRig(t, Settings{}, asker)
	withHooks(r, helperHook("careful", "ask"))
	if code, _ := r.run(t, "Bash", bash("go test ./...")); code != blockExit || len(asker.asked) != 1 ||
		!strings.Contains(strings.Join(asker.asked[0].Reasons, ";"), "your hook careful: check the target") {
		t.Fatalf("exit %d, asked %+v", code, asker.asked)
	}
	withHooks(r, helperHook("yes-man", "allow"))
	if code, _ := r.run(t, "Bash", bash("rm -rf src")); code != blockExit || len(asker.asked) != 2 {
		t.Errorf("a hook's allow does not skip ritam's question: exit %d, asked %d", code, len(asker.asked))
	}
}

func TestAFailingHookRefusesUnlessToldOtherwise(t *testing.T) {
	r := newPolicyRig(t, Settings{}, &fakeAsker{})
	withHooks(r, helperHook("broken", "fail"))
	if code, msg := r.run(t, "Bash", bash("go test ./...")); code != blockExit || !strings.Contains(msg, "failed") {
		t.Errorf("block: exit %d: %s", code, msg)
	}
	h := helperHook("broken", "fail")
	h.Block = false
	withHooks(r, h)
	if code, msg := r.run(t, "Bash", bash("go test ./...")); code != 0 {
		t.Errorf("ignore: exit %d: %s", code, msg)
	}
	slow := helperHook("slow", "sleep")
	slow.Timeout = 300 * time.Millisecond
	withHooks(r, slow)
	if code, msg := r.run(t, "Bash", bash("go test ./...")); code != blockExit || !strings.Contains(msg, "no answer within") {
		t.Errorf("timeout: exit %d: %s", code, msg)
	}
}

func TestAHookGetsTheCallAndWhatRitamFound(t *testing.T) {
	r := newPolicyRig(t, Settings{}, &fakeAsker{})
	out := filepath.Join(t.TempDir(), "in.json")
	only := helperHook("watch", "capture", out)
	only.Wants = func(tool string, kind facts.ToolKind, cli domain.AgentKind) bool { return kind == facts.ToolShell }
	withHooks(r, only)
	if code, _ := r.run(t, "Read", map[string]string{"file_path": "/home/me/proj/a.go"}); code != 0 {
		t.Fatal("a read passes")
	}
	if _, err := os.Stat(out); err == nil {
		t.Fatal("a hook for shell calls does not see a read")
	}
	if code, _ := r.run(t, "Bash", bash("sudo rm -f /etc/x")); code == 0 {
		t.Log("refused by the rules first, as expected or not")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a call the rules refused does not reach the hooks")
	}
	if code, _ := r.run(t, "Bash", bash("go vet ./...")); code != 0 {
		t.Fatal("go vet passes")
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var got hookPayload
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	if got.Tool != "Bash" || got.Agent != "opus" || got.Kind != "shell" || len(got.Commands) != 1 ||
		got.Commands[0].Program != "go" || !strings.Contains(string(got.ToolInput), "go vet") {
		t.Errorf("payload: %+v", got)
	}
}
