// Package fake provides a scripted agent that behaves like a CLI adapter, for
// testing what the orchestrator does when agents are slow, fail or answer
// badly.
//
// The mock agent (internal/agents/mock) answers synchronously, skips the
// reply parser, reports no usage and takes no batches, so none of the real
// failure paths could be tested end to end with it. This one runs each turn
// on the same invocation queue the real adapters use (it is seen WORKING
// while a turn runs), turns raw reply text into messages with the shared
// header parser, reports usage after its messages as the real adapters do,
// accepts batches, and is scripted turn by turn: latency, a timeout, a crash,
// a rate limit with or without a reset time, an overloaded server, a
// malformed or empty reply, a budget stop.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Marker is the reply header the fake's raw replies use, as "@@ORCH" is
// Claude Code's and "##CODEX" Codex's.
const Marker = "@@ORCH"

// Turn is what the fake does with one delivery.
type Turn struct {
	// Latency is how long the turn works before it ends.
	Latency time.Duration
	// Reply is the agent's raw text, parsed exactly as a CLI reply is. Empty
	// is an agent saying nothing.
	Reply string
	// Fail, when set, ends the turn with a failure of this kind instead of a
	// reply. A Timeout turn first works until the adapter's TurnLimit, as a
	// CLI does until the runner kills it.
	Fail domain.FailureKind
	// RetryAfter is a rate limit's reset time (0: the CLI did not say).
	RetryAfter time.Duration
	// Usage is what the turn reports it cost; nil reports a small default.
	Usage *ports.Usage
}

// Script decides each turn: n counts the adapter's turns from 1, in is what
// it was handed (a combined delivery when several messages waited).
type Script func(n int, t *domain.Task, in domain.Message) Turn

// Steps scripts turns in order. Once they run out the agent says nothing.
func Steps(steps ...Turn) Script {
	return func(n int, _ *domain.Task, _ domain.Message) Turn {
		if n <= len(steps) {
			return steps[n-1]
		}
		return Turn{}
	}
}

// Then plays first's turns, then hands every later turn to rest.
func Then(rest Script, first ...Turn) Script {
	return func(n int, t *domain.Task, in domain.Message) Turn {
		if n <= len(first) {
			return first[n-1]
		}
		return rest(n-len(first), t, in)
	}
}

// Reply is a turn answering with raw text.
func Reply(text string) Turn { return Turn{Reply: text} }

// Header writes a reply block: the header line, then the body.
func Header(typ domain.MessageType, to domain.AgentID, body string) string {
	return fmt.Sprintf("%s type=%s to=%s\n%s\n", Marker, typ, to, body)
}

// Failing is a turn that fails with kind.
func Failing(kind domain.FailureKind) Turn { return Turn{Fail: kind} }

// RateLimited is a turn stopped by a rate limit resetting after reset (0:
// unknown).
func RateLimited(reset time.Duration) Turn {
	return Turn{Fail: domain.FailureRateLimit, RetryAfter: reset}
}

// Agent is the scripted adapter.
type Agent struct {
	id     domain.AgentID
	kind   domain.AgentKind
	script Script
	// TurnLimit is how long a Timeout turn works before it is ended, the
	// fake's responseTimeout.
	TurnLimit time.Duration

	out      chan ports.Outbound
	activity *core.ActivityEmitter
	queue    *core.InvocationQueue

	mu        sync.Mutex
	state     domain.AgentState
	stopped   bool
	lifetime  context.Context
	cancelAll context.CancelFunc
	turns     int
	received  []domain.Message
}

// New builds a fake agent of kind mock.
func New(id domain.AgentID, script Script) *Agent {
	a := &Agent{
		id:        id,
		kind:      domain.AgentKindMock,
		script:    script,
		TurnLimit: 200 * time.Millisecond,
		out:       make(chan ports.Outbound, 64),
		activity:  core.NewActivityEmitter(id, 256),
		state:     domain.AgentStateStopped,
		stopped:   true,
	}
	a.queue = core.NewInvocationQueue(func(busy bool) {
		if busy {
			a.setState(domain.AgentStateWorking)
		} else {
			a.setState(domain.AgentStateIdle)
		}
	})
	return a
}

// ID returns the agent's identity.
func (a *Agent) ID() domain.AgentID { return a.id }

// Kind returns the agent's kind.
func (a *Agent) Kind() domain.AgentKind { return a.kind }

// Start opens the agent for deliveries. It may be called again after Stop.
func (a *Agent) Start(context.Context) error {
	a.mu.Lock()
	a.stopped = false
	a.lifetime, a.cancelAll = context.WithCancel(context.Background())
	a.mu.Unlock()
	a.queue.Reopen()
	a.activity.Reopen()
	a.setState(domain.AgentStateIdle)
	return nil
}

// Stop ends the turn in progress, as killing a CLI process does, and drops
// the queued ones.
func (a *Agent) Stop(context.Context) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return nil
	}
	a.stopped = true
	cancel := a.cancelAll
	a.mu.Unlock()
	a.queue.Close()
	if cancel != nil {
		cancel()
	}
	a.queue.Wait()
	a.activity.Close()
	a.setState(domain.AgentStateStopped)
	return nil
}

