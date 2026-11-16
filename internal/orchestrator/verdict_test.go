package orchestrator_test

import (
	"errors"
	"strings"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/orchestrator"
)

func verdictTask() *domain.Task {
	return &domain.Task{
		ID: "TASK-001", AuthorityAgent: "opus", State: domain.TaskDeliberating,
		Participants: []domain.AgentID{"opus", "scout", "codex"},
	}
}

// TestSupportingApproveIsRefusedWithTheWayToRecover is the failure seen in a
// real run: asked by a human to verify something, scout sent APPROVE. It must
// still be refused -- APPROVE belongs to the authority -- but the refusal now
// says what to send instead.
func TestSupportingApproveIsRefusedWithTheWayToRecover(t *testing.T) {
	auth := orchestrator.NewAuthorityManager()
	err := auth.Authorize(verdictTask(), &domain.Message{FromAgent: "scout", ToAgent: "human", Type: domain.MsgApprove})

	var v *orchestrator.Violation
	if !errors.As(err, &v) || v.Kind != orchestrator.ViolationAuthority {
		t.Fatalf("want an authority violation, got %v", err)
	}
	if !strings.Contains(v.Reason, "REVIEW with verdict=approve") {
		t.Errorf("the refusal must name the legitimate alternative, got %q", v.Reason)
	}
}

func TestAReviewVerdictIsAcceptedAndNormalised(t *testing.T) {
	auth := orchestrator.NewAuthorityManager()
	m := &domain.Message{FromAgent: "scout", ToAgent: "human", Type: domain.MsgReview,
		Metadata: map[string]string{domain.MetaVerdict: "Approved"}}
	if err := auth.Authorize(verdictTask(), m); err != nil {
		t.Fatalf("a supporting agent's REVIEW verdict must be allowed: %v", err)
	}
	if got := m.Metadata[domain.MetaVerdict]; got != string(domain.VerdictApprove) {
		t.Errorf("verdict spelling should be normalised to %q, got %q", domain.VerdictApprove, got)
	}
}

func TestAnUnknownVerdictIsRefused(t *testing.T) {
	auth := orchestrator.NewAuthorityManager()
	err := auth.Authorize(verdictTask(), &domain.Message{FromAgent: "scout", ToAgent: "opus", Type: domain.MsgReview,
		Metadata: map[string]string{domain.MetaVerdict: "maybe"}})
	var v *orchestrator.Violation
	if !errors.As(err, &v) || !strings.Contains(v.Reason, "approve, changes or reject") {
		t.Errorf("an unknown verdict must be refused with the valid values, got %v", err)
	}
}

func TestAVerdictOnAnotherTypeIsDroppedNotRefused(t *testing.T) {
	auth := orchestrator.NewAuthorityManager()
	m := &domain.Message{FromAgent: "scout", ToAgent: "opus", Type: domain.MsgAnswer,
		Metadata: map[string]string{domain.MetaVerdict: "approve"}}
	if err := auth.Authorize(verdictTask(), m); err != nil {
		t.Fatalf("the message itself is valid: %v", err)
	}
	if _, ok := m.Metadata[domain.MetaVerdict]; ok {
		t.Error("a verdict only means something on a REVIEW; it should be dropped elsewhere")
	}
}

func TestEveryAuthorityOnlyTypeHasAnAlternative(t *testing.T) {
	for _, mt := range domain.AllMessageTypes() {
		if mt.RequiresAuthority() && domain.SupportingAlternative(mt) == "" {
			t.Errorf("%s is authority-only but a supporting agent is told nothing about what to send instead", mt)
		}
	}
}

func TestAllowedTypesMatchWhatTheOrchestratorEnforces(t *testing.T) {
	for _, mt := range domain.AllowedTypes(false) {
		if mt.RequiresAuthority() {
			t.Errorf("supporting agents are offered %s, which the orchestrator refuses from them", mt)
		}
	}
	// The authority is offered every type but ERROR, which is the
	// orchestrator's own way of reporting a refused or unreadable reply.
	for _, mt := range domain.AllowedTypes(true) {
		if mt == domain.MsgError {
			t.Error("no agent should be offered ERROR")
		}
	}
	if len(domain.AllowedTypes(true)) != len(domain.AllMessageTypes())-1 {
		t.Error("the authority is offered every type but ERROR")
	}
}

// TestASupportingAgentsTaskAssignmentIsRefused: from a supporting agent, a
// TASK_ASSIGNMENT handed another agent work the authority never asked for,
// a way round DELEGATE.
func TestASupportingAgentsTaskAssignmentIsRefused(t *testing.T) {
	auth := orchestrator.NewAuthorityManager()
	err := auth.Authorize(verdictTask(), &domain.Message{FromAgent: "scout", ToAgent: "codex", Type: domain.MsgTaskAssignment})
	var v *orchestrator.Violation
	if !errors.As(err, &v) || v.Kind != orchestrator.ViolationAuthority {
		t.Fatalf("want an authority violation, got %v", err)
	}
	if !strings.Contains(v.Reason, "send a QUESTION") {
		t.Errorf("the refusal must say what to send instead, got %q", v.Reason)
	}
	if err := auth.Authorize(verdictTask(), &domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgTaskAssignment}); err != nil {
		t.Errorf("the authority may still assign: %v", err)
	}
}

func TestParseVerdictAcceptsCommonSpellings(t *testing.T) {
	cases := map[string]domain.Verdict{
		"approve": domain.VerdictApprove, "LGTM": domain.VerdictApprove,
		"request_changes": domain.VerdictChanges, "changes": domain.VerdictChanges,
		"Rejected": domain.VerdictReject,
	}
	for in, want := range cases {
		if got, ok := domain.ParseVerdict(in); !ok || got != want {
			t.Errorf("ParseVerdict(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if _, ok := domain.ParseVerdict("perhaps"); ok {
		t.Error("an unrecognised verdict must not parse")
	}
}
