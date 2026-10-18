package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// runAccess feeds a command's result back into the model, following the
// access settings' own messages.
func runAccess(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			m = runAccess(t, m, c)
		}
	case accessLoadedMsg, accessChangedMsg:
		next, more := m.Update(msg)
		m = runAccess(t, next.(Model), more)
	}
	return m
}

func settingsModel(t *testing.T, info ports.AccessInfo) (Model, *fakeService) {
	t.Helper()
	svc := newFakeService()
	svc.access = info
	m := populatedModel(svc)
	m.width, m.height = 160, 50
	next, cmd := m.runCommand("settings")
	return runAccess(t, next, cmd), svc
}

func TestTheSettingsTabShowsTheLevelsAndTheRisk(t *testing.T) {
	m, _ := settingsModel(t, ports.AccessInfo{Level: domain.AccessFilesOnly, Commands: []string{"cmake"},
		Builders: []domain.AgentID{"codex"}, File: "/p/.ritam/ritam.yaml"})
	if m.centerOrDefault() != tabSettings {
		t.Fatalf(":settings opens the settings tab, got %s", m.centerOrDefault())
	}
	view := stripANSI(m.View())
	for _, want := range []string{"Files only", "Sandboxed builder", "Build and test", "Any command",
		"cmake", "codex", "ALWAYS BLOCKED", "killing processes", "no sandbox", "I trust these agents", "L0 files only"} {
		if !strings.Contains(view, want) {
			t.Errorf("settings tab missing %q", want)
		}
	}
}

func TestRaisingTheLevelAsksFirst(t *testing.T) {
	m, svc := settingsModel(t, ports.AccessInfo{Level: domain.AccessFilesOnly, Commands: []string{"cmake"}})

	m, _ = key(m, "j")
	m, _ = key(m, "j") // Build and test
	m, cmd := key(m, "enter")
	if cmd != nil || m.mode != modePrompt || m.prompt.action != ActionSetAccess {
		t.Fatalf("raising opens a confirmation, mode %v action %q", m.mode, m.prompt.action)
	}
	if !strings.Contains(m.prompt.label, "no sandbox") || !strings.Contains(m.prompt.label, "cmake") {
		t.Errorf("the confirmation states the risk: %q", m.prompt.label)
	}

	// Anything but yes leaves the level alone.
	m, _ = key(m, "no")
	m, cmd = key(m, "enter")
	m = runAccess(t, m, cmd)
	if svc.calls["SetAccess"] != 0 || m.access.Level != domain.AccessFilesOnly {
		t.Fatalf("a refused raise changes nothing: calls %d level %d", svc.calls["SetAccess"], m.access.Level)
	}

	m, _ = key(m, "enter")
	m, _ = key(m, "yes")
	m, cmd = key(m, "enter")
	m = runAccess(t, m, cmd)
	if svc.calls["SetAccess"] != 1 || m.access.Level != domain.AccessBuildAndTest {
		t.Fatalf("yes raises the level: calls %d level %d", svc.calls["SetAccess"], m.access.Level)
	}
	if !strings.Contains(stripANSI(m.View()), "L2 build and test") {
		t.Error("the header shows the new level")
	}
}

func TestLoweringTheLevelAppliesAtOnce(t *testing.T) {
	m, svc := settingsModel(t, ports.AccessInfo{Level: domain.AccessAnyCommand})
	next, cmd := m.runCommand("access 0")
	m = runAccess(t, next, cmd)
	if m.mode == modePrompt || svc.calls["SetAccess"] != 1 || m.access.Level != domain.AccessFilesOnly {
		t.Errorf("lowering needs no confirmation: mode %v calls %d level %d", m.mode, svc.calls["SetAccess"], m.access.Level)
	}
}

func TestAllowAndDisallowEditTheCommandList(t *testing.T) {
	m, svc := settingsModel(t, ports.AccessInfo{Level: domain.AccessBuildAndTest, Commands: []string{"cmake"}})
	next, cmd := m.runCommand("allow ctest")
	m = runAccess(t, next, cmd)
	if !slices.Equal(svc.access.Commands, []string{"cmake", "ctest"}) {
		t.Fatalf("allow adds a command: %v", svc.access.Commands)
	}
	next, cmd = m.runCommand("disallow cmake")
	m = runAccess(t, next, cmd)
	if !slices.Equal(m.access.Commands, []string{"ctest"}) || m.access.Level != domain.AccessBuildAndTest {
		t.Errorf("disallow removes one and keeps the level: %+v", m.access)
	}
}

func TestALockedLevelCannotBeRaised(t *testing.T) {
	m, svc := settingsModel(t, ports.AccessInfo{Level: domain.AccessFilesOnly, Locked: "a global launch stays at level 0"})
	next, _ := m.runCommand("access 3")
	if next.mode == modePrompt || svc.calls["SetAccess"] != 0 || !strings.Contains(next.statusLine, "global launch") {
		t.Errorf("a locked level says why and changes nothing: mode %v status %q", next.mode, next.statusLine)
	}
}

// TestTheTaskFormChoosesTheBuilder: Auto by default; left/right cycles
// Manual and each participant, explained under the workspace's access level.
func TestTheTaskFormChoosesTheBuilder(t *testing.T) {
	svc := teamService()
	svc.models[3].Builds = true // gpt-6.1-sol passed the sandbox check
	svc.access = ports.AccessInfo{Level: domain.AccessSandboxedBuilder, Commands: []string{"cmake"}}
	m := populatedModel(svc)
	m.width, m.height = 140, 60
	m = runAccess(t, m, loadAccessCmd(m.ctx, svc))
	next, cmd := key(m, "n")
	m = run(t, next, cmd)
	for _, r := range "Build it" {
		m, _ = key(m, string(r))
	}
	m.form.focus = fieldRecruit
	m, _ = key(m, " ") // opus
	m, _ = key(m, "down")
	m, _ = key(m, "down")
	m, _ = key(m, " ") // gpt-6.1-sol
	m.form.focus = fieldParticipants
	m, _ = key(m, " ") // opus-1 is the authority

	view := stripANSI(m.View())
	for _, want := range []string{"builds & tests", "Builder: ", "auto", "gpt-6-1-sol-1 build"} {
		if !strings.Contains(view, want) {
			t.Errorf("form missing %q:\n%s", want, view)
		}
	}

	m.form.focus = fieldBuilder
	m, _ = key(m, "right")
	if m.formBuilder() != domain.BuilderManual || !strings.Contains(stripANSI(m.View()), "You run the builds") {
		t.Fatalf("right picks Manual: %q", m.formBuilder())
	}
	m, _ = key(m, "right")
	if m.formBuilder() != "opus-1" || !strings.Contains(stripANSI(m.View()), "has not passed the sandbox check") {
		t.Errorf("then each participant, warned when it cannot build at level 1: %q", m.formBuilder())
	}
	m, _ = key(m, "right")
	if m.formBuilder() != "gpt-6-1-sol-1" {
		t.Fatalf("builder = %q", m.formBuilder())
	}
	next, cmd = key(m, "enter")
	if cmd == nil {
		t.Fatalf("submit: %s", next.statusLine)
	}
	cmd()
	if svc.created == nil || svc.created.Builder != "gpt-6-1-sol-1" {
		t.Errorf("the task is created with the builder chosen: %+v", svc.created)
	}
}
