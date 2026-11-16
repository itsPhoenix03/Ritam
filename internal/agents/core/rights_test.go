package core_test

import (
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// TestAgentRightsFollowTheAccessPolicy: the roster and the bus's check read
// each agent's rights from the same policy that writes its brief. A
// sandboxed writer builds; a writer without a sandbox does not; a read-only
// agent neither writes nor builds, even with a sandbox; a mock has no rights
// to report.
func TestAgentRightsFollowTheAccessPolicy(t *testing.T) {
	reg := core.NewRegistry()
	for id, kind := range map[domain.AgentID]domain.AgentKind{
		"opus-1": domain.AgentKindClaudeCode, "sonnet-1": domain.AgentKindClaudeCode,
		"codex-1": domain.AgentKindCodex, "mock-1": domain.AgentKindMock,
	} {
		if err := reg.RegisterFactory(domain.Agent{ID: id, Kind: kind}, nil); err != nil {
			t.Fatal(err)
		}
	}
	mgr := core.NewManager(reg, nil)
	task := &domain.Task{ID: "TASK-1", AuthorityAgent: "opus-1",
		Participants: []domain.AgentID{"opus-1", "sonnet-1", "codex-1", "mock-1"}}
	if _, known := mgr.AgentRights(task, "opus-1"); known {
		t.Fatal("without an access policy there are no rights to report")
	}

	acc := core.NewAccess(domain.AccessBuildAndTest, []string{"go test"})
	acc.SetWrites("opus-1", true)
	acc.SetWrites("codex-1", true)
	acc.SetSandboxed("codex-1", true)
	acc.SetSandboxed("sonnet-1", true)
	mgr.UseAccess(acc)

	want := map[domain.AgentID]ports.AgentRights{
		"opus-1":   {Writes: true},
		"sonnet-1": {},
		"codex-1":  {Writes: true, Builds: true},
	}
	for id, w := range want {
		if got, known := mgr.AgentRights(task, id); !known || got != w {
			t.Errorf("%s: rights = %+v (known %v), want %+v", id, got, known, w)
		}
	}
	if _, known := mgr.AgentRights(task, "mock-1"); known {
		t.Error("a mock runs no CLI: its rights must not be reported")
	}
}
