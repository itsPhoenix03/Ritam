package domain

import "testing"

func TestCanTransition_HappyPath(t *testing.T) {
	path := []TaskState{
		TaskCreated, TaskPlanning, TaskInvestigating, TaskImplementing,
		TaskReviewing, TaskDeliberating, TaskAwaitingDecision, TaskCompleted,
	}
	for i := 0; i < len(path)-1; i++ {
		if !CanTransition(path[i], path[i+1]) {
			t.Errorf("expected %s -> %s to be legal", path[i], path[i+1])
		}
	}
}

func TestCanTransition_Loops(t *testing.T) {
	// The lifecycle is not a linear pipeline: review can send work back to
	// implementation and deliberation can send it back to investigation.
	loops := [][2]TaskState{
		{TaskReviewing, TaskImplementing},
		{TaskImplementing, TaskReviewing},
		{TaskDeliberating, TaskInvestigating},
		{TaskInvestigating, TaskDeliberating},
		{TaskAwaitingDecision, TaskDeliberating},
	}
	for _, l := range loops {
		if !CanTransition(l[0], l[1]) {
			t.Errorf("expected loop %s -> %s to be legal", l[0], l[1])
		}
	}
}

func TestCanTransition_Invalid(t *testing.T) {
	invalid := [][2]TaskState{
		{TaskCreated, TaskCompleted},
		{TaskCreated, TaskReviewing},
		{TaskCompleted, TaskPlanning},
		{TaskCancelled, TaskInvestigating},
		{TaskFailed, TaskCompleted},
		{TaskPlanning, TaskPlanning}, // self-transition is not a transition
	}
	for _, c := range invalid {
		if CanTransition(c[0], c[1]) {
			t.Errorf("expected %s -> %s to be rejected", c[0], c[1])
		}
	}
}

func TestTerminalStatesHaveNoSuccessors(t *testing.T) {
	for _, s := range []TaskState{TaskCompleted, TaskFailed, TaskCancelled} {
		if !s.IsTerminal() {
			t.Errorf("%s should be terminal", s)
		}
		if got := AllowedTransitions(s); len(got) != 0 {
			t.Errorf("%s should have no successors, got %v", s, got)
		}
	}
}

func TestAllowedTransitionsIsACopy(t *testing.T) {
	got := AllowedTransitions(TaskCreated)
	if len(got) == 0 {
		t.Fatal("expected successors for CREATED")
	}
	got[0] = TaskState("MUTATED")
	if AllowedTransitions(TaskCreated)[0] == TaskState("MUTATED") {
		t.Fatal("AllowedTransitions leaked the internal transition table")
	}
}

func TestMessageTypeAuthority(t *testing.T) {
	authorityOnly := []MessageType{
		MsgFinalDecision, MsgApprove, MsgReject, MsgRequestChange, MsgImplement, MsgDelegate,
	}
	for _, mt := range authorityOnly {
		if !mt.RequiresAuthority() {
			t.Errorf("%s should require authority", mt)
		}
	}
	// Supporting agents must always be able to disagree.
	open := []MessageType{MsgChallenge, MsgReview, MsgAnswer, MsgProposal, MsgTestResult}
	for _, mt := range open {
		if mt.RequiresAuthority() {
			t.Errorf("%s should not require authority: supporting agents must be able to send it", mt)
		}
	}
}

func TestIsKnownMessageType(t *testing.T) {
	if !IsKnownMessageType(MsgChallenge) {
		t.Error("CHALLENGE should be known")
	}
	// Agent output is untrusted; invented types must be rejected.
	if IsKnownMessageType(MessageType("SEIZE_CONTROL")) {
		t.Error("unknown message types must be rejected")
	}
}

func TestRoleCapabilities(t *testing.T) {
	if !RoleAuthority.CanIssueFinalDecision() {
		t.Error("AUTHORITY must be able to issue a final decision")
	}
	for _, r := range []AgentRole{RoleImplementer, RoleReviewer, RoleObserver} {
		if r.CanIssueFinalDecision() {
			t.Errorf("%s must not be able to issue a final decision", r)
		}
	}
	if !RoleImplementer.CanChallenge() || !RoleReviewer.CanChallenge() {
		t.Error("supporting roles must be able to challenge")
	}
	if RoleObserver.CanChallenge() {
		t.Error("observers must not challenge")
	}
}

func TestTaskCloneIsDeep(t *testing.T) {
	task := &Task{ID: "TASK-001", Participants: []AgentID{"opus", "codex"}}
	cp := task.Clone()
	cp.Participants[0] = "rogue"
	if task.Participants[0] != "opus" {
		t.Fatal("Clone must not share the participants slice")
	}
}

func TestFormatIDs(t *testing.T) {
	if got := FormatTaskID(1); got != "TASK-001" {
		t.Errorf("FormatTaskID(1) = %q", got)
	}
	if got := FormatDecisionID(12); got != "D-012" {
		t.Errorf("FormatDecisionID(12) = %q", got)
	}
	if NewMessageID() == NewMessageID() {
		t.Error("message ids must be unique")
	}
}
