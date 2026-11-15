package config

import (
	"math"
	"os"
	"path/filepath"
	"strings"
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
	path := filepath.Join(dir, "ritam.yaml")
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
		"negative agent timeout": func(c *Config) {
			a := c.Agents["opus"]
			a.ResponseTimeout = -time.Second
			c.Agents["opus"] = a
		},
		"cost that is not a number": func(c *Config) {
			c.Orchestrator.MaxTaskCostUSD = math.NaN()
		},
		"unknown log level":  func(c *Config) { c.Logging.Level = "loud" },
		"unknown log format": func(c *Config) { c.Logging.Format = "xml" },
		"unknown deleted-project rule": func(c *Config) {
			c.Backups.WhenProjectDeleted = "forget"
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
	got := ResolvePath(filepath.Join("C:", "z", "ritam.yaml"), "knowledge.db")
	want := filepath.Join("C:", "z", "knowledge.db")
	if got != want {
		t.Errorf("ResolvePath = %q, want %q", got, want)
	}
	abs := filepath.Join(t.TempDir(), "knowledge.db")
	if ResolvePath("ritam.yaml", abs) != abs {
		t.Error("absolute paths must be returned unchanged")
	}
}

func TestProvidersSayHowToRunEachCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.yaml")
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
// defaultAuthority line left behind does not stop ritam starting.
func TestAProvidersOnlyFileLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.yaml")
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

// TestAnAgentEntryReplacesTheWholeAgentAcrossLayers: what ritam.example.yaml
// promises about layering. Providers merge field by field (tested above), an
// agent entry replaces the whole agent, other agents are kept, and a list is
// replaced rather than added to.
func TestAnAgentEntryReplacesTheWholeAgentAcrossLayers(t *testing.T) {
	dir := t.TempDir()
	global, project := filepath.Join(dir, "global.yaml"), filepath.Join(dir, "project.yaml")
	if err := os.WriteFile(global, []byte("agents:\n"+
		"  lead: {type: claude-code, role: AUTHORITY, executable: C:/tools/claude.exe, model: opus}\n"+
		"  helper: {type: codex, role: REVIEWER}\n"+
		"access: {commands: [go test, go vet]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte("agents:\n  lead: {type: claude-code, role: AUTHORITY, model: sonnet}\n"+
		"access: {commands: [cmake]}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLayers(global, project)
	if err != nil {
		t.Fatal(err)
	}
	if lead := cfg.Agents["lead"]; lead.Model != "sonnet" || lead.Executable != "" {
		t.Errorf("the project's entry replaces the whole agent, executable included: %+v", lead)
	}
	if _, ok := cfg.Agents["helper"]; !ok {
		t.Error("an agent the project does not list is kept")
	}
	if got := cfg.Access.Commands; len(got) != 1 || got[0] != "cmake" {
		t.Errorf("a list is replaced, not added to: %v", got)
	}
}

// TestNegativeLimitsKeepAndIntervalAreRefused: a negative value loaded
// without complaint used to switch a limit off or, for backups.keep, crash
// `ritam backups now`. Every problem is named with its key and value.
func TestNegativeLimitsKeepAndIntervalAreRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.yaml")
	body := "orchestrator:\n  maxMessages: -1\n  maxTaskCostUSD: -5\n  maxDailyTokens: -10\n  idleTimeout: -1m\n" +
		"providers:\n  codex: {responseTimeout: -5m}\n" +
		"backups:\n  keep: -1\n  interval: -10m\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("negative limits must not load")
	}
	for _, want := range []string{
		"orchestrator.maxMessages must not be negative, got -1",
		"orchestrator.maxTaskCostUSD must not be negative, got -5",
		"orchestrator.maxDailyTokens must not be negative, got -10",
		"orchestrator.idleTimeout must not be negative, got -1m0s",
		"providers.codex.responseTimeout must not be negative, got -5m0s",
		"backups.keep must be at least 1, got -1",
		"backups.interval must be at least 1m0s, or 0 to take no periodic snapshots, got -10m0s",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must say %q:\n%v", want, err)
		}
	}
}

// TestBackupSettingsThatWouldLoseBackupsAreRefused: keep 0 would delete the
// snapshot just taken; deleteAfterDays 0 would delete a "missing" project's
// backups the first time its drive is not mounted.
func TestBackupSettingsThatWouldLoseBackupsAreRefused(t *testing.T) {
	cases := map[string]struct {
		mutate func(*BackupConfig)
		want   string
	}{
		"keep none":          {func(b *BackupConfig) { b.Keep = 0 }, "backups.keep must be at least 1, got 0"},
		"interval too short": {func(b *BackupConfig) { b.Interval = time.Second }, "got 1s"},
		"delete at once": {func(b *BackupConfig) { b.WhenProjectDeleted, b.DeleteAfterDays = "delete", 0 },
			"backups.deleteAfterDays must be at least 1 with whenProjectDeleted: delete, got 0"},
		"negative days": {func(b *BackupConfig) { b.DeleteAfterDays = -3 }, "backups.deleteAfterDays must not be negative, got -3"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(&cfg.Backups)
			if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want an error saying %q, got %v", tc.want, err)
			}
		})
	}
}

// TestZeroIsTheWayToTurnALimitOrPeriodicBackupsOff: zero is documented as
// "off" and stays valid; only a negative value is a mistake.
func TestZeroIsTheWayToTurnALimitOrPeriodicBackupsOff(t *testing.T) {
	cfg := Default()
	cfg.Orchestrator.MaxTaskCostUSD = 0
	cfg.Orchestrator.MaxDailyTokens = 0
	cfg.Orchestrator.MaxTaskDuration = 0
	cfg.Backups.Interval = 0
	cfg.Backups.WhenProjectDeleted, cfg.Backups.DeleteAfterDays = "delete", 1
	if err := cfg.Validate(); err != nil {
		t.Errorf("zero limits, no periodic backups and a one-day wait are all valid: %v", err)
	}
}

// TestRealAgentsGetTheIdleFloorHoweverTheyAreChosen: the floor used to be
// applied only by the -real flag, so useMockAgents: false in a settings file
// kept the 60-second default and paused tasks mid-answer.
func TestRealAgentsGetTheIdleFloorHoweverTheyAreChosen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ritam.yaml")
	if err := os.WriteFile(path, []byte("tui:\n  useMockAgents: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ApplyRealAgentIdleFloor() || cfg.Orchestrator.IdleTimeout != RealAgentIdleFloor {
		t.Errorf("real agents from the settings get the floor: %v", cfg.Orchestrator.IdleTimeout)
	}

	mocks := Default()
	if mocks.ApplyRealAgentIdleFloor() || mocks.Orchestrator.IdleTimeout != 60*time.Second {
		t.Errorf("the mocks keep the default: %v", mocks.Orchestrator.IdleTimeout)
	}

	longer := Default()
	longer.TUI.UseMockAgents = false
	longer.Orchestrator.IdleTimeout = 30 * time.Minute
	if longer.ApplyRealAgentIdleFloor() || longer.Orchestrator.IdleTimeout != 30*time.Minute {
		t.Errorf("a longer timeout that was set is kept: %v", longer.Orchestrator.IdleTimeout)
	}
}
