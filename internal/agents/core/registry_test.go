package core_test

import (
	"testing"

	"projectorch/internal/agents/core"
	mockagent "projectorch/internal/agents/mock"
	"projectorch/internal/domain"
)

func newTestAgent(id domain.AgentID, role domain.AgentRole) (domain.Agent, *mockagent.Adapter) {
	ad := mockagent.New(id, domain.AgentKindMock, mockagent.SilentBehavior())
	return domain.Agent{
		ID:          id,
		DisplayName: string(id),
		Kind:        domain.AgentKindMock,
		Role:        role,
		Enabled:     true,
		State:       domain.AgentStateStopped,
	}, ad
}

func TestRegistryRegisterAndGet(t *testing.T) {
	reg := core.NewRegistry()
	a, ad := newTestAgent("opus", domain.RoleAuthority)
	if err := reg.Register(a, ad); err != nil {
		t.Fatalf("register: %v", err)
	}
	got, ok := reg.Get("opus")
	if !ok {
		t.Fatal("expected agent to be found")
	}
	if got.ID != "opus" {
		t.Fatalf("got id %q", got.ID)
	}
	gotAd, ok := reg.Adapter("opus")
	if !ok || gotAd != ad {
		t.Fatal("expected same adapter back")
	}
}

func TestRegistryDuplicateRejected(t *testing.T) {
	reg := core.NewRegistry()
	a, ad := newTestAgent("opus", domain.RoleAuthority)
	if err := reg.Register(a, ad); err != nil {
		t.Fatalf("register: %v", err)
	}
	a2, ad2 := newTestAgent("opus", domain.RoleImplementer)
	if err := reg.Register(a2, ad2); err == nil {
		t.Fatal("expected duplicate registration to fail")
	}
}

func TestRegistryListSortedAndNAgents(t *testing.T) {
	reg := core.NewRegistry()
	ids := []domain.AgentID{"codex", "opus", "reviewer", "aardvark"}
	for _, id := range ids {
		a, ad := newTestAgent(id, domain.RoleObserver)
		if err := reg.Register(a, ad); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	list := reg.List()
	if len(list) != len(ids) {
		t.Fatalf("expected %d agents, got %d", len(ids), len(list))
	}
	for i := 1; i < len(list); i++ {
		if list[i-1].ID >= list[i].ID {
			t.Fatalf("list not sorted: %v", list)
		}
	}
}

func TestRegistrySetStateAndUnregister(t *testing.T) {
	reg := core.NewRegistry()
	a, ad := newTestAgent("opus", domain.RoleAuthority)
	_ = reg.Register(a, ad)

	if !reg.SetState("opus", domain.AgentStateError, "boom") {
		t.Fatal("expected SetState to succeed")
	}
	got, _ := reg.Get("opus")
	if got.State != domain.AgentStateError || got.LastError != "boom" {
		t.Fatalf("unexpected state: %+v", got)
	}

	if reg.SetState("nope", domain.AgentStateIdle, "") {
		t.Fatal("expected SetState on unknown agent to fail")
	}

	reg.Unregister("opus")
	if _, ok := reg.Get("opus"); ok {
		t.Fatal("expected agent to be gone after unregister")
	}
}
