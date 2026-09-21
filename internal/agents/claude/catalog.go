package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"projectorch/internal/ports"
)

// Catalog lists the models Claude Code can run: what its /model picker
// offers.
//
// The CLI has no command that prints the list (a non-interactive "/model" is
// sent to the model as a prompt), so the catalog is built from the same two
// sources the picker uses: the model tiers the CLI always offers, and the
// account-specific options it caches in ~/.claude.json
// (additionalModelOptionsCache), such as a 1M-context variant. Any full model
// id the CLI accepts can still be recruited by name.
type Catalog struct {
	// ConfigFile is Claude Code's user config, ~/.claude.json by default.
	ConfigFile string
}

// tiers are the aliases the CLI always offers, with its own descriptions.
// An alias runs the latest model of that tier.
var tiers = []ports.ModelInfo{
	{ID: "opus", Label: "Opus", Description: "Most capable for ambitious work"},
	{ID: "sonnet", Label: "Sonnet", Description: "Most efficient for everyday tasks"},
	{ID: "haiku", Label: "Haiku", Description: "Fastest for quick answers"},
	{ID: "fable", Label: "Fable", Description: "For your toughest challenges"},
}

// Provider is the agent type this catalog serves.
func (c Catalog) Provider() string { return "claude-code" }

// Models lists the tiers, then the account's extra options.
func (c Catalog) Models(_ context.Context) ([]ports.ModelInfo, error) {
	out := make([]ports.ModelInfo, 0, len(tiers)+4)
	seen := map[string]bool{}
	for _, m := range tiers {
		m.Provider = c.Provider()
		out = append(out, m)
		seen[m.ID] = true
	}
	extra, err := c.accountOptions()
	if err != nil {
		return out, nil // the tiers alone are still a usable list
	}
	for _, m := range extra {
		if !seen[m.ID] {
			seen[m.ID] = true
			out = append(out, m)
		}
	}
	return out, nil
}

func (c Catalog) accountOptions() ([]ports.ModelInfo, error) {
	path := c.ConfigFile
	if path == "" {
		dir := os.Getenv("CLAUDE_CONFIG_DIR")
		if dir == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, err
			}
			dir = home
		}
		path = filepath.Join(dir, ".claude.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg struct {
		Options []struct {
			Value       *string `json:"value"`
			Label       string  `json:"label"`
			Description string  `json:"description"`
		} `json:"additionalModelOptionsCache"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, errors.New("claude config is not valid JSON")
	}
	var out []ports.ModelInfo
	for _, o := range cfg.Options {
		if o.Value == nil || *o.Value == "" {
			continue
		}
		out = append(out, ports.ModelInfo{Provider: c.Provider(), ID: *o.Value, Label: o.Label, Description: o.Description})
	}
	return out, nil
}
