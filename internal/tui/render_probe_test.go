package tui

import (
	"os"
	"strings"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// probeModel is a realistic finished-review screen: three agents, markdown
// replies, a decision, a pause. It backs the probe below and the layout
// tests, so they exercise the content real runs produce.
func probeModel(width, height int) Model {
	m := populatedModel(newFakeService())
	m.width, m.height = width, height
	m.agents = []domain.Agent{
		{ID: "opus", DisplayName: "OPUS", Kind: domain.AgentKindClaudeCode, Role: domain.RoleAuthority, State: domain.AgentStateStopped},
		{ID: "codex", DisplayName: "CODEX", Kind: domain.AgentKindCodex, Role: domain.RoleImplementer, State: domain.AgentStateWorking},
		{ID: "scout", DisplayName: "SCOUT", Kind: domain.AgentKindClaudeCode, Role: domain.RoleImplementer, State: domain.AgentStateStopped},
	}
	t := m.tasks[0]
	t.Title, t.State, t.PauseReason = "test", domain.TaskPaused, "TIMEOUT: no agent activity"
	t.MessageCount, t.InputTokens, t.OutputTokens, t.CostUSD = 31, 120000, 19200, 4.18
	t.Participants = []domain.AgentID{"opus", "codex", "scout"}

	base := time.Date(2026, 10, 9, 14, 20, 0, 0, time.Local)
	at := func(s int) time.Time { return base.Add(time.Duration(s) * time.Second) }
	m.messages = []*domain.Message{
		{ID: "M1", FromAgent: "human", ToAgent: "opus", Type: domain.MsgTaskAssignment, Timestamp: at(0),
			Content: "Review the planted-bug exercise and rate both reviewers."},
		{ID: "M2", FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Timestamp: at(20), Round: 1,
			Content: "Review `bugtest/inventory.py` cold. List every bug with a line number, a fix and a test."},
		{ID: "M3", FromAgent: "codex", ToAgent: "opus", Type: domain.MsgAnswer, Timestamp: at(140), Round: 1,
			Content: "## Findings\n\n1. **Off-by-one** in `average()` at line 12: `range(len(xs)-1)` skips the last item.\n2. **Mutable default** in `add_tag(tags=[])` at line 30.\n3. Missing `/100` in `discount()`.\n\nAll fixes are in the patch below.\n\n```python\ndef average(xs):\n    return sum(xs) / len(xs)\n```"},
		{ID: "M4", FromAgent: "scout", ToAgent: "opus", Type: domain.MsgReview, Timestamp: at(150), Round: 1,
			Metadata: map[string]string{"verdict": "approve"},
			Content:  "I agree with codex on all seven. One gap: **no tests for empty input**."},
		{ID: "M5", FromAgent: "opus", ToAgent: "human", Type: domain.MsgAnswer, Timestamp: at(229), Round: 4,
			Content: "**Both reviews are accurate.** Each found all 7 planted bugs, with no false positives and no wrong line numbers. But this test was too easy to tell the reviewers apart.\n\n## Verdict\n\n- **Accuracy:** 7/7, all lines correct\n- **Completeness:** every bug has an example, a fix and a test\n- **Limitations:** nothing was run; all three agents were read-only, so `inventory.py` and both findings files still need to be saved.\n\n> Scout's report is better as documentation; codex's is the cleaner set of fixes to apply.\n\n| reviewer | bugs | tests |\n|---|---|---|\n| codex | 7/7 | thin |\n| scout | 7/7 | good |"},
		{ID: "M6", FromAgent: "opus", ToAgent: "codex", Type: domain.MsgAnswer, Timestamp: at(229), Round: 4,
			Content: "I've sent the evaluation to the human directly in the ANSWER above. No further action is needed."},
		{ID: "M7", FromAgent: "codex", ToAgent: "opus", Type: domain.MsgAnswer, Timestamp: at(240), Round: 4,
			Content: "Understood. Thanks for the evaluation."},
	}
	m.events = []domain.Event{
		{ID: "E1", Type: domain.EvtAgentStateChanged, AgentID: "codex", TaskID: "TASK-001", Timestamp: at(250), Summary: "CODEX is STOPPED"},
		{ID: "E2", Type: domain.EvtTaskStateChanged, TaskID: "TASK-001", Timestamp: at(846), Summary: "DELIBERATING -> PAUSED",
			Details: map[string]string{"from": "DELIBERATING", "to": "PAUSED", "reason": "TIMEOUT: no agent activity"}},
	}
	m.decisions = []*domain.Decision{{ID: "D-001", TaskID: "TASK-001", Author: "opus", Status: domain.DecisionAccepted,
		Timestamp: at(200), Decision: "Both reviewers pass: **7/7 each**. Apply codex's fixes; keep scout's report as documentation.",
		Reasoning: "Every claimed bug was checked against the answer key.", Evidence: []string{"answer key", "line recount"}}}
	m.tasks = append(m.tasks, &domain.Task{ID: "TASK-002", Title: "Follow-up: write the files", State: domain.TaskCompleted})

	mm, _ := m.applyAgentActivity(domain.Event{Type: domain.EvtAgentActivity, AgentID: "codex", TaskID: "TASK-001",
		Summary: "working on ANSWER from opus", Details: map[string]string{"kind": string(ports.ActivityStatus)}})
	return mm.(Model)
}

// TestRenderProbe prints the real View() for inspection (-v) and guards that
// no line exceeds the terminal width.
func TestRenderProbe(t *testing.T) {
	for _, size := range [][2]int{{237, 52}, {120, 40}, {90, 34}} {
		m := probeModel(size[0], size[1])
		m.follow = false
		m.convSel = 4 // opus's answer to the human
		out := m.View()
		if os.Getenv("RITAM_PROBE") != "" {
			t.Logf("\n%s", out)
		}
		lines := strings.Split(out, "\n")
		if len(lines) != size[1] {
			t.Errorf("%dx%d: %d rows, want %d", size[0], size[1], len(lines), size[1])
		}
		for i, l := range lines {
			if w := visibleWidth(l); w != size[0] {
				t.Errorf("%dx%d: line %d is %d cols, want exactly %d", size[0], size[1], i, w, size[0])
			}
		}
	}
}
