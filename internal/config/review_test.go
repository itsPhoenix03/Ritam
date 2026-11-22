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

func TestSaveReviewKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.yaml")
	if err := os.WriteFile(path, []byte("# mine\naccess:\n  level: 2\n  commands: [go]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveReview(path, ReviewConfig{Mode: ReviewManual, AskTimeout: 2 * time.Minute}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Review.Mode != ReviewManual || c.Review.AskTimeout != 2*time.Minute || c.Access.Level != 2 {
		t.Errorf("loaded: %+v %+v", c.Review, c.Access)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "# mine") {
		t.Errorf("the file's comment was lost:\n%s", data)
	}
}
