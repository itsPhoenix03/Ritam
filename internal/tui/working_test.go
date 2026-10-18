package tui

import (
	"strings"
	"testing"
	"time"

	"ritam/internal/domain"
)

// TestAWorkingAgentHasAThinkingEntry: an agent handed work has a thinking
// entry in the conversation, at the time its turn began and drawn like the
// other entries, until the message it posts replaces it. Agents that are
// not working, or not on the task, have none.
func TestAWorkingAgentHasAThinkingEntry(t *testing.T) {
	m := swarmModel() // chair and codex WORKING, claude-b STARTING, gemini IDLE
	m.width, m.height = 160, 40
	m = m.trackLive(m.agents)
	began := time.Now().Add(-75 * time.Second)
	m.liveSince[liveKey(m.currentTaskID, "chair")] = began

	working := map[domain.AgentID]convItem{}
	for _, it := range m.convItems() {
		if it.working != "" {
			working[it.working] = it
		}
	}
	if len(working) != 3 || working["chair"].at != began {
		t.Fatalf("chair, codex and claude-b should each have one thinking entry, chair's at its turn start: %+v", working)
	}
	inner, _ := m.paneBox(paneConversation)
	text := plainLines(m.convLinesForTest(inner))
	if !strings.Contains(text, began.Format("15:04:05")+" chair  ") || !strings.Contains(text, "thinking") {
		t.Errorf("the thinking entry should read like a conversation entry:\n%s", text)
	}

	// The message chair posts replaces its thinking entry.
	m.messages = append(m.messages, &domain.Message{ID: "M-new", TaskID: m.currentTaskID, FromAgent: "chair",
		ToAgent: "codex", Type: domain.MsgDelegate, Content: "do it", Timestamp: time.Now()})
	for _, it := range m.convItems() {
		if it.working == "chair" {
			t.Error("chair's message should replace its thinking entry")
		}
	}

	// A thinking entry can be selected; the inspector then shows the agent.
	m.follow = false
	for i, it := range m.convItems() {
		if it.working == "codex" {
			m.convSel = i
		}
	}
	if title, _ := m.inspectorContent(60); title != "agent codex" {
		t.Errorf("inspecting a thinking entry should show the agent, got %q", title)
	}

	// Nobody working, or the task finished: no thinking entries.
	for i := range m.agents {
		m.agents[i].State = domain.AgentStateIdle
	}
	m = m.trackLive(m.agents)
	if len(m.workingItems()) != 0 || len(m.liveSince) != 0 {
		t.Error("idle agents should have no thinking entries")
	}
	checkFits(t, "conversation with thinking entries", m.View(), 160)
}

// convLinesForTest renders the whole conversation.
func (m Model) convLinesForTest(inner int) []line {
	l, _ := m.convLines(m.convItems(), inner)
	return l
}

// TestTheFinalDecisionStandsOut: barred in violet with a DECISION badge and
// several lines of its text.
func TestTheFinalDecisionStandsOut(t *testing.T) {
	m := swarmModel()
	m.messages = append(m.messages, &domain.Message{ID: "M-final", TaskID: m.currentTaskID, FromAgent: "chair",
		ToAgent: domain.BroadcastAgentID, Type: domain.MsgFinalDecision, Timestamp: time.Now(),
		Content: "**Approved.**\n\n- one\n- two\n\n" + strings.Repeat("reasoning ", 60)})
	inner, _ := m.paneBox(paneConversation)
	text := plainLines(m.convLinesForTest(inner))
	if !strings.Contains(text, "*   FINAL DECISION ") || !strings.Contains(text, gl().youBar+"   Approved. one two") {
		t.Errorf("the final decision should be badged and barred:\n%s", text)
	}
	checkFits(t, "final decision", m.View(), m.width)
}

// TestTheAgentsPaneShowsTheTasksParticipants: with a task selected, agents
// on the team but not on the task are left out of the pane.
func TestTheAgentsPaneShowsTheTasksParticipants(t *testing.T) {
	m := swarmModel() // cursor is on the team but not on TASK-001
	for _, a := range m.rosterIdents() {
		if a.agent.ID == "cursor" {
			t.Error("cursor is not a participant of the selected task")
		}
	}
	if n := len(m.rosterIdents()); n != 5 {
		t.Errorf("the pane should list the 5 participants, got %d", n)
	}
	if strings.Contains(stripANSI(m.View()), "] cursor") {
		t.Error("the agents pane should not list cursor")
	}
	m.currentTaskID = ""
	if n := len(m.rosterIdents()); n != len(m.agents) {
		t.Errorf("with no task selected the whole team is listed, got %d of %d", n, len(m.agents))
	}
}
