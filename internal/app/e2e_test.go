package app_test

import (
	"context"
	"testing"
	"time"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// TestEndToEndThroughTheRealWiring exercises the whole stack the way the TUI
// does — configuration, the agent registry and manager, the orchestrator and
// the application service — with mock agents standing in for Claude Code and
// Codex. It requires no provider access, no network and no API keys.
func TestEndToEndThroughTheRealWiring(t *testing.T) {
	cfg := testConfig(t)
	cfg.TUI.UseMockAgents = true
	cfg.Orchestrator.IdleTimeout = 2 * time.Second
	cfg.Logging.File = "" // never write a log file from tests

	sys, err := build(t, cfg)
	if err != nil {
		t.Fatalf("build system: %v", err)
	}
	svc := sys.Service
	ctx := context.Background()

	agents, err := svc.Agents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 2 {
		t.Fatalf("expected opus and codex to be registered, got %d agents", len(agents))
	}

	participants := []domain.AgentID{}
	for _, a := range agents {
		participants = append(participants, a.ID)
	}

	task, err := svc.CreateTask(ctx, ports.CreateTaskInput{
		Title:        "Fix the authentication cookie bug",
		Description:  "Logged-out users keep an authenticated session cookie.",
		Authority:    "opus",
		Participants: participants,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	if task.ID != "TASK-001" {
		t.Errorf("task id = %s, want TASK-001", task.ID)
	}

	var seen []domain.EventType
	unsubscribe := svc.Subscribe(func(e domain.Event) { seen = append(seen, e.Type) })
	defer unsubscribe()

	if err := svc.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("run task: %v", err)
	}

	final, err := svc.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.State != domain.TaskCompleted {
		t.Fatalf("task state = %s, want COMPLETED (pause %q, failure %q)",
			final.State, final.PauseReason, final.FailureReason)
	}

	msgs, err := svc.TaskMessages(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 8 {
		t.Fatalf("expected a multi-round exchange, got %d messages", len(msgs))
	}

	ds, err := svc.TaskDecisions(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Status != domain.DecisionAccepted || ds[0].Author != "opus" {
		t.Fatalf("expected one ACCEPTED decision authored by opus, got %+v", ds)
	}

	events, err := svc.TaskEvents(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("the task must have an event log")
	}
	if len(seen) == 0 {
		t.Fatal("subscribers must receive events as they happen")
	}

	// Shared knowledge, not the conversation, is the durable project state.
	hits, err := svc.SearchKnowledge(ctx, domain.KnowledgeQuery{Text: "cookie"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Error("expected the task's findings to be searchable in shared knowledge")
	}
}

// TestBuildRegistersNAgents proves the system is not hard-coded around exactly
// two agents: adding a third is configuration, not code.
func TestBuildRegistersNAgents(t *testing.T) {
	cfg := testConfig(t)
	cfg.Logging.File = ""
	cfg.Agents["reviewer"] = config.AgentConfig{
		Type: "mock", DisplayName: "REVIEWER", Role: "REVIEWER", Enabled: true,
	}
	cfg.Agents["disabled"] = config.AgentConfig{
		Type: "mock", DisplayName: "OFF", Role: "OBSERVER", Enabled: false,
	}

	sys, err := build(t, cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	agents, err := sys.Service.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 3 {
		t.Fatalf("expected 3 enabled agents, got %d", len(agents))
	}
	for _, a := range agents {
		if a.ID == "disabled" {
			t.Fatal("a disabled agent must not be registered")
		}
	}
}

// TestRealAdaptersAreSelectedWhenMocksAreOff proves the wiring picks the
// provider-specific adapters, while making no claim that they work: they are
// skeletons, and starting them would require the real CLIs.
func TestRealAdaptersAreSelectedWhenMocksAreOff(t *testing.T) {
	cfg := testConfig(t)
	cfg.Logging.File = ""
	cfg.TUI.UseMockAgents = false

	sys, err := build(t, cfg)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	agents, err := sys.Service.Agents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[domain.AgentID]domain.AgentKind{}
	for _, a := range agents {
		kinds[a.ID] = a.Kind
	}
	if kinds["opus"] != domain.AgentKindClaudeCode {
		t.Errorf("opus kind = %s, want claude-code", kinds["opus"])
	}
	if kinds["codex"] != domain.AgentKindCodex {
		t.Errorf("codex kind = %s, want codex", kinds["codex"])
	}
}

// TestProxyIsDisabledByDefault guards the boundary: the scaffold must not try
// to route provider traffic anywhere.
func TestProxyIsDisabledByDefault(t *testing.T) {
	cfg := testConfig(t)
	cfg.Logging.File = ""
	sys, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if sys.Proxy.Enabled() {
		t.Error("the local LLM proxy must be disabled by default")
	}
	if sys.Proxy.Endpoint() != "" {
		t.Errorf("a disabled proxy must not advertise an endpoint, got %q", sys.Proxy.Endpoint())
	}
}
