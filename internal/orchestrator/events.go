// Package orchestrator is the coordination core. It owns task state,
// authority, routing, decisions and permissions. Agents supply information and
// actions; they never own state and never talk to each other directly.
package orchestrator

import (
	"context"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
)

// EventBus fans domain events out to subscribers and appends every event to
// the durable event log. It is the single place where "something happened"
// becomes both a log line and a UI update.
type EventBus struct {
	mu     sync.RWMutex
	nextID int
	subs   map[int]domain.EventHandler

	repo ports.EventRepository
	log  *logging.Logger
}

// NewEventBus builds an EventBus writing to repo.
func NewEventBus(repo ports.EventRepository, log *logging.Logger) *EventBus {
	if log == nil {
		log = logging.NewDiscard()
	}
	return &EventBus{subs: map[int]domain.EventHandler{}, repo: repo, log: log}
}

// Subscribe registers a handler and returns an unsubscribe function. Handlers
// are invoked synchronously and must not block: the TUI handler, for example,
// only forwards the event into the Bubble Tea program.
func (b *EventBus) Subscribe(h domain.EventHandler) func() {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = h
	b.mu.Unlock()

	return func() {
		b.mu.Lock()
		delete(b.subs, id)
		b.mu.Unlock()
	}
}

// PublishEphemeral distributes an event to subscribers without recording it or
// logging it.
//
// Live agent activity goes through here. It arrives many times a second while
// an agent is generating, and it is presentation only — appending it to the
// durable event log would bury the record of what actually happened under
// token deltas, and writing it to the log file would do the same there.
func (b *EventBus) PublishEphemeral(e domain.Event) {
	if e.ID == "" {
		e.ID = domain.NewEventID()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	b.mu.RLock()
	handlers := make([]domain.EventHandler, 0, len(b.subs))
	for _, h := range b.subs {
		handlers = append(handlers, h)
	}
	b.mu.RUnlock()

	for _, h := range handlers {
		h(e)
	}
}

// Publish stamps, records and distributes an event. Publishing never fails the
// caller: a log-write error is reported but does not abort orchestration.
func (b *EventBus) Publish(ctx context.Context, e domain.Event) domain.Event {
	if e.ID == "" {
		e.ID = domain.NewEventID()
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now()
	}

	if b.repo != nil {
		if err := b.repo.Append(ctx, e); err != nil {
			b.log.Error("append event", "error", err, "eventType", string(e.Type))
		}
	}
	b.log.Info("event",
		"eventType", string(e.Type),
		"taskId", string(e.TaskID),
		"agentId", string(e.AgentID),
		"messageId", string(e.MessageID),
		"summary", logging.Redact(e.Summary),
	)

	b.mu.RLock()
	handlers := make([]domain.EventHandler, 0, len(b.subs))
	for _, h := range b.subs {
		handlers = append(handlers, h)
	}
	b.mu.RUnlock()

	for _, h := range handlers {
		h(e)
	}
	return e
}

// emit is the internal shorthand used throughout the orchestrator.
func (b *EventBus) emit(ctx context.Context, t domain.EventType, taskID domain.TaskID, agentID domain.AgentID, summary string, details map[string]string) domain.Event {
	return b.Publish(ctx, domain.Event{
		Type:    t,
		TaskID:  taskID,
		AgentID: agentID,
		Summary: summary,
		Details: details,
	})
}
