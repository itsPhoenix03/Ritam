package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatalf("default config must be valid: %v", err)
	}
}

func TestLoadMissingFileFallsBackToDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.yaml"))
	if err != nil {
		t.Fatalf("missing config should not be an error: %v", err)
	}
	if len(cfg.Agents) != 0 {
		t.Fatalf("no agents are defined by default (the team is recruited in the TUI), got %d", len(cfg.Agents))
	}
	if !cfg.TUI.UseMockAgents {
		t.Error("the scaffold must default to mock agents so nothing reaches a provider by accident")
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "orch.yaml")
	body := `
agents:
  opus:
    type: claude-code
    role: AUTHORITY
    enabled: true
    executable: C:\tools\claude.exe
  codex:
    type: codex
    role: IMPLEMENTER
    enabled: true
  reviewer:
    type: mock
    role: REVIEWER
    enabled: false
orchestrator:
  maxConcurrentTasks: 3
  maxDeliberationRounds: 4
  maxMessages: 50
  maxTaskDuration: 5m
  defaultAuthority: opus
proxy:
  enabled: false
  host: 127.0.0.1
  port: 9000
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(cfg.Agents) != 3 {
		t.Fatalf("expected 3 agents, got %d — the system must not be hard-coded to two", len(cfg.Agents))
	}
	if cfg.Agents["opus"].Executable != `C:\tools\claude.exe` {
		t.Errorf("executable override lost: %q", cfg.Agents["opus"].Executable)
	}
	if cfg.Orchestrator.MaxDeliberationRounds != 4 {
		t.Errorf("maxDeliberationRounds = %d", cfg.Orchestrator.MaxDeliberationRounds)
	}
	if cfg.Orchestrator.MaxTaskDuration != 5*time.Minute {
		t.Errorf("maxTaskDuration = %v", cfg.Orchestrator.MaxTaskDuration)
	}
}

func TestValidateRejectsBadInput(t *testing.T) {
	cases := map[string]func(*Config){
		"unknown agent type": func(c *Config) {
			a := c.Agents["opus"]
			a.Type = "gpt-5-cli"
			c.Agents["opus"] = a
		},
		"unknown role": func(c *Config) {
			a := c.Agents["opus"]
			a.Role = "OVERLORD"
			c.Agents["opus"] = a
		},
		"authority is not an agent": func(c *Config) {
			c.Orchestrator.DefaultAuthority = "nobody"
		},
		"non-positive round limit": func(c *Config) {
			c.Orchestrator.MaxDeliberationRounds = 0
		},
		"bad proxy port": func(c *Config) {
			c.Proxy.Enabled = true
			c.Proxy.Port = 70000
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			cfg.Agents = map[string]AgentConfig{"opus": {Type: "claude-code", Role: "AUTHORITY", Enabled: true}}
			mutate(cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestResolvePath(t *testing.T) {
	got := ResolvePath(filepath.Join("C:", "z", "orch.yaml"), "knowledge.db")
	want := filepath.Join("C:", "z", "knowledge.db")
	if got != want {
		t.Errorf("ResolvePath = %q, want %q", got, want)
	}
	abs := filepath.Join(t.TempDir(), "knowledge.db")
	if ResolvePath("orch.yaml", abs) != abs {
		t.Error("absolute paths must be returned unchanged")
	}
}

func TestProvidersSayHowToRunEachCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orch.yaml")
	body := "providers:\n  codex:\n    executable: C:/tools/codex.exe\n    responseTimeout: 9m\n    allowWrites: false\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 0 {
		t.Errorf("a file with only providers defines no agents, got %d", len(cfg.Agents))
	}
	cx := cfg.Providers["codex"]
	if cx.Executable != "C:/tools/codex.exe" || cx.ResponseTimeout != 9*time.Minute || cx.MayWrite() {
		t.Errorf("codex provider = %+v", cx)
	}
	if cl := cfg.Providers["claude-code"]; cl.Executable != "claude" || !cl.MayWrite() {
		t.Errorf("an unlisted provider keeps its defaults: %+v", cl)
	}

	cfg.Providers["gemini"] = ProviderConfig{}
	if cfg.Validate() == nil {
		t.Error("an unknown provider must be rejected")
	}
}

// TestAProvidersOnlyFileLoads: the file the user keeps once the team is
// recruited in the TUI: providers and limits, no agents, and an old
// defaultAuthority line left behind does not stop orch starting.
func TestAProvidersOnlyFileLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orch.yaml")
	body := "providers:\n  claude-code:\n    executable: claude\norchestrator:\n  defaultAuthority: opus\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("a providers-only config must load: %v", err)
	}
}

// TestProjectSettingsOverrideTheGlobalOnes: the global file sets a provider
// and limits; the project's file changes one field of the provider and one
// limit, and everything else comes from the global file.
func TestProjectSettingsOverrideTheGlobalOnes(t *testing.T) {
	dir := t.TempDir()
	global := filepath.Join(dir, "global.yaml")
	project := filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(global, []byte("providers:\n  codex:\n    executable: C:/tools/codex.exe\n    responseTimeout: 9m\n"+
		"orchestrator:\n  maxTaskCostUSD: 25\n  stallMessages: 12\nbackups:\n  keep: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte("providers:\n  codex:\n    allowWrites: false\norchestrator:\n  maxTaskCostUSD: 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLayers(global, filepath.Join(dir, "absent.yaml"), project)
	if err != nil {
		t.Fatal(err)
	}
	cx := cfg.Providers["codex"]
	if cx.Executable != "C:/tools/codex.exe" || cx.ResponseTimeout != 9*time.Minute || cx.MayWrite() {
		t.Errorf("codex should keep the global command and timeout and take the project's allowWrites: %+v", cx)
	}
	if cfg.Orchestrator.MaxTaskCostUSD != 3 || cfg.Orchestrator.StallMessages != 12 || cfg.Backups.Keep != 8 {
		t.Errorf("the project's limit wins, the rest is global: %+v %+v", cfg.Orchestrator, cfg.Backups)
	}
	if !cfg.Backups.Enabled || cfg.Backups.WhenProjectDeleted != "ask" {
		t.Errorf("unset backup settings keep their defaults: %+v", cfg.Backups)
	}
}
