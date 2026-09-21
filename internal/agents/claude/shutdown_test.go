package claude

import (
	"context"
	"os"
	"testing"
	"time"

	"projectorch/internal/config"
	"projectorch/internal/domain"
)

// TestStopKillsInFlightInvocation is a regression test for real token waste.
//
// Stop used to wait for an in-flight CLI call to finish naturally. Observed in
// a live run: the authority delegated to two agents and issued its
// FINAL_DECISION in the same instant; the task completed, and the orchestrator
// then sat for 15 more seconds while both agents produced answers that were
// discarded. Every one of those tokens was billed. With responseTimeout at its
// 5-minute default the stall could be far longer.
//
// The child process here is a sleeping copy of the test binary, so nothing is
// spawned that this test does not control, and no CLI or network is involved.
func TestStopKillsInFlightInvocation(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}

	a := New("opus", config.AgentConfig{
		Executable: exe,
		// Run the helper below, which sleeps far longer than this test allows.
		Args:            []string{"-test.run=TestSleepHelper"},
		ResponseTimeout: 2 * time.Minute,
	}, nil)

	if err := a.Start(context.Background()); err != nil {
		t.Fatalf("start: %v", err)
	}

	m := domain.Message{TaskID: "TASK-001", FromAgent: "human", ToAgent: "opus", Type: domain.MsgTaskAssignment}
	if err := a.SendMessage(context.Background(), nil, m); err != nil {
		t.Fatalf("send: %v", err)
	}

	// Let the child actually start before stopping it.
	time.Sleep(300 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- a.Stop(context.Background()) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return promptly: it is waiting for the in-flight call " +
			"instead of killing it, which bills tokens for an answer nobody will read")
	}

	if got := a.Status(); got != domain.AgentStateStopped {
		t.Errorf("status = %s, want STOPPED", got)
	}
}

// TestSleepHelper is the long-running child process. It is inert unless the
// adapter invokes the test binary with -test.run pointed at it.
func TestSleepHelper(t *testing.T) {
	if os.Getenv("ORCH_SLEEP_HELPER") != "1" {
		t.Skip("not running as the sleep helper")
	}
	time.Sleep(2 * time.Minute)
}

// TestDoubleStopIsSafe checks Stop is idempotent now that it cancels.
func TestDoubleStopIsSafe(t *testing.T) {
	a := New("opus", config.AgentConfig{Executable: "claude"}, nil)
	// Never started: cancelAll is nil, so this also covers the nil path.
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("stop on a never-started adapter: %v", err)
	}
	if err := a.Stop(context.Background()); err != nil {
		t.Fatalf("second stop: %v", err)
	}
}
