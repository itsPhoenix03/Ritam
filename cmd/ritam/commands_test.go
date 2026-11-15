package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ritam/internal/backup"
	"ritam/internal/brand"
	"ritam/internal/config"
)

// writeGlobal writes the global settings file in the app home.
func writeGlobal(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, brand.ConfigFile()), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestBackupsNowKeepsTheBackupItTook: with keep 1 and a snapshot dated in
// the future (a clock set back since), pruning after `backups now` used to
// delete the backup just asked for.
func TestBackupsNowKeepsTheBackupItTook(t *testing.T) {
	home, _ := inProject(t)
	writeGlobal(t, home, "backups:\n  keep: 1\n")
	_, pl, err := prepare("")
	if err != nil {
		t.Fatal(err)
	}
	dir := pl.home.BackupDir(pl.proj)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	future := filepath.Join(dir, "20991231-235959.000-periodic.db.gz")
	if err := os.WriteFile(future, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if _, err := runCommand([]string{"backups", "now"}, &out); err != nil {
		t.Fatalf("backups now: %v", err)
	}
	taken := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(out.String(), "\n", 2)[0], "backup taken:"))
	if _, err := os.Stat(taken); err != nil {
		t.Errorf("the backup just taken must survive pruning (%s): %v\n%s", taken, err, out.String())
	}
	if list, _ := backup.List(dir); len(list) != 2 {
		t.Errorf("the newest and the one just taken are kept: %+v", list)
	}
}

// TestASettingsMistakeStopsEveryCommand: a negative keep used to reach
// `backups now` and panic; now the settings do not load.
func TestASettingsMistakeStopsEveryCommand(t *testing.T) {
	home, _ := inProject(t)
	writeGlobal(t, home, "backups:\n  keep: -1\n")
	var out bytes.Buffer
	_, err := runCommand([]string{"backups", "now"}, &out)
	if err == nil || !strings.Contains(err.Error(), "backups.keep must be at least 1, got -1") {
		t.Errorf("want the settings error, got %v", err)
	}
}

// TestConfigCheckShowsTheSettingsAndWhereTheyCameFrom: the global file, the
// project's, what was held back and the idle floor are each named.
func TestConfigCheckShowsTheSettingsAndWhereTheyCameFrom(t *testing.T) {
	home, project := inProject(t)
	writeGlobal(t, home, "orchestrator:\n  stallMessages: 11\ntui:\n  useMockAgents: false\n")
	if err := os.WriteFile(filepath.Join(project, brand.ConfigFile()),
		[]byte("orchestrator:\n  maxTaskCostUSD: 2\nbackups:\n  keep: 9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if ok, err := runCommand([]string{"config", "check"}, &out); !ok || err != nil {
		t.Fatalf("config check: %v %v", ok, err)
	}
	got := out.String()
	for _, want := range []string{
		"stallMessages: 11    <- global (" + filepath.Join(home, brand.ConfigFile()) + ")",
		"maxTaskCostUSD: 2    <- project (" + filepath.Join(project, brand.ConfigFile()) + ")",
		"backups.keep: 5    <- the project's value was held back",
		"orchestrator.idleTimeout: 10m0s    <- raised to 10m0s for the real CLIs",
		"orchestrator.maxDailyCostUSD: 50\n",
		"held back from the project's settings",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the output must hold %q:\n%s", want, got)
		}
	}

	if _, err := runCommand([]string{"config", "bogus"}, &out); err == nil {
		t.Error("an unknown config command is an error")
	}
}

// TestRealAgentsFromTheSettingsGetTheIdleFloorWithoutTheFlag: only -real
// used to raise the idle timeout; settings choosing real agents did not.
func TestRealAgentsFromTheSettingsGetTheIdleFloorWithoutTheFlag(t *testing.T) {
	home, _ := inProject(t)
	writeGlobal(t, home, "tui:\n  useMockAgents: false\n")
	cfg, _, err := prepare("")
	if err != nil {
		t.Fatal(err)
	}
	if err := chooseAgents(cfg, false, false); err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.UseMockAgents || cfg.Orchestrator.IdleTimeout != config.RealAgentIdleFloor {
		t.Errorf("real agents without -real get the floor: mocks %v, idle %v",
			cfg.TUI.UseMockAgents, cfg.Orchestrator.IdleTimeout)
	}

	cfg, _, err = prepare("")
	if err != nil {
		t.Fatal(err)
	}
	if err := chooseAgents(cfg, false, true); err != nil {
		t.Fatal(err)
	}
	if !cfg.TUI.UseMockAgents || cfg.Orchestrator.IdleTimeout == config.RealAgentIdleFloor {
		t.Errorf("-mock wins and keeps the mocks' timeout: mocks %v, idle %v",
			cfg.TUI.UseMockAgents, cfg.Orchestrator.IdleTimeout)
	}
	if chooseAgents(cfg, true, true) == nil {
		t.Error("-real and -mock together are an error")
	}
}
