package app

import (
	"testing"
	"time"

	"projectorch/internal/config"
)

// TestRecruitsAreStartedTheWayTheProviderSays: a recruit takes its command,
// timeouts and extra commands from the provider's entry in orch.yaml.
func TestRecruitsAreStartedTheWayTheProviderSays(t *testing.T) {
	cfg := config.Default()
	cfg.Providers["codex"] = config.ProviderConfig{Executable: "C:/tools/codex.exe",
		ResponseTimeout: 9 * time.Minute, AllowedCommands: []string{"g++"}}
	ac := templateFor(cfg, "codex")
	if ac.Executable != "C:/tools/codex.exe" || ac.ResponseTimeout != 9*time.Minute ||
		len(ac.AllowedCommands) != 1 || ac.AllowedCommands[0] != "g++" {
		t.Errorf("codex template = %+v", ac)
	}

	cfg.Providers["claude-code"] = config.ProviderConfig{}
	if ac := templateFor(cfg, "claude-code"); ac.Executable != "claude" || ac.ResponseTimeout != 5*time.Minute {
		t.Errorf("an empty provider entry falls back to the usual command: %+v", ac)
	}

	no := false
	if (config.ProviderConfig{AllowWrites: &no}).MayWrite() || !(config.ProviderConfig{}).MayWrite() {
		t.Error("allowWrites: false makes a provider read-only; unset leaves it able to write")
	}
}
