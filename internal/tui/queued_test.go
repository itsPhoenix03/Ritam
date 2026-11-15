package tui

import (
	"strings"
	"testing"

	"ritam/internal/domain"
)

// TestAQueuedTaskIsMarkedInTheTaskListUntilItsRunStarts: a run waiting for
// a free slot looked exactly like a running task that had gone quiet. The
// task list marks it queued until its run starts or is withdrawn.
func TestAQueuedTaskIsMarkedInTheTaskListUntilItsRunStarts(t *testing.T) {
	m := populatedModel(newFakeService())
	for _, step := range []struct {
		event  domain.EventType
		queued bool
	}{
		{domain.EvtRunQueued, true},
		{domain.EvtRunStarted, false},
		{domain.EvtRunQueued, true},
		{domain.EvtRunUnqueued, false},
	} {
		mm, _ := m.Update(domainEventMsg(domain.Event{Type: step.event, TaskID: "TASK-001"}))
		m = mm.(Model)
		if got := strings.Contains(plainLines(m.taskLines(60)), "queued"); got != step.queued {
			t.Errorf("after %s the task list shows queued=%v, want %v:\n%s",
				step.event, got, step.queued, plainLines(m.taskLines(60)))
		}
	}
}
