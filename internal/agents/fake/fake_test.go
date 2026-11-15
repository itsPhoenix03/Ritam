package fake

import (
	"context"
	"testing"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

func started(t *testing.T, a *Agent) {
	t.Helper()
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Stop(context.Background()) })
}

func next(t *testing.T, a *Agent) ports.Outbound {
	t.Helper()
	select {
	case o := <-a.Outbound():
		return o
	case <-time.After(5 * time.Second):
		t.Fatal("no outbound")
	}
	return ports.Outbound{}
}

var task = &domain.Task{ID: "TASK-001", AuthorityAgent: "opus", Participants: []domain.AgentID{"opus", "codex"}}

// TestATurnIsWorkingUntilItEndsAndReportsUsageAfterItsMessages: like a CLI
// adapter, the fake is WORKING while its turn runs, sends its messages, and
// only then the turn's usage.
func TestATurnIsWorkingUntilItEndsAndReportsUsageAfterItsMessages(t *testing.T) {
	reply := Header(domain.MsgAnswer, "opus", "one") + Header(domain.MsgStatusUpdate, "human", "two")
	a := New("codex", Steps(Turn{Latency: 150 * time.Millisecond, Reply: reply}))
	started(t, a)
	if err := a.SendMessage(context.Background(), task, domain.Message{Type: domain.MsgDelegate, FromAgent: "opus", ToAgent: "codex"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if a.Status() != domain.AgentStateWorking {
		t.Fatalf("a running turn is WORKING, got %s", a.Status())
	}
	if o := next(t, a); o.Message.Type != domain.MsgAnswer || o.Message.Content != "one" {
		t.Fatalf("first the messages, parsed from the raw reply: %+v", o)
	}
	if o := next(t, a); o.Message.Type != domain.MsgStatusUpdate {
		t.Fatalf("second message: %+v", o)
	}
	if o := next(t, a); !o.IsUsageOnly() {
		t.Fatalf("then the usage: %+v", o)
	}
	if _, ok := any(a).(ports.BatchCapable); !ok || !a.AcceptsBatches() {
		t.Error("the fake takes combined deliveries, as the CLI adapters do")
	}
}

// TestScriptedFailuresArriveAsTypedFailures: each scripted failure reaches
// the orchestrator as the kind a real adapter would report; a malformed
// reply goes through the shared parser and carries the agent's text.
func TestScriptedFailuresArriveAsTypedFailures(t *testing.T) {
	a := New("codex", Steps(
		Failing(domain.FailureCrash),
		RateLimited(2*time.Second),
		Failing(domain.FailureTimeout),
		Reply(Header(domain.MsgAnswer, "", "no recipient")),
		Reply("prose, no header"),
		Reply(""),
	))
	a.TurnLimit = 50 * time.Millisecond
	started(t, a)
	want := []domain.FailureKind{domain.FailureCrash, domain.FailureRateLimit, domain.FailureTimeout,
		domain.FailureMalformed, domain.FailureMalformed}
	for i, kind := range want {
		_ = a.SendMessage(context.Background(), task, domain.Message{Type: domain.MsgDelegate, ToAgent: "codex"})
		f := domain.FailureOf(next(t, a).Err)
		if f == nil || f.Kind != kind {
			t.Fatalf("turn %d: want %s, got %+v", i+1, kind, f)
		}
		if kind == domain.FailureRateLimit && f.RetryAfter != 2*time.Second {
			t.Errorf("the reset time must come through: %v", f.RetryAfter)
		}
		if kind == domain.FailureMalformed && f.Raw == "" {
			t.Error("a malformed reply carries the agent's text")
		}
		if o := next(t, a); !o.IsUsageOnly() {
			t.Fatalf("turn %d: a failed turn still reports its usage: %+v", i+1, o)
		}
	}
	_ = a.SendMessage(context.Background(), task, domain.Message{Type: domain.MsgDelegate, ToAgent: "codex"})
	if o := next(t, a); !o.IsUsageOnly() {
		t.Fatalf("an empty reply is the agent saying nothing: %+v", o)
	}
}
