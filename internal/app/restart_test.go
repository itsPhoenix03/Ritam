package app_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ritam/internal/app"
	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
	"ritam/internal/storage"
)

// testConfig is the default configuration plus two agents defined in the
// config (opus and codex), with every file it would write moved into the
// test's temp dir.
func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Agents = map[string]config.AgentConfig{
		"opus": {Type: "claude-code", DisplayName: "OPUS", Role: "AUTHORITY", Enabled: true,
			Executable: "claude", ResponseTimeout: 5 * time.Minute},
		"codex": {Type: "codex", DisplayName: "CODEX", Role: "IMPLEMENTER", Enabled: true,
			Executable: "codex", ResponseTimeout: 5 * time.Minute},
	}
	cfg.Orchestrator.DefaultAuthority = "opus"
	cfg.Storage.Path = filepath.Join(t.TempDir(), "ritam.db")
	cfg.Sessions.File = ""
	cfg.Logging.File = ""
	return cfg
}

// build wires the system and closes it when the test ends, releasing the
// database before the temp dir is removed.
func build(t *testing.T, cfg *config.Config) (*app.System, error) {
	t.Helper()
	sys, err := app.Build(cfg, logging.NewDiscard())
	if err == nil {
		t.Cleanup(func() { _ = sys.Close() })
	}
	return sys, err
}

func mockConfig(t *testing.T) *config.Config {
	cfg := testConfig(t)
	cfg.TUI.UseMockAgents = true
	cfg.Orchestrator.IdleTimeout = 2 * time.Second
	return cfg
}

func createDemoTask(t *testing.T, svc *app.Service) *domain.Task {
	t.Helper()
	task, err := svc.CreateTask(context.Background(), ports.CreateTaskInput{
		Title: "Fix the cookie bug", Authority: "opus", Participants: []domain.AgentID{"opus", "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

// TestATaskSurvivesARestart: a completed task, its messages and its
// decision are all still there after ritam exits and starts again, and new
// tasks keep counting from where the last run stopped.
func TestATaskSurvivesARestart(t *testing.T) {
	cfg := mockConfig(t)
	ctx := context.Background()

	first, err := app.Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	task := createDemoTask(t, first.Service)
	if err := first.Service.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	before, _ := first.Service.TaskMessages(ctx, task.ID)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	second, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := second.Service.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("the task is gone after a restart: %v", err)
	}
	if got.State != domain.TaskCompleted {
		t.Errorf("state = %s, want COMPLETED", got.State)
	}
	after, _ := second.Service.TaskMessages(ctx, task.ID)
	if len(after) == 0 || len(after) != len(before) {
		t.Errorf("messages: %d before the restart, %d after", len(before), len(after))
	}
	if ds, _ := second.Service.TaskDecisions(ctx, task.ID); len(ds) == 0 {
		t.Error("the decision is gone after a restart")
	}
	if len(second.Recovered) != 0 {
		t.Errorf("a completed task is not interrupted: %v", second.Recovered)
	}
	next := createDemoTask(t, second.Service)
	if next.ID != "TASK-002" {
		t.Errorf("numbering must continue across restarts: got %s", next.ID)
	}
}

// TestAFollowUpWorksAfterARestart is what persistence is for: a completed
// task can be followed up in a later session.
func TestAFollowUpWorksAfterARestart(t *testing.T) {
	cfg := mockConfig(t)
	ctx := context.Background()
	first, err := app.Build(cfg, logging.NewDiscard())
	if err != nil {
		t.Fatal(err)
	}
	task := createDemoTask(t, first.Service)
	if err := first.Service.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	_ = first.Close()

	second, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Orchestrator.Reopen(ctx, task.ID, "opus", "anything else to check?"); err != nil {
		t.Fatalf("a task from an earlier run must be reopenable: %v", err)
	}
	got, _ := second.Service.GetTask(ctx, task.ID)
	if got.State != domain.TaskDeliberating || !got.FollowUp {
		t.Errorf("reopened state = %s followUp=%v", got.State, got.FollowUp)
	}
}

// TestATaskRunningAtExitComesBackPaused: a task left mid-run (ritam was
// killed) must not sit in a working state with nothing driving it.
func TestATaskRunningAtExitComesBackPaused(t *testing.T) {
	cfg := mockConfig(t)
	ctx := context.Background()

	// Simulate the crash: write a task straight into the database in a
	// working state, with a question still owed, as a killed ritam leaves it.
	db, err := storage.OpenSQLite(cfg.Storage.Path)
	if err != nil {
		t.Fatal(err)
	}
	s := db.Storage()
	now := time.Now()
	for _, tk := range []*domain.Task{
		{ID: "TASK-001", Title: "mid-run", AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex"},
			State: domain.TaskDeliberating, PreviousState: domain.TaskPlanning, CreatedAt: now, UpdatedAt: now},
		{ID: "TASK-002", Title: "done", AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex"},
			State: domain.TaskCompleted, CreatedAt: now, UpdatedAt: now},
		{ID: "TASK-003", Title: "paused", AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex"},
			State: domain.TaskPaused, PreviousState: domain.TaskDeliberating, PauseReason: "paused by human",
			CreatedAt: now, UpdatedAt: now},
	} {
		if err := s.Tasks.Create(ctx, tk); err != nil {
			t.Fatal(err)
		}
	}
	_ = s.Messages.Append(ctx, &domain.Message{ID: "M1", TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex",
		Type: domain.MsgQuestion, Content: "is the cookie cleared on logout?", Timestamp: now})
	_ = db.Close()

	sys, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sys.Recovered) != 1 || sys.Recovered[0] != "TASK-001" {
		t.Fatalf("only the running task is interrupted, got %v", sys.Recovered)
	}
	got, _ := sys.Service.GetTask(ctx, "TASK-001")
	if got.State != domain.TaskPaused || got.PauseReason != domain.PauseReasonInterrupted {
		t.Fatalf("interrupted task: %s (%q)", got.State, got.PauseReason)
	}
	if other, _ := sys.Service.GetTask(ctx, "TASK-003"); other.PauseReason != "paused by human" {
		t.Errorf("an already paused task keeps its reason, got %q", other.PauseReason)
	}

	// Resuming tells codex, who owed the reply, that it was lost.
	var delivered []*domain.Message
	if err := sys.Orchestrator.Resume(ctx, "TASK-001"); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = sys.Orchestrator.RunTask(runCtx, "TASK-001")
	msgs, _ := sys.Service.TaskMessages(ctx, "TASK-001")
	for _, m := range msgs {
		if m.Metadata["orchestrator"] == "resumed" {
			delivered = append(delivered, m)
		}
	}
	if len(delivered) != 1 || delivered[0].ToAgent != "codex" ||
		!strings.Contains(delivered[0].Content, "is the cookie cleared on logout?") {
		t.Fatalf("codex must be told what it still owes; got %+v", delivered)
	}
}

func TestASecondRitamOnTheSameDatabaseIsRefused(t *testing.T) {
	cfg := mockConfig(t)
	if _, err := build(t, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Build(cfg, logging.NewDiscard()); err == nil || !strings.Contains(err.Error(), "in use by another ritam") {
		t.Fatalf("want a clear refusal, got %v", err)
	}
}
