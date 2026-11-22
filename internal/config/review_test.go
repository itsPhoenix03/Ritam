package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTheReviewModeDefaultsToAutoAndIsChecked(t *testing.T) {
	c := Default()
	if c.Review.Mode != ReviewAuto || c.Review.Wait() != DefaultAskTimeout {
		t.Errorf("default: %+v", c.Review)
	}
	c.Review.Mode = "yolo"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "review.mode") {
		t.Errorf("an unknown mode: %v", err)
	}
	c.Review = ReviewConfig{Mode: ReviewManual, AskTimeout: time.Hour}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "review.askTimeout") {
		t.Errorf("a wait past the limit: %v", err)
	}
}

// TestAProjectMayAskForManualApprovalButNeverDropIt: a cloned project's
// settings cannot switch off the approval the human chose on this machine.
func TestAProjectMayAskForManualApprovalButNeverDropIt(t *testing.T) {
	global := Default()
	global.Review.Mode = ReviewManual
	project := Default()
	project.Review.Mode = ReviewAuto
	held := project.RestrictProject(global, t.TempDir())
	if project.Review.Mode != ReviewManual || !strings.Contains(strings.Join(held, ","), "review.mode") {
		t.Errorf("mode %s, held %v", project.Review.Mode, held)
	}
	global.Review.Mode = ReviewAuto
	project.Review.Mode = ReviewManual
	if held := project.RestrictProject(global, t.TempDir()); project.Review.Mode != ReviewManual || len(held) != 0 {
		t.Errorf("a project may be stricter: %s %v", project.Review.Mode, held)
	}
}

func TestSaveReviewModeKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.yaml")
	body := "# mine\naccess:\n  level: 2\n  commands: [go]\nreview:\n  askTimeout: 2m\n  rules:\n    deny: [terraform apply]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveReviewMode(path, ReviewManual); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Review.Mode != ReviewManual || c.Access.Level != 2 || c.Review.AskTimeout != 2*time.Minute ||
		len(c.Review.Rules.Deny) != 1 {
		t.Errorf("only the mode changes: %+v %+v", c.Review, c.Access)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# mine") {
		t.Errorf("the file's comment was lost:\n%s", data)
	}
}

// TestAProjectAddsRulesButItsHooksWaitForApproval: a project's rules join
// the human's own (they only tighten); its hooks are programs it would
// have ritam run, so they wait for the human's approval on this machine.
func TestAProjectAddsRulesButItsHooksWaitForApproval(t *testing.T) {
	global := Default()
	global.Review.Rules.Deny = []string{"terraform apply"}
	mine := ReviewHook{Name: "mine", Run: []string{"python", "mine.py"}}
	global.Review.Hooks = []ReviewHook{mine}
	project := Default()
	project.Review.Rules.Deny = []string{"npm publish"} // replaced the global list when layered
	theirs := ReviewHook{Name: "theirs", Run: []string{"sh", "hooks/check.sh"}}
	project.Review.Hooks = []ReviewHook{theirs}
	held := project.RestrictProject(global, t.TempDir())
	if d := project.Review.Rules.Deny; len(d) != 2 || d[0] != "terraform apply" {
		t.Errorf("deny: %v", d)
	}
	if len(project.Review.Hooks) != 1 || project.Review.Hooks[0].Name != "mine" ||
		len(project.Runtime.HeldHooks) != 1 || !strings.Contains(strings.Join(held, ","), "review.hooks.theirs") {
		t.Errorf("hooks %+v, held %+v %v", project.Review.Hooks, project.Runtime.HeldHooks, held)
	}
	project.Review.Hooks = []ReviewHook{theirs}
	project.Runtime.HookApproved = func(h ReviewHook, root string) bool { return h.Name == "theirs" }
	project.RestrictProject(global, t.TempDir())
	if len(project.Review.Hooks) != 2 || len(project.Runtime.HeldHooks) != 0 {
		t.Errorf("approved: %+v held %+v", project.Review.Hooks, project.Runtime.HeldHooks)
	}
}

func TestAHookIsChecked(t *testing.T) {
	c := Default()
	c.Review.Hooks = []ReviewHook{{Name: "a", Run: []string{"x"}, Input: "weird"}, {Name: "a"}}
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "input must be") || !strings.Contains(err.Error(), "used twice") ||
		!strings.Contains(err.Error(), "needs run") {
		t.Errorf("err: %v", err)
	}
}
