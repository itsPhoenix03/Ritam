package tui

import (
	"strings"
	"testing"

	"ritam/internal/domain"
)

// TestAFailedTaskCanBeRetriedFromTheKeyAndThePalette: FAILED was terminal,
// with nothing on screen to do about it. R and :retry bring a failed task
// back; on any other task they do nothing but say why, and the key is shown
// only for a failed task.
func TestAFailedTaskCanBeRetriedFromTheKeyAndThePalette(t *testing.T) {
	if got, _, ok := lookupAction("R"); !ok || got != ActionRetryTask {
		t.Fatalf("R must retry, got %q", got)
	}
	svc := newFakeService()
	m := populatedModel(svc)
	if _, cmd := m.dispatch(ActionRetryTask, 0); cmd != nil {
		t.Fatal("a task that has not failed is not retried")
	}
	if strings.Contains(stripANSI(m.helpBarRow(200)), "R retry") {
		t.Error("the retry key is shown only for a failed task")
	}

	m.tasks[0].State = domain.TaskFailed
	m.tasks[0].FailureReason = "AGENT_ERROR: codex: agent CLI timed out after 5m0s"
	if !strings.Contains(stripANSI(m.helpBarRow(200)), "R retry") {
		t.Error("a failed task shows the retry key")
	}
	_, byKey := m.dispatch(ActionRetryTask, 0)
	_, byPalette := m.runCommand("retry")
	if byKey == nil || byPalette == nil {
		t.Fatal("R and :retry must act on a failed task")
	}
	for _, cmd := range []func() any{func() any { return byKey() }, func() any { return byPalette() }} {
		if res, ok := cmd().(actionResultMsg); !ok || res.err != nil || res.action != ActionRetryTask {
			t.Errorf("want a successful retry result, got %+v", res)
		}
	}
	if got := svc.callCount("Retry"); got != 2 {
		t.Errorf("Retry called %d times, want 2 (key and palette)", got)
	}
}
