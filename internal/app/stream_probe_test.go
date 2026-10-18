package app_test

import (
	"context"
	"os"
	"testing"
	"time"

	"ritam/internal/app"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// TestLiveStreamingActivity proves the adapters really stream: it runs a real
// CLI task and asserts that activity arrives *before* the task finishes.
//
// It is skipped unless RITAM_LIVE=1, so the normal suite stays offline and free.
// Run it with:
//
//	$env:RITAM_LIVE="1"; go test -run TestLiveStreamingActivity ./internal/app/ -v
func TestLiveStreamingActivity(t *testing.T) {
	if os.Getenv("RITAM_LIVE") != "1" {
		t.Skip("set RITAM_LIVE=1 to run against the real CLIs")
	}

	cfg, err := config.Load("../../ritam.yaml")
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.TUI.UseMockAgents = false
	cfg.Logging.File = ""
	cfg.Orchestrator.IdleTimeout = 5 * time.Minute

	sys, err := app.Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatalf("build: %v", err)
	}

	agents, err := sys.Service.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var participants []domain.AgentID
	for _, a := range agents {
		participants = append(participants, a.ID)
	}

	type sample struct {
		agent domain.AgentID
		kind  string
		when  time.Time
	}
	var (
		samples   []sample
		completed time.Time
	)
	unsub := sys.Service.Subscribe(func(e domain.Event) {
		switch e.Type {
		case domain.EvtAgentActivity:
			samples = append(samples, sample{e.AgentID, e.Details["kind"], time.Now()})
		case domain.EvtTaskCompleted:
			completed = time.Now()
		}
	})
	defer unsub()

	title := os.Getenv("RITAM_LIVE_TASK")
	if title == "" {
		title = "Name one exported function in internal/domain/task.go."
	}

	task, err := sys.Service.CreateTask(context.Background(), ports.CreateTaskInput{
		Title:        title,
		Authority:    domain.AgentID(cfg.Orchestrator.DefaultAuthority),
		Participants: participants,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.Service.RunTask(context.Background(), task.ID); err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(samples) == 0 {
		t.Fatal("no activity was streamed: the UI would show a frozen pane for the whole call")
	}

	kinds := map[string]int{}
	byAgent := map[domain.AgentID]int{}
	streamedEarly := 0
	for _, s := range samples {
		kinds[s.kind]++
		byAgent[s.agent]++
		if s.when.Before(completed) {
			streamedEarly++
		}
	}
	// A task that ends by pausing on a limit never emits TaskCompleted, so
	// only assert the ordering when there is a completion to order against.
	if !completed.IsZero() && streamedEarly == 0 {
		t.Error("all activity arrived after completion; it is not streaming")
	}

	t.Logf("activity events: %d across %d agent(s)", len(samples), len(byAgent))
	for k, n := range kinds {
		t.Logf("  kind %-9s %d", k, n)
	}
	for a, n := range byAgent {
		t.Logf("  agent %-8s %d", a, n)
	}
}
