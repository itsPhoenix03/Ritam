package tui

import (
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Background results (a task created, a list loaded) arrive whenever their
// command finishes. These tests pin the rule they follow: they may change
// data and the status line, never what the human is doing.

func TestATaskCreatedWhileTypingAnInstructionLeavesThePromptAlone(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m, seq := m.beginCreate()
	m, _ = key(m, "i")
	for _, k := range []string{"c", "h", "e", "c", "k"} {
		m, _ = key(m, k)
	}
	if m.mode != modePrompt || m.prompt.input != "check" {
		t.Fatalf("setup: mode %v input %q", m.mode, m.prompt.input)
	}

	created := &domain.Task{ID: "TASK-002", Title: "another", State: domain.TaskCreated}
	n, _ := m.Update(taskCreatedMsg{seq: seq, task: created})
	mm := n.(Model)
	if mm.mode != modePrompt {
		t.Errorf("a creation result must not close the prompt, mode = %v", mm.mode)
	}
	if mm.prompt.input != "check" {
		t.Errorf("the typed text must survive, got %q", mm.prompt.input)
	}
	if mm.currentTaskID != "TASK-001" {
		t.Errorf("the task on screen must not change under the human, got %s", mm.currentTaskID)
	}
	// The next key still types rather than running as a hotkey.
	mm, _ = key(mm, "q")
	if mm.mode != modePrompt || mm.prompt.input != "checkq" || mm.quitting {
		t.Errorf("after the result the prompt keeps taking text: mode %v input %q", mm.mode, mm.prompt.input)
	}
}

func TestATaskCreatedFromTheFormIsShownWhenTheHumanWaitedForIt(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.models = []ports.ModelInfo{{Provider: "claude-code", ID: "opus"}, {Provider: "codex", ID: "gpt-6-sol"}}
	m.mode = modeCreateForm
	m.form = createForm{title: "new work", recruit: map[int]int{0: 1, 1: 1}, authority: "opus-1"}

	next, cmd := m.submitCreateForm()
	if cmd == nil {
		t.Fatal("a valid form should produce a create command")
	}
	msg := cmd()
	if svc.callCount("StartTask") != 1 || svc.callCount("RunTask") != 0 {
		t.Fatalf("creating must start the task without waiting for its run: StartTask %d RunTask %d",
			svc.callCount("StartTask"), svc.callCount("RunTask"))
	}
	n, follow := next.(Model).Update(msg)
	mm := n.(Model)
	if mm.currentTaskID != svc.created.ID {
		t.Errorf("the task just created is shown, got %s want %s", mm.currentTaskID, svc.created.ID)
	}
	if mm.mode != modeNormal {
		t.Errorf("mode = %v", mm.mode)
	}
	if follow == nil {
		t.Error("showing the new task loads its detail and agents")
	}
}

func TestATaskCreatedAfterTheHumanMovedOnIsNotShown(t *testing.T) {
	m := twoTasks(populatedModel(newFakeService()))
	m, seq := m.beginCreate()
	m, _ = m.selectTask(1) // the human went to TASK-002 meanwhile

	n, _ := m.Update(taskCreatedMsg{seq: seq, task: &domain.Task{ID: "TASK-003", State: domain.TaskCreated}})
	mm := n.(Model)
	if mm.currentTaskID != "TASK-002" {
		t.Errorf("the human moved on; the new task must not take the screen, got %s", mm.currentTaskID)
	}
	if mm.statusLine == "" {
		t.Error("the status line still says the task was created")
	}
}
