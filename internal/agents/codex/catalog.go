package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"projectorch/internal/ports"
)

// Catalog lists the models Codex can run, from the CLI's own catalog:
// `codex debug models` prints it as JSON, and ~/.codex/models_cache.json
// holds the copy the CLI last fetched, used when the command is unavailable.
type Catalog struct {
	// Executable is the codex command, "codex" by default.
	Executable string
	// CacheFile is the CLI's model cache, ~/.codex/models_cache.json by
	// default.
	CacheFile string
}

// catalogTimeout bounds `codex debug models`.
var catalogTimeout = 30 * time.Second

// Provider is the agent type this catalog serves.
func (c Catalog) Provider() string { return "codex" }

// Models lists the CLI's models, the ones it hides marked Hidden.
func (c Catalog) Models(ctx context.Context) ([]ports.ModelInfo, error) {
	exe := c.Executable
	if exe == "" {
		exe = "codex"
	}
	ctx, cancel := context.WithTimeout(ctx, catalogTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, exe, "debug", "models").Output()
	if err == nil {
		if models, perr := parseCatalog(out); perr == nil && len(models) > 0 {
			return models, nil
		}
	}
	path := c.CacheFile
	if path == "" {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return nil, fmt.Errorf("codex models: %w", herr)
		}
		path = filepath.Join(home, ".codex", "models_cache.json")
	}
	data, rerr := os.ReadFile(path)
	if rerr != nil {
		if err != nil {
			return nil, fmt.Errorf("codex models: %v; no cache either: %w", err, rerr)
		}
		return nil, fmt.Errorf("codex models: %w", rerr)
	}
	return parseCatalog(data)
}

// parseCatalog reads the catalog JSON both sources share: {"models": [...]}.
func parseCatalog(data []byte) ([]ports.ModelInfo, error) {
	var doc struct {
		Models []struct {
			Slug        string `json:"slug"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
			Visibility  string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("codex models: %w", err)
	}
	out := make([]ports.ModelInfo, 0, len(doc.Models))
	for _, m := range doc.Models {
		if m.Slug == "" {
			continue
		}
		out = append(out, ports.ModelInfo{
			Provider: "codex", ID: m.Slug, Label: m.DisplayName, Description: m.Description,
			Hidden: m.Visibility != "" && m.Visibility != "list",
		})
	}
	return out, nil
}
