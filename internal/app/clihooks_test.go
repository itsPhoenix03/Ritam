package app

import (
	"os"
	"path/filepath"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/review/facts"
)

const userClaudeSettings = `{
  "model": "opus",
  "permissions": {"allow": ["Bash(*)"]},
  "hooks": {
    "PreToolUse": [{"matcher": "Bash", "hooks": [{"type": "command", "command": "python ~/hooks/guard.py", "timeout": 60}]}],
    "PostToolUse": [{"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "prettier --write"}]}]
  }
}`

// TestYourClaudeHooksComeBackAsChosen: on keeps the pre-tool hooks for the
// review to run and the rest native; native keeps them all native; off
// keeps them out. Nothing but the hooks comes back: not the user's allow
// rules.
func TestYourClaudeHooksComeBackAsChosen(t *testing.T) {
	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, []byte(userClaudeSettings), 0o600); err != nil {
		t.Fatal(err)
	}
	on := loadCLIHooks("on", file, t.TempDir())
	if on.err != nil || len(on.native) != 1 || on.native["PostToolUse"] == nil || len(on.review) != 1 {
		t.Fatalf("on: %+v", on)
	}
	h := on.review[0]
	if !h.Native || !h.Wants("Bash", facts.ToolShell, domain.AgentKindClaudeCode) ||
		h.Wants("Read", facts.ToolRead, domain.AgentKindClaudeCode) || h.Wants("Bash", facts.ToolShell, domain.AgentKindCodex) {
		t.Errorf("the pre-tool hook keeps its matcher and is Claude Code's only: %+v", h)
	}
	if h.Timeout > 15e9 || h.Block {
		t.Errorf("bounded, and like Claude Code a failure is no objection: %+v", h)
	}
	native := loadCLIHooks("native", file, "")
	if len(native.native) != 2 || len(native.review) != 0 {
		t.Errorf("native: %+v", native)
	}
	if off := loadCLIHooks("off", file, ""); off.native != nil || off.review != nil {
		t.Errorf("off: %+v", off)
	}
	bad := filepath.Join(t.TempDir(), "settings.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if b := loadCLIHooks("on", bad, ""); b.err == nil || b.native != nil {
		t.Errorf("unreadable: %+v", b)
	}
	if missing := loadCLIHooks("on", filepath.Join(t.TempDir(), "none.json"), ""); missing.err != nil {
		t.Errorf("no settings file is no error: %v", missing.err)
	}
}
