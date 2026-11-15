package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// View correctness: what the keys act on is what the screen shows, results
// land on the task they were loaded for, and rows fit their panes whatever
// the script.

func TestTheNumberKeysFocusTheAgentsAsTheRosterNumbersThem(t *testing.T) {
	m := swarmModel()
	// alpha is on the team but not on the task, and sorts before codex.
	m.agents = append(m.agents[:1], append([]domain.Agent{{ID: "alpha", Kind: domain.AgentKindMock,
		Role: domain.RoleImplementer, State: domain.AgentStateIdle}}, m.agents[1:]...)...)

	m, _ = key(m, "2")
	id, ok := m.focusedAgentID()
	if !ok || id != "codex" {
		t.Fatalf("2 focuses the second agent in the roster (codex), got %q", id)
	}

	inner, _ := m.paneBox(paneAgents)
	var codexRow string
	for _, l := range m.agentLines(inner) {
		if strings.Contains(l.plain(), " codex") {
			codexRow = l.plain()
		}
	}
	if !strings.HasPrefix(strings.TrimLeft(codexRow, " "+gl().caret), "2 ") {
		t.Errorf("the roster prints the key beside the agent: %q", codexRow)
	}

	m, _ = key(m, "6") // the roster has five agents
	if _, ok := m.focusedAgentID(); ok {
		t.Error("a number past the roster returns to the split view")
	}
}

func TestAgentsLoadedForAnotherTaskAreDropped(t *testing.T) {
	m := twoTasks(populatedModel(newFakeService()))
	stale := []domain.Agent{{ID: "opus", State: domain.AgentStateError}}
	n, _ := m.Update(agentsLoadedMsg{taskID: "TASK-002", agents: stale})
	if mm := n.(Model); mm.agents[0].State == domain.AgentStateError {
		t.Error("agents loaded for TASK-002 must not show on TASK-001")
	}
	n, _ = m.Update(agentsLoadedMsg{taskID: "TASK-001", agents: stale})
	if mm := n.(Model); mm.agents[0].State != domain.AgentStateError {
		t.Error("agents loaded for the task shown are applied")
	}
}

func TestEveryChangeOfTaskReloadsItsAgents(t *testing.T) {
	svc := newFakeService()
	m := newModel(t.Context(), svc, Options{})
	// Finished tasks, so no ticker is armed: its command would sleep.
	tasks := []*domain.Task{{ID: "TASK-001", State: domain.TaskCompleted}, {ID: "TASK-002", State: domain.TaskCancelled}}
	n, cmd := m.Update(tasksLoadedMsg{tasks: tasks})
	if n.(Model).currentTaskID == "" || cmd == nil {
		t.Fatal("startup selects a task")
	}
	runAll(cmd)
	if svc.callCount("Agents") == 0 {
		t.Error("selecting the first task at startup loads its agents")
	}
}

// runAll runs a command and every command it batches.
func runAll(cmd tea.Cmd) {
	if cmd == nil {
		return
	}
	if b, ok := cmd().(tea.BatchMsg); ok {
		for _, c := range b {
			runAll(c)
		}
	}
}

