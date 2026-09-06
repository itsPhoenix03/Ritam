package mockagent_test

import (
	"context"
	"testing"

	mockagent "projectorch/internal/agents/mock"
	"projectorch/internal/domain"
)

func drain(t *testing.T, a *mockagent.Adapter) []domain.Message {
	t.Helper()
	var out []domain.Message
	for {
		select {
		case o, ok := <-a.Outbound():
			if !ok {
				return out
			}
			out = append(out, o.Message)
		default:
			return out
		}
	}
}

func task(authority domain.AgentID, participants ...domain.AgentID) *domain.Task {
	return &domain.Task{ID: "TASK-001", AuthorityAgent: authority, Participants: participants}
}

func TestAuthorityBehaviorSequence(t *testing.T) {
	ctx := context.Background()
	opus := mockagent.New("opus", domain.AgentKindMock, mockagent.AuthorityBehavior())
	if err := opus.Start(ctx); err != nil {
		t.Fatal(err)
	}
	tk := task("opus", "opus", "codex")

	// TASK_ASSIGNMENT -> DELEGATE
	assign := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, ToAgent: "opus", Type: domain.MsgTaskAssignment}
	if err := opus.SendTask(ctx, tk, assign); err != nil {
		t.Fatal(err)
	}
	out := drain(t, opus)
	if len(out) != 1 || out[0].Type != domain.MsgDelegate || out[0].ToAgent != "codex" {
		t.Fatalf("expected one DELEGATE to codex, got %+v", out)
	}

	// ANSWER -> IMPLEMENT
	answer := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "codex", ToAgent: "opus", Type: domain.MsgAnswer}
	if err := opus.SendMessage(ctx, tk, answer); err != nil {
		t.Fatal(err)
	}
	out = drain(t, opus)
	if len(out) != 1 || out[0].Type != domain.MsgImplement {
		t.Fatalf("expected IMPLEMENT, got %+v", out)
	}

	// IMPLEMENTATION_RESULT (1st) -> REQUEST_CHANGE
	res1 := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "codex", ToAgent: "opus", Type: domain.MsgImplementationResult}
	if err := opus.SendMessage(ctx, tk, res1); err != nil {
		t.Fatal(err)
	}
	out = drain(t, opus)
	if len(out) != 1 || out[0].Type != domain.MsgRequestChange {
		t.Fatalf("expected REQUEST_CHANGE, got %+v", out)
	}

	// IMPLEMENTATION_RESULT (2nd) -> FINAL_DECISION
	res2 := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "codex", ToAgent: "opus", Type: domain.MsgImplementationResult}
	if err := opus.SendMessage(ctx, tk, res2); err != nil {
		t.Fatal(err)
	}
	out = drain(t, opus)
	if len(out) != 1 || out[0].Type != domain.MsgFinalDecision || out[0].ToAgent != domain.BroadcastAgentID {
		t.Fatalf("expected broadcast FINAL_DECISION, got %+v", out)
	}
	if out[0].Metadata["decision"] == "" {
		t.Fatalf("expected decision metadata, got %+v", out[0].Metadata)
	}

	// CHALLENGE -> DELEGATE back
	chal := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "codex", ToAgent: "opus", Type: domain.MsgChallenge}
	if err := opus.SendMessage(ctx, tk, chal); err != nil {
		t.Fatal(err)
	}
	out = drain(t, opus)
	if len(out) != 1 || out[0].Type != domain.MsgDelegate || out[0].ToAgent != "codex" {
		t.Fatalf("expected DELEGATE back to codex, got %+v", out)
	}
}

func TestImplementerBehaviorSequence(t *testing.T) {
	ctx := context.Background()
	codex := mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior())
	_ = codex.Start(ctx)
	tk := task("opus", "opus", "codex")

	assign := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, ToAgent: "codex", Type: domain.MsgTaskAssignment}
	_ = codex.SendTask(ctx, tk, assign)
	if out := drain(t, codex); len(out) != 0 {
		t.Fatalf("expected silence on TASK_ASSIGNMENT, got %+v", out)
	}

	del := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate}
	_ = codex.SendMessage(ctx, tk, del)
	out := drain(t, codex)
	if len(out) != 1 || out[0].Type != domain.MsgAnswer {
		t.Fatalf("expected ANSWER, got %+v", out)
	}

	impl := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgImplement}
	_ = codex.SendMessage(ctx, tk, impl)
	out = drain(t, codex)
	if len(out) != 1 || out[0].Type != domain.MsgImplementationResult {
		t.Fatalf("expected IMPLEMENTATION_RESULT, got %+v", out)
	}

	rc := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgRequestChange}
	_ = codex.SendMessage(ctx, tk, rc)
	out = drain(t, codex)
	if len(out) != 1 || out[0].Type != domain.MsgImplementationResult {
		t.Fatalf("expected IMPLEMENTATION_RESULT after change, got %+v", out)
	}
}

