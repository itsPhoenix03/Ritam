package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"projectorch/internal/config"
	"projectorch/internal/domain"
	"projectorch/internal/logging"
	"projectorch/internal/ports"
)

type fixedCatalog []ports.ModelInfo

func (f fixedCatalog) Provider() string                                  { return "claude-code" }
func (f fixedCatalog) Models(context.Context) ([]ports.ModelInfo, error) { return f, nil }

func testTeam(t *testing.T) *Team {
	t.Helper()
	cfg := config.Default()
	cfg.Storage.Path = filepath.Join(t.TempDir(), "orch.db")
	tm := &Team{path: teamFile(cfg), cfg: cfg, log: logging.NewDiscard(),
		catalogs: []ports.ModelCatalog{fixedCatalog{
			{Provider: "claude-code", ID: "opus"},
			{Provider: "claude-code", ID: "claude-fable-5-1"},
			{Provider: "claude-code", ID: "legacy", Hidden: true},
		}}}
	tm.loadAvailability()
	return tm
}

// TestAModelThatFailedIsRememberedAsUnavailable: a turn that showed the
// account cannot run a model marks it, and the mark survives a restart.
func TestAModelThatFailedIsRememberedAsUnavailable(t *testing.T) {
	tm := testTeam(t)
	tm.MarkUnavailable("claude-code", "claude-fable-5-1", domain.FailurePlan, domain.FailurePlan.Advice())
	tm.MarkUnavailable("claude-code", "opus", domain.FailureLimit, "limit") // a limit lifts by itself: not marked

	again := &Team{path: tm.path, cfg: tm.cfg, log: tm.log, catalogs: tm.catalogs}
	again.loadAvailability()
	ms, err := again.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 {
		t.Fatalf("hidden models are left out: %+v", ms)
	}
	for _, m := range ms {
		switch m.ID {
		case "claude-fable-5-1":
			if !m.Unavailable || m.Reason == "" {
				t.Errorf("fable should be remembered as unavailable: %+v", m)
			}
		case "opus":
			if m.Unavailable {
				t.Errorf("a usage limit must not mark a model unavailable: %+v", m)
			}
		}
	}
}

func TestCheckingAModel(t *testing.T) {
	tm := testTeam(t)
	tm.cfg.TUI.UseMockAgents = false
	tm.probe = func(ctx context.Context, provider, model string) (bool, domain.FailureKind, string, error) {
		switch model {
		case "opus":
			return true, "", "", nil
		case "claude-fable-5-1":
			return false, domain.FailureCredits, "Credit balance is too low", errors.New("exit status 1")
		}
		return false, domain.FailureAuth, "not logged in", errors.New("exit status 1")
	}
	ctx := context.Background()
	if info, err := tm.CheckModel(ctx, "claude-code", "opus"); err != nil || !info.Checked {
		t.Errorf("opus: %+v %v", info, err)
	}
	if info, err := tm.CheckModel(ctx, "claude-code", "claude-fable-5-1"); err != nil || !info.Unavailable {
		t.Errorf("fable without credits: %+v %v", info, err)
	}
	if _, err := tm.CheckModel(ctx, "claude-code", "sonnet"); err == nil {
		t.Error("a check that could not run (not logged in) must say so rather than judge the model")
	}
	ms, _ := tm.Models(ctx)
	for _, m := range ms {
		if m.ID == "opus" && !m.Checked {
			t.Error("a passed check is shown")
		}
	}
}

func TestJudgingACheckCall(t *testing.T) {
	ok := `{"type":"result","subtype":"success","is_error":false,"result":"OK","usage":{"cache_read_input_tokens":14290}}`
	if pass, _, _, _ := judgeCheck("claude-code", ok, "", nil); !pass {
		t.Error("a successful claude call passes, whatever numbers its output carries")
	}
	bad := `{"type":"result","subtype":"success","is_error":true,"result":"Credit balance is too low"}`
	if pass, kind, _, _ := judgeCheck("claude-code", bad, "", errors.New("exit status 1")); pass || kind != domain.FailureCredits {
		t.Errorf("an error result is classified: pass=%v kind=%s", pass, kind)
	}
	if pass, kind, _, _ := judgeCheck("codex", "", "The model `gpt-9` does not exist or you do not have access to it.", errors.New("exit status 1")); pass || kind != domain.FailureModel {
		t.Errorf("codex failure: pass=%v kind=%s", pass, kind)
	}
	if pass, _, _, _ := judgeCheck("codex", "OK", "", nil); !pass {
		t.Error("a clean codex run passes")
	}
}

func TestTheCachedListShowsNewMarks(t *testing.T) {
	tm := testTeam(t)
	ctx := context.Background()
	if _, err := tm.Models(ctx); err != nil { // fills the cache
		t.Fatal(err)
	}
	tm.MarkUnavailable("claude-code", "opus", domain.FailureCredits, "out of credits")
	ms, _ := tm.Models(ctx)
	for _, m := range ms {
		if m.ID == "opus" && !m.Unavailable {
			t.Error("a mark made after the list was cached must still show")
		}
	}
}
