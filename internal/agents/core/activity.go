package core

import (
	"sync"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// activityFlushInterval bounds how often coalesced text is published. Token
// deltas arrive far faster than any terminal should redraw; batching them
// keeps the UI readable and stops a fast agent from flooding the event path.
const activityFlushInterval = 80 * time.Millisecond

// ActivityEmitter publishes live agent output on a channel, coalescing rapid
// same-kind text into periodic flushes.
//
// It is shared by the real adapters because both face the same problem from
// opposite directions: Claude emits many small token deltas, Codex emits
// occasional large items. Both want the same contract at the far end — a
// channel that never blocks the agent and never floods the reader.
type ActivityEmitter struct {
	agentID domain.AgentID
	out     chan ports.AgentActivity

	mu      sync.Mutex
	pending map[ports.ActivityKind]*pendingText
	taskID  domain.TaskID
	closed  bool
	timer   *time.Timer
}

type pendingText struct {
	text string
}

// NewActivityEmitter builds an emitter with a buffered output channel.
func NewActivityEmitter(agentID domain.AgentID, buffer int) *ActivityEmitter {
	if buffer <= 0 {
		buffer = 256
	}
	return &ActivityEmitter{
		agentID: agentID,
		out:     make(chan ports.AgentActivity, buffer),
		pending: map[ports.ActivityKind]*pendingText{},
	}
}

// Channel returns the activity stream.
func (e *ActivityEmitter) Channel() <-chan ports.AgentActivity { return e.out }

// SetTask tags subsequent activity with the task being worked on.
func (e *ActivityEmitter) SetTask(id domain.TaskID) {
	e.mu.Lock()
	e.taskID = id
	e.mu.Unlock()
}

// Append adds text to the pending buffer for a kind, scheduling a flush.
// Streaming deltas go here; whole events should use Emit.
func (e *ActivityEmitter) Append(kind ports.ActivityKind, text string) {
	if text == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}

	p, ok := e.pending[kind]
	if !ok {
		p = &pendingText{}
		e.pending[kind] = p
	}
	p.text += text

	if e.timer == nil {
		e.timer = time.AfterFunc(activityFlushInterval, e.Flush)
	}
}

// Emit publishes a discrete activity immediately, after flushing anything
// pending so ordering is preserved.
func (e *ActivityEmitter) Emit(kind ports.ActivityKind, text string) {
	if text == "" {
		return
	}
	e.Flush()

	e.mu.Lock()
	closed, taskID := e.closed, e.taskID
	e.mu.Unlock()
	if closed {
		return
	}
	e.send(ports.AgentActivity{
		AgentID: e.agentID, TaskID: taskID, Kind: kind, Text: text, Time: time.Now(),
	})
}

// Flush publishes any buffered text at once.
func (e *ActivityEmitter) Flush() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	taskID := e.taskID
	batch := make([]ports.AgentActivity, 0, len(e.pending))
	for kind, p := range e.pending {
		if p.text == "" {
			continue
		}
		batch = append(batch, ports.AgentActivity{
			AgentID: e.agentID, TaskID: taskID, Kind: kind, Text: p.text, Time: time.Now(),
		})
		p.text = ""
	}
	e.mu.Unlock()

	for _, a := range batch {
		e.send(a)
	}
}

// send drops rather than blocks. A stalled reader must never be able to wedge
// an agent mid-answer: losing presentation output is always preferable to
// losing the agent's actual work.
func (e *ActivityEmitter) send(a ports.AgentActivity) {
	select {
	case e.out <- a:
	default:
	}
}

// Close flushes and releases the emitter. It is safe to call more than once,
// and the channel is deliberately left open: a late send from an in-flight
// invocation must not panic on a closed channel.
func (e *ActivityEmitter) Close() {
	e.Flush()
	e.mu.Lock()
	e.closed = true
	if e.timer != nil {
		e.timer.Stop()
		e.timer = nil
	}
	e.mu.Unlock()
}

// Reopen allows the emitter to publish again after a Close, which happens when
// an adapter is stopped and later restarted.
func (e *ActivityEmitter) Reopen() {
	e.mu.Lock()
	e.closed = false
	e.mu.Unlock()
}
