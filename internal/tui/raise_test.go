package tui

import (
	"testing"

	"ritam/internal/domain"
)

// TestTheRaiseCommandGivesATaskAtItsLimitMoreRoom: a task paused at its
// message ceiling could only be resumed for one more message at a time.
// ":raise messages N" allows N more counted from what the task has used, and
// ":raise budget $N" adds to its budget the same way.
func TestTheRaiseCommandGivesATaskAtItsLimitMoreRoom(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	task := m.currentTask()
	task.Limits.MaxTotalMessages, task.MessageCount = 40, 40
	task.Limits.MaxCostUSD, task.CostUSD = 2, 2.30

	for _, c := range []struct {
		cmd  string
		want domain.TaskLimits
	}{
		{"raise messages 20", domain.TaskLimits{MaxTotalMessages: 60}},
		{"raise budget $1.50", domain.TaskLimits{MaxCostUSD: 3.80}},
	} {
		mm, cmd := m.runCommand(c.cmd)
		if cmd == nil {
			t.Fatalf("%q did nothing: %s", c.cmd, mm.statusLine)
		}
		if msg, ok := cmd().(actionResultMsg); !ok || msg.err != nil || msg.action != ActionRaiseLimits {
			t.Fatalf("%q: %+v", c.cmd, msg)
		}
		got := svc.lastRaise
		if got.MaxTotalMessages != c.want.MaxTotalMessages || got.MaxCostUSD < c.want.MaxCostUSD-1e-9 || got.MaxCostUSD > c.want.MaxCostUSD+1e-9 {
			t.Errorf("%q asked for %+v, want %+v", c.cmd, got, c.want)
		}
	}

	task.Limits.MaxCostUSD = 0
	for _, bad := range []string{"raise", "raise messages", "raise budget 2", "raise tokens 5", "raise messages -3"} {
		if mm, cmd := m.runCommand(bad); cmd != nil || mm.statusLine == "" {
			t.Errorf("%q must only explain itself", bad)
		}
	}
	if n := svc.callCount("RaiseLimits"); n != 2 {
		t.Errorf("RaiseLimits called %d times, want 2", n)
	}
}
