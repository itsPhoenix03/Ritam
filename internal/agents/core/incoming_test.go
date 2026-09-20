package core

import (
	"strings"
	"testing"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

func TestACombinedDeliveryAndTheTeamBoardReachThePrompt(t *testing.T) {
	task := &domain.Task{ID: "TASK-001", Title: "t", AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex"}}
	m := domain.Message{ToAgent: "codex", FromAgent: "*", Type: domain.MsgInformation,
		Content: "=== 1 of 2 · PROPOSAL from scout ===\ndraft\n\n=== 2 of 2 · INFORMATION from [HUMAN] human ===\nstatus?",
		Metadata: map[string]string{
			ports.BatchMetadataKey:      "2",
			ports.BatchHumanMetadataKey: "true",
			ports.TeamMetadataKey:       "- opus (authority): working\n- codex (you): idle",
		}}
	got := RenderTurn(task, m, false, "@@ORCH")
	for _, want := range []string{
		"2 NEW MESSAGES SINCE YOUR LAST TURN",
		"answer those first (to=human)",
		"status?",
		"TEAM STATUS",
		"- opus (authority): working",
		"for your awareness only",
		"tell opus",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "INCOMING INFORMATION FROM") {
		t.Error("a combined delivery must not be presented as a single message")
	}

	m.ToAgent = "opus"
	if got := RenderTurn(task, m, false, "@@ORCH"); !strings.Contains(got, "You are the authority: act on this") {
		t.Errorf("the authority is told it may act on the board:\n%s", got)
	}
}
