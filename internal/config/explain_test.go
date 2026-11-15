package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExplainSaysWhereEachChangedValueCameFrom: the global file, the project
// file that overrides it, a value held back, a path set at start, and the
// defaults with no note.
func TestExplainSaysWhereEachChangedValueCameFrom(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "project")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	global, project := filepath.Join(dir, "global.yaml"), filepath.Join(root, "ritam.yaml")
	if err := os.WriteFile(global, []byte("orchestrator: {stallMessages: 12, maxMessages: 300}\n"+
		"agents:\n  lead: {type: claude-code, role: AUTHORITY, model: opus}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project, []byte("orchestrator: {maxMessages: 100, maxTotalMessages: 5000}\n"+
		"agents:\n  lead: {type: claude-code, role: AUTHORITY}\nbackups: {keep: 9}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	layers := []Layer{{"global", global}, {"project", filepath.Join(root, "absent.yaml")}, {"project", project}}
	g, err := LoadLayers(global)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadLayers(global, layers[1].Path, project)
	if err != nil {
		t.Fatal(err)
	}
	held := cfg.RestrictProject(g, root)
	cfg.Storage.Path = filepath.Join(root, ".ritam", "ritam.db") // as prepare sets it
	settings, err := Explain(cfg, layers, held, map[string]string{"orchestrator.idleTimeout": "raised"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Setting{}
	for _, s := range settings {
		got[s.Key] = s
	}
	want := map[string]string{
		"orchestrator.stallMessages":    "global (" + global + ")",
		"orchestrator.maxMessages":      "project (" + project + ")",
		"orchestrator.maxTotalMessages": "the project's value was held back",
		"agents.lead.model":             "project (" + project + ")", // the entry replaced the whole agent
		"backups.keep":                  "the project's value was held back",
		"storage.path":                  "set at start",
		"orchestrator.idleTimeout":      "raised",
		"orchestrator.maxTaskCostUSD":   "",
	}
	for key, source := range want {
		s, ok := got[key]
		if !ok {
			t.Errorf("%s is not listed", key)
			continue
		}
		if s.Source != source {
			t.Errorf("%s = %s: source %q, want %q", key, s.Value, s.Source, source)
		}
	}
	if got["orchestrator.maxMessages"].Value != "100" || got["backups.keep"].Value != "5" {
		t.Errorf("the values in force are shown: %+v %+v", got["orchestrator.maxMessages"], got["backups.keep"])
	}
	if !strings.HasPrefix(settings[0].Key, "providers.") {
		t.Errorf("settings are listed in file order, providers first: %s", settings[0].Key)
	}
}