func TestAnOlderTaskDetailDoesNotOverwriteANewerOne(t *testing.T) {
	m := populatedModel(newFakeService())
	m, _ = m.reloadDetail()
	older := m.detailSeq
	m, _ = m.reloadDetail()
	newer := m.detailSeq

	newMsgs := []*domain.Message{{ID: "NEW", TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex", Content: "newer"}}
	oldMsgs := []*domain.Message{{ID: "OLD", TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex", Content: "older"}}
	n, _ := m.Update(taskDetailMsg{taskID: "TASK-001", seq: newer, messages: newMsgs})
	n, _ = n.(Model).Update(taskDetailMsg{taskID: "TASK-001", seq: older, messages: oldMsgs})
	if got := n.(Model).messages; len(got) != 1 || got[0].ID != "NEW" {
		t.Errorf("an older reload arriving last must be ignored, got %+v", got[0])
	}
}

func TestStartupShowsTheNewestOpenTask(t *testing.T) {
	tasks := func(states ...domain.TaskState) []*domain.Task {
		var out []*domain.Task
		for i, s := range states {
			out = append(out, &domain.Task{ID: domain.FormatTaskID(i + 1), State: s})
		}
		return out
	}
	cases := []struct {
		name  string
		tasks []*domain.Task
		want  domain.TaskID
	}{
		{"newest open", tasks(domain.TaskDeliberating, domain.TaskPaused, domain.TaskCompleted), "TASK-002"},
		{"all finished", tasks(domain.TaskCompleted, domain.TaskCancelled), "TASK-002"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newModel(t.Context(), newFakeService(), Options{})
			n, _ := m.Update(tasksLoadedMsg{tasks: c.tasks})
			if got := n.(Model).currentTaskID; got != c.want {
				t.Errorf("shown %s, want %s", got, c.want)
			}
		})
	}
}

func TestARejectionNoticeIsNotShownAsTheHumansOwnMessage(t *testing.T) {
	notice := &domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "codex", Type: domain.MsgError,
		Content: "Your FINAL_DECISION was rejected by the orchestrator", Metadata: map[string]string{"violation": "AUTHORITY_VIOLATION"}}
	if humanAuthored(notice) {
		t.Error("a rejection notice is the orchestrator's, not the human's")
	}
	if !humanAuthored(&domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "codex", Content: "look at the cache"}) {
		t.Error("what the human typed is still theirs")
	}
}

func TestTheHelpSaysWhatPauseAndQuitDo(t *testing.T) {
	for _, c := range commands {
		switch c.name {
		case "pause":
			if strings.Contains(c.help, "keep the processes") {
				t.Errorf("pause stops the agents: %q", c.help)
			}
		case "quit":
			if strings.Contains(c.help, "keep running") {
				t.Errorf("quitting stops running tasks: %q", c.help)
			}
		}
	}
}

func TestACutLineOfWideTextIsExactlyItsWidth(t *testing.T) {
	for _, s := range []string{"数据库迁移失败了吗请检查日志", "deploy 🚀🚀🚀 done ✅ ok", "ab数c"} {
		for _, w := range []int{1, 2, 5, 10, 11} {
			if got := ansi.StringWidth(line{txt(s)}.render(w)); got != w {
				t.Errorf("render(%d) of %q is %d columns", w, s, got)
			}
			if got := ansi.StringWidth(line{txt(s)}.renderSelected(w)); got != w {
				t.Errorf("renderSelected(%d) of %q is %d columns", w, s, got)
			}
		}
	}
	for _, l := range wrapWords("数据库迁移失败了吗 请检查日志 🚀🚀🚀🚀", 7) {
		if ansi.StringWidth(l) > 7 {
			t.Errorf("wrapped row %q is wider than 7", l)
		}
	}
	for _, l := range wrapKeepingSpaces("数据库迁移失败了吗 请检查日志", 7) {
		if ansi.StringWidth(l) > 7 {
			t.Errorf("input row %q is wider than 7", l)
		}
	}
}

// TestEveryLineIsExactlyTheScreenWidthWithWideText: CJK and emoji used to be
// counted one column each, so a row holding them came out wider than its
// pane and sheared the columns.
func TestEveryLineIsExactlyTheScreenWidthWithWideText(t *testing.T) {
	for _, width := range []int{80, 120, 160} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			m := swarmModel()
			m.width, m.height = width, 40
			m.tasks[0].Title = "修复登录 cookie 问题 🍪 并迁移数据库"
			m.tasks = append(m.tasks, &domain.Task{ID: "TASK-002", Title: "第二个任务 📦📦📦📦📦📦📦📦📦📦📦📦", State: domain.TaskPaused})
			for _, msg := range m.messages {
				msg.Content = "调查 cookie 路径 🔍 " + msg.Content + " 数据库迁移失败了吗请检查日志"
			}
			m.activityFor("codex").appendAt(ports.ActivityText, "测试失败 ❌ webhook_test.go:88 预期一次扣款，实际两次 💸💸", time.Now())
			m.statusLine = "状态 ✅ 一切正常"
			for _, tab := range []centerTab{tabConversation, tabActivity, tabEvents, tabDecisions} {
				m.center = tab
				for i, l := range strings.Split(m.View(), "\n") {
					if w := ansi.StringWidth(l); w != width {
						t.Errorf("%s tab: line %d is %d columns, want %d:\n%s", tab, i, w, width, ansi.Strip(l))
					}
				}
			}
		})
	}
}