func TestChallengingImplementerBehaviorChallengesOnDelegate(t *testing.T) {
	ctx := context.Background()
	codex := mockagent.New("codex", domain.AgentKindMock, mockagent.ChallengingImplementerBehavior())
	_ = codex.Start(ctx)
	tk := task("opus", "opus", "codex")

	del := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate}
	_ = codex.SendMessage(ctx, tk, del)
	out := drain(t, codex)
	if len(out) != 1 || out[0].Type != domain.MsgChallenge {
		t.Fatalf("expected CHALLENGE, got %+v", out)
	}

	// Still behaves like ImplementerBehavior for IMPLEMENT.
	impl := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgImplement}
	_ = codex.SendMessage(ctx, tk, impl)
	out = drain(t, codex)
	if len(out) != 1 || out[0].Type != domain.MsgImplementationResult {
		t.Fatalf("expected IMPLEMENTATION_RESULT, got %+v", out)
	}
}

func TestRogueBehaviorViolatesAuthority(t *testing.T) {
	ctx := context.Background()
	rogue := mockagent.New("rogue", domain.AgentKindMock, mockagent.RogueBehavior())
	_ = rogue.Start(ctx)
	tk := task("opus", "opus", "codex", "rogue")

	assign := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, ToAgent: "rogue", Type: domain.MsgTaskAssignment}
	_ = rogue.SendTask(ctx, tk, assign)
	out := drain(t, rogue)
	if len(out) != 2 {
		t.Fatalf("expected 2 messages, got %+v", out)
	}
	var sawFinal, sawStranger bool
	for _, m := range out {
		if m.Type == domain.MsgFinalDecision {
			sawFinal = true
		}
		if m.Type == domain.MsgDelegate && m.ToAgent == domain.AgentID("stranger") {
			sawStranger = true
		}
	}
	if !sawFinal || !sawStranger {
		t.Fatalf("expected FINAL_DECISION and DELEGATE-to-stranger, got %+v", out)
	}
}

func TestCrashingAdapterReportsErrorAndErrorMessage(t *testing.T) {
	ctx := context.Background()
	crashing := mockagent.NewCrashing("codex")
	_ = crashing.Start(ctx)
	tk := task("opus", "opus", "codex")

	m := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate}
	if err := crashing.SendMessage(ctx, tk, m); err != nil {
		t.Fatal(err)
	}

	var sawProtocolError, sawRuntimeErr bool
	for i := 0; i < 2; i++ {
		select {
		case o := <-crashing.Outbound():
			if o.Err != nil {
				sawRuntimeErr = true
			} else if o.Message.Type == domain.MsgError {
				sawProtocolError = true
			}
		}
	}
	if !sawProtocolError || !sawRuntimeErr {
		t.Fatalf("expected both a protocol ERROR message and a runtime Err, got protoErr=%v runtimeErr=%v", sawProtocolError, sawRuntimeErr)
	}
}

func TestSilentBehaviorNeverResponds(t *testing.T) {
	ctx := context.Background()
	silent := mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior())
	_ = silent.Start(ctx)
	tk := task("opus", "opus", "codex")

	for _, typ := range domain.AllMessageTypes() {
		m := domain.Message{ID: domain.NewMessageID(), TaskID: tk.ID, FromAgent: "opus", ToAgent: "codex", Type: typ}
		_ = silent.SendMessage(ctx, tk, m)
	}
	if out := drain(t, silent); len(out) != 0 {
		t.Fatalf("expected silence, got %+v", out)
	}
}

func TestStoppedAdapterRejectsSends(t *testing.T) {
	ctx := context.Background()
	a := mockagent.New("codex", domain.AgentKindMock, mockagent.ImplementerBehavior())
	// Never started.
	m := domain.Message{ID: domain.NewMessageID(), Type: domain.MsgDelegate, ToAgent: "codex"}
	if err := a.SendMessage(ctx, nil, m); err == nil {
		t.Fatal("expected error sending to a never-started adapter")
	}

	_ = a.Start(ctx)
	if err := a.SendMessage(ctx, nil, m); err != nil {
		t.Fatalf("expected send to succeed once started: %v", err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := a.SendMessage(ctx, nil, m); err == nil {
		t.Fatal("expected error sending to a stopped adapter")
	}
}

func TestDoubleStopIsSafe(t *testing.T) {
	ctx := context.Background()
	a := mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior())
	_ = a.Start(ctx)
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("first stop: %v", err)
	}
	if err := a.Stop(ctx); err != nil {
		t.Fatalf("second stop should be safe, got: %v", err)
	}
}

func TestSentRecordsInboundMessages(t *testing.T) {
	ctx := context.Background()
	a := mockagent.New("codex", domain.AgentKindMock, mockagent.SilentBehavior())
	_ = a.Start(ctx)
	m1 := domain.Message{ID: domain.NewMessageID(), Type: domain.MsgDelegate, ToAgent: "codex", Content: "one"}
	m2 := domain.Message{ID: domain.NewMessageID(), Type: domain.MsgImplement, ToAgent: "codex", Content: "two"}
	_ = a.SendMessage(ctx, nil, m1)
	_ = a.SendMessage(ctx, nil, m2)
	sent := a.Sent()
	if len(sent) != 2 || sent[0].Content != "one" || sent[1].Content != "two" {
		t.Fatalf("unexpected sent history: %+v", sent)
	}
}
