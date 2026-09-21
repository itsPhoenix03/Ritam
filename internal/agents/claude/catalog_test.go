package claude

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestTheCatalogMatchesTheModelPicker: the CLI's tiers plus the account's
// extra options from ~/.claude.json, as /model shows them.
func TestTheCatalogMatchesTheModelPicker(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), ".claude.json")
	data := `{"additionalModelOptionsCache":[{"value":"claude-fable-5-1[1m]","label":"Fable",` +
		`"description":"Fable 5.1 · Most capable for your hardest and longest-running tasks"},{"value":null,"label":"x"}]}`
	if err := os.WriteFile(cfg, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	ms, err := Catalog{ConfigFile: cfg}.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range ms {
		ids[m.ID] = true
		if m.Provider != "claude-code" {
			t.Errorf("provider = %q", m.Provider)
		}
	}
	for _, want := range []string{"opus", "sonnet", "haiku", "fable", "claude-fable-5-1[1m]"} {
		if !ids[want] {
			t.Errorf("missing %s: %+v", want, ms)
		}
	}
	if len(ms) != 5 {
		t.Errorf("an option with no value must be skipped: %d models", len(ms))
	}

	// Without the config file the tiers are still offered.
	ms, _ = Catalog{ConfigFile: filepath.Join(t.TempDir(), "missing.json")}.Models(context.Background())
	if len(ms) != 4 {
		t.Errorf("tiers alone: %d", len(ms))
	}
}