// SendTask delivers the opening assignment.
func (a *Agent) SendTask(ctx context.Context, t *domain.Task, m domain.Message) error {
	return a.SendMessage(ctx, t, m)
}

// SendMessage queues a turn and returns at once, as the real adapters do.
func (a *Agent) SendMessage(ctx context.Context, t *domain.Task, m domain.Message) error {
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		return fmt.Errorf("fake adapter: agent %s is not running", a.id)
	}
	a.received = append(a.received, *m.Clone())
	lifetime := a.lifetime
	task := t.Clone()
	a.mu.Unlock()
	if !a.queue.Enqueue(func() {
		turnCtx, cancel := context.WithCancel(lifetime)
		defer cancel()
		defer context.AfterFunc(ctx, cancel)()
		if turnCtx.Err() != nil {
			return
		}
		a.run(turnCtx, task, m)
	}) {
		return fmt.Errorf("fake adapter: agent %s is not running", a.id)
	}
	return nil
}

// run plays one turn.
func (a *Agent) run(ctx context.Context, t *domain.Task, m domain.Message) {
	a.mu.Lock()
	a.turns++
	n := a.turns
	a.mu.Unlock()
	if t != nil {
		a.activity.SetTask(t.ID)
	}
	a.activity.Emit(ports.ActivityStatus, core.WorkingOn(m))

	turn := a.script(n, t, m)
	wait := turn.Latency
	if turn.Fail == domain.FailureTimeout {
		wait = a.TurnLimit
	}
	select {
	case <-ctx.Done():
		// Cancelled: nobody is waiting for the answer, so nothing is sent.
		a.activity.Emit(ports.ActivityStatus, "cancelled")
		return
	case <-time.After(wait):
	}

	usage := turn.Usage
	if usage == nil {
		usage = &ports.Usage{InputTokens: 100, OutputTokens: 20}
	}
	// Usage last, after the turn's messages, as the real adapters send it.
	defer a.emit(ports.Outbound{AgentID: a.id, Usage: usage})

	if turn.Fail != "" {
		f := domain.NewFailure(turn.Fail, a.detail(turn), nil)
		f.RetryAfter = turn.RetryAfter
		a.activity.Emit(ports.ActivityStatus, "failed: "+f.Detail)
		a.emit(ports.Outbound{AgentID: a.id, Err: f})
		return
	}
	if turn.Reply == "" {
		a.activity.Emit(ports.ActivityStatus, "nothing to reply")
		return
	}
	a.activity.Append(ports.ActivityText, turn.Reply)
	a.activity.Flush()
	msgs, err := core.ParseReply(turn.Reply, Marker, "fake adapter")
	if err != nil {
		a.emit(ports.Outbound{AgentID: a.id, Err: err})
		return
	}
	if len(msgs) == 0 {
		a.emit(ports.Outbound{AgentID: a.id, Err: core.NoHeaderFailure("fake adapter", a.id, Marker, turn.Reply)})
		return
	}
	for _, msg := range msgs {
		a.emit(ports.Outbound{AgentID: a.id, Message: msg})
	}
}

// detail words a scripted failure the way an adapter would.
func (a *Agent) detail(turn Turn) string {
	switch turn.Fail {
	case domain.FailureTimeout:
		return fmt.Sprintf("fake adapter: agent %s: %v after %s", a.id, core.ErrTimeout, a.TurnLimit)
	case domain.FailureRateLimit:
		if turn.RetryAfter > 0 {
			return fmt.Sprintf("fake adapter: usage limit reached; resets in %s", turn.RetryAfter)
		}
		return "fake adapter: usage limit reached"
	case domain.FailureOverloaded:
		return "fake adapter: API Error: 529 overloaded"
	case domain.FailureCrash:
		return fmt.Sprintf("fake adapter: agent %s: the CLI exited with code 1", a.id)
	}
	return "fake adapter: " + turn.Fail.Advice()
}

// AcceptsBatches reports that waiting messages may be combined into one turn.
func (a *Agent) AcceptsBatches() bool { return true }

// Interrupt does nothing: a turn is ended by cancelling its context.
func (a *Agent) Interrupt(context.Context) error { return nil }

// Status reports WORKING while a turn runs or waits.
func (a *Agent) Status() domain.AgentState {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state
}

// Outbound yields what the turns produced.
func (a *Agent) Outbound() <-chan ports.Outbound { return a.out }

// Activity yields the turns' live output.
func (a *Agent) Activity() <-chan ports.AgentActivity { return a.activity.Channel() }

// Received is every delivery, in order.
func (a *Agent) Received() []domain.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]domain.Message(nil), a.received...)
}

// Turns is how many turns have started.
func (a *Agent) Turns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.turns
}

func (a *Agent) setState(s domain.AgentState) {
	a.mu.Lock()
	a.state = s
	a.mu.Unlock()
}

func (a *Agent) emit(o ports.Outbound) {
	a.mu.Lock()
	stopped := a.stopped
	a.mu.Unlock()
	if stopped {
		return
	}
	select {
	case a.out <- o:
	default:
	}
}

var (
	_ ports.AgentAdapter = (*Agent)(nil)
	_ ports.BatchCapable = (*Agent)(nil)
)
