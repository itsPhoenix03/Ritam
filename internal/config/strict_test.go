package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeSettings(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ritam.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAMistypedKeyFailsToLoadAndSuggestsTheRightOne: a wrongly capitalised
// limit used to be ignored, leaving the $50 default in force while the user
// believed they had set $5.
func TestAMistypedKeyFailsToLoadAndSuggestsTheRightOne(t *testing.T) {
	path := writeSettings(t, "orchestrator:\n  maxTaskCostUSD: 2\n  maxDailyCostUsd: 5\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("a file with an unknown key must not load")
	}
	for _, want := range []string{path, "orchestrator.maxDailyCostUsd", "line 3", "did you mean maxDailyCostUSD?"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error must name %q: %v", want, err)
		}
	}
}

// TestUnknownKeysAreRefusedAtEveryLevel: inside a provider, an agent and a
// list, and with a suggestion only when one is close.
func TestUnknownKeysAreRefusedAtEveryLevel(t *testing.T) {
	cases := map[string]struct{ body, key, hint string }{
		"top level":      {"tuii: {useMockAgents: false}\n", "tuii", "did you mean tui?"},
		"provider entry": {"providers:\n  codex: {excutable: codex}\n", "providers.codex.excutable", "did you mean executable?"},
		"agent entry":    {"agents:\n  lead: {type: mock, role: AUTHORITY, allowwrite: true}\n", "agents.lead.allowwrite", "did you mean allowWrites?"},
		"wrong level":    {"idleTimeout: 10m\n", "idleTimeout", "it belongs under orchestrator.idleTimeout"},
		"far from any":   {"backups:\n  rotation: weekly\n", "backups.rotation", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeSettings(t, tc.body))
			if err == nil {
				t.Fatal("an unknown key must not load")
			}
			if !strings.Contains(err.Error(), "unknown setting "+tc.key) {
				t.Errorf("the error must name %s: %v", tc.key, err)
			}
			if tc.hint != "" && !strings.Contains(err.Error(), tc.hint) {
				t.Errorf("the error must say %q: %v", tc.hint, err)
			}
			if tc.hint == "" && strings.Contains(err.Error(), "did you mean") {
				t.Errorf("no key is close, so none is suggested: %v", err)
			}
		})
	}
}

// TestTheKnowledgeSectionStillLoads: older files carry a knowledge section;
// it has no effect, but refusing it would stop those files loading.
func TestTheKnowledgeSectionStillLoads(t *testing.T) {
	path := writeSettings(t, "knowledge:\n  backend: local\n  path: \"\"\ntui:\n  useMockAgents: false\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("a file with a knowledge section must load: %v", err)
	}
	if cfg.TUI.UseMockAgents {
		t.Error("the rest of the file still applies")
	}
}

// TestAnEmptyOrCommentOnlyFileLoads: a settings file may hold nothing yet.
func TestAnEmptyOrCommentOnlyFileLoads(t *testing.T) {
	for _, body := range []string{"", "# nothing here yet\n"} {
		cfg, err := Load(writeSettings(t, body))
		if err != nil {
			t.Fatalf("%q: %v", body, err)
		}
		if cfg.Orchestrator.MaxDailyCostUSD != 50 {
			t.Errorf("an empty file keeps the defaults: %+v", cfg.Orchestrator)
		}
	}
}

// TestAnchorsAndMergeKeysAreCheckedToo: YAML's own reuse features cannot
// carry an unknown key past the check, and valid ones still work.
func TestAnchorsAndMergeKeysAreCheckedToo(t *testing.T) {
	ok := "providers:\n  claude-code: &p {responseTimeout: 7m}\n  codex:\n    <<: *p\n    executable: codex\n"
	cfg, err := Load(writeSettings(t, ok))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Providers["codex"].ResponseTimeout != 7*time.Minute {
		t.Errorf("the merged timeout applies: %+v", cfg.Providers["codex"])
	}
	bad := "providers:\n  claude-code: &p {responseTimeot: 7m}\n  codex:\n    <<: *p\n"
	if _, err := Load(writeSettings(t, bad)); err == nil || !strings.Contains(err.Error(), "did you mean responseTimeout?") {
		t.Errorf("an unknown key behind an anchor is refused: %v", err)
	}
}

// TestTheShippedSettingsFilesLoadStrictly: the example file, and the local
// ritam.yaml when this checkout has one, use only keys ritam knows.
func TestTheShippedSettingsFilesLoadStrictly(t *testing.T) {
	if _, err := Load(filepath.Join("..", "..", "ritam.example.yaml")); err != nil {
		t.Errorf("ritam.example.yaml: %v", err)
	}
	local := filepath.Join("..", "..", "ritam.yaml")
	if _, err := os.Stat(local); err != nil {
		return // ritam.yaml is local and not checked in
	}
	if _, err := Load(local); err != nil {
		t.Errorf("ritam.yaml: %v", err)
	}
}
