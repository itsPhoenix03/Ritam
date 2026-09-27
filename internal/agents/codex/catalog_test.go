package codex

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// catalogJSON is trimmed from `codex debug models` (codex-cli 0.159.2).
const catalogJSON = `{"fetched_at":"x","models":[
 {"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","description":"Latest workhorse model for coding and everyday work.","visibility":"list"},
 {"slug":"gpt-6-astra","display_name":"GPT-6-Astra","description":"Frontier intelligence for the most demanding work.","visibility":"list"},
 {"slug":"gpt-5.5","display_name":"GPT-5.5","description":"Legacy coding model.","visibility":"hide"}]}`

func TestTheCatalogListsEveryModelAndMarksHiddenOnes(t *testing.T) {
	ms, err := parseCatalog([]byte(catalogJSON))
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 3 || ms[0].ID != "gpt-6.1-sol" || ms[0].Label != "GPT-6.1-Sol" || ms[0].Provider != "codex" {
		t.Fatalf("parsed %+v", ms)
	}
	if ms[0].Hidden || !ms[2].Hidden {
		t.Errorf("only models the CLI hides are marked hidden: %+v", ms)
	}
}

// TestTheCatalogFallsBackToTheCLIsCache: without a working `codex debug
// models` the copy the CLI cached is used.
func TestTheCatalogFallsBackToTheCLIsCache(t *testing.T) {
	cache := filepath.Join(t.TempDir(), "models_cache.json")
	if err := os.WriteFile(cache, []byte(catalogJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	ms, err := Catalog{Executable: filepath.Join(t.TempDir(), "no-such-codex"), CacheFile: cache}.Models(context.Background())
	if err != nil || len(ms) != 3 {
		t.Fatalf("models from the cache: %d, %v", len(ms), err)
	}
}
