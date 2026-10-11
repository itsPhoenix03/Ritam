package app_test

import (
	"context"
	"strings"
	"testing"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// TestARecruitedAgentJoinsTheTeamAndStays: recruit a second agent on the
// same model, run a task with it, and find it still there after a restart.
// orch.yaml is never touched: the recruit lives in team.json.
func TestARecruitedAgentJoinsTheTeamAndStays(t *testing.T) {
	cfg := mockConfig(t)
	sys, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := sys.Service.Recruit(ctx, ports.RecruitInput{Name: "opus-2", Provider: "claude-code", Model: "opus"})
	if err != nil {
		t.Fatalf("recruit: %v", err)
	}
	if a.ID != "opus-2" {
		t.Errorf("recruited %+v", a)
	}
	agents, _ := sys.Service.Agents(ctx)
	if !hasAgent(agents, "opus-2") {
		t.Fatal("the recruit should be on the team")
	}

	task, err := sys.Service.CreateTask(ctx, ports.CreateTaskInput{
		Title: "Review the scheduler", Authority: "opus-2", Participants: []domain.AgentID{"opus-2", "codex"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.Orchestrator.RunTask(ctx, task.ID); err != nil {
		t.Fatalf("run with the recruit: %v", err)
	}
	if got, _ := sys.Service.GetTask(ctx, task.ID); got.State != domain.TaskCompleted {
		t.Errorf("a task led by the recruit should complete: %s (%s)", got.State, got.PauseReason)
	}
	_ = sys.Close()

	sys2, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	agents, _ = sys2.Service.Agents(ctx)
	if !hasAgent(agents, "opus-2") {
		t.Error("a recruited agent must survive a restart")
	}
	if err := sys2.Service.Dismiss(ctx, "opus-2"); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	agents, _ = sys2.Service.Agents(ctx)
	if hasAgent(agents, "opus-2") {
		t.Error("a dismissed agent must leave the team")
	}
}

func TestRecruitingRefusesWhatCannotWork(t *testing.T) {
	sys, err := build(t, mockConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, in := range map[string]ports.RecruitInput{
		"taken name":       {Name: "opus", Provider: "claude-code", Model: "opus"},
		"bad name":         {Name: "Opus 2!", Provider: "claude-code", Model: "opus"},
		"the human's name": {Name: "human", Provider: "claude-code", Model: "opus"},
		"unknown provider": {Name: "g1", Provider: "gemini", Model: "x"},
	} {
		if _, err := sys.Service.Recruit(ctx, in); err == nil {
			t.Errorf("%s: recruiting %+v should fail", name, in)
		}
	}
	if err := sys.Service.Dismiss(ctx, "opus"); err == nil || !strings.Contains(err.Error(), "orch.yaml") {
		t.Errorf("an agent from orch.yaml cannot be dismissed: %v", err)
	}
}

func hasAgent(agents []domain.Agent, id domain.AgentID) bool {
	for _, a := range agents {
		if a.ID == id {
			return true
		}
	}
	return false
}

// TestATeamRecruitedFromNothing: orch.yaml defines no agents, so orch starts
// with an empty team; the agents the form recruits run a task to the end.
func TestATeamRecruitedFromNothing(t *testing.T) {
	cfg := mockConfig(t)
	cfg.Agents = nil
	cfg.Orchestrator.DefaultAuthority = ""
	sys, err := build(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if agents, _ := sys.Service.Agents(ctx); len(agents) != 0 {
		t.Fatalf("no agents are configured, got %d", len(agents))
	}
	for _, in := range []ports.RecruitInput{
		{Name: "opus-1", Provider: "claude-code", Model: "opus", AllowWrites: true},
		{Name: "gpt-6-sol-1", Provider: "codex", Model: "gpt-6-sol", AllowWrites: true},
	} {
		if _, err := sys.Service.Recruit(ctx, in); err != nil {
			t.Fatalf("recruit %s: %v", in.Name, err)
		}
	}
	task, err := sys.Service.CreateTask(ctx, ports.CreateTaskInput{
		Title: "Review the scheduler", Authority: "opus-1", Participants: []domain.AgentID{"opus-1", "gpt-6-sol-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sys.Orchestrator.RunTask(ctx, task.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := sys.Service.GetTask(ctx, task.ID); got.State != domain.TaskCompleted {
		t.Errorf("a task run by recruits should complete: %s (%s)", got.State, got.PauseReason)
	}
}
