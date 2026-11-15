package core

import (
	"strings"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
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

// TestANoticeIsShownAsTheOrchestratorsNotTheHumans: a notice about an
// agent's unreadable reply used to arrive as "INCOMING ERROR FROM human", and
// the agent answered the human about its own format mistake. It is shown as a
// notice from the orchestrator -- also one recorded as the human's before the
// orchestrator had an id of its own -- and every brief says what a notice is.
func TestANoticeIsShownAsTheOrchestratorsNotTheHumans(t *testing.T) {
	task := &domain.Task{ID: "TASK-001", Title: "t", AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex"}}
	for name, from := range map[string]domain.AgentID{"new": domain.OrchestratorID, "old": domain.HumanAgentID} {
		m := domain.Message{FromAgent: from, ToAgent: "codex", Type: domain.MsgError,
			Content: "Your last reply could not be read.", Metadata: map[string]string{domain.MetaReason: "malformed-reply"}}
		got := RenderTurn(task, m, false, "@@ORCH")
		if !strings.Contains(got, "--- NOTICE FROM orchestrator ---") {
			t.Errorf("%s: a notice must be shown as one:\n%s", name, got)
		}
		if strings.Contains(got, "FROM human") {
			t.Errorf("%s: a notice must never be shown as the human's:\n%s", name, got)
		}
		if !strings.Contains(got, NoticeRule) {
			t.Errorf("%s: the standing brief must say what a notice is for:\n%s", name, got)
		}
		if w := WorkingOn(m); strings.Contains(w, "human") {
			t.Errorf("%s: status line = %q", name, w)
		}
	}

	// The human's own message is still the human's.
	h := domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "codex", Type: domain.MsgInformation, Content: "Status?"}
	if got := RenderTurn(task, h, false, "@@ORCH"); !strings.Contains(got, "--- INCOMING INFORMATION FROM human ---") {
		t.Errorf("the human's message must be shown as the human's:\n%s", got)
	}

	var b strings.Builder
	WriteRoleGuide(&b, false, "@@ORCH")
	if !strings.Contains(b.String(), NoticeRule) {
		t.Errorf("the opening brief must say what a notice is for:\n%s", b.String())
	}
}
