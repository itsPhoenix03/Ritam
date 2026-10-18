package storage

import (
	"context"
	"fmt"
	"sync"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// NewInMemory builds a fully populated ports.Storage backed by the
// implementations in this package. This is the scaffold default.
func NewInMemory() *ports.Storage {
	return &ports.Storage{
		Tasks:         NewMemTaskRepository(),
		Messages:      NewMemMessageRepository(),
		Events:        NewMemEventRepository(),
		Decisions:     NewMemDecisionRepository(),
		Disagreements: NewMemDisagreementRepository(),
		Knowledge:     NewMemKnowledgeRepository(),
		Artifacts:     NewMemArtifactRepository(),
	}
}

// MemTaskRepository is an in-memory, goroutine-safe ports.TaskRepository.
type MemTaskRepository struct {
	mu    sync.RWMutex
	tasks map[domain.TaskID]*domain.Task
	// order records creation order so List is stable and chronological.
	order []domain.TaskID
	seq   int
}

// NewMemTaskRepository constructs an empty task repository.
func NewMemTaskRepository() *MemTaskRepository {
	return &MemTaskRepository{tasks: make(map[domain.TaskID]*domain.Task)}
}

// Create stores a new task. It fails if the id is already taken.
func (r *MemTaskRepository) Create(_ context.Context, t *domain.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[t.ID]; ok {
		return fmt.Errorf("%w: task %s", ports.ErrAlreadyExists, t.ID)
	}
	r.tasks[t.ID] = t.Clone()
	r.order = append(r.order, t.ID)
	return nil
}

// Update overwrites an existing task. It fails if the task does not exist.
func (r *MemTaskRepository) Update(_ context.Context, t *domain.Task) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tasks[t.ID]; !ok {
		return fmt.Errorf("%w: task %s", ports.ErrNotFound, t.ID)
	}
	r.tasks[t.ID] = t.Clone()
	return nil
}

// Get returns a deep copy of the task with the given id.
func (r *MemTaskRepository) Get(_ context.Context, id domain.TaskID) (*domain.Task, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: task %s", ports.ErrNotFound, id)
	}
	return t.Clone(), nil
}

// List returns every task in creation order, as deep copies.
func (r *MemTaskRepository) List(_ context.Context) ([]*domain.Task, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*domain.Task, 0, len(r.order))
	for _, id := range r.order {
		out = append(out, r.tasks[id].Clone())
	}
	return out, nil
}

// NextSequence returns 1, 2, 3, ... on successive calls.
func (r *MemTaskRepository) NextSequence(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq, nil
}

// MemMessageRepository is an in-memory, goroutine-safe ports.MessageRepository.
type MemMessageRepository struct {
	mu   sync.RWMutex
	byID map[domain.MessageID]*domain.Message
	// byTask preserves insertion (chronological) order per task.
	byTask map[domain.TaskID][]*domain.Message
}

// NewMemMessageRepository constructs an empty message repository.
func NewMemMessageRepository() *MemMessageRepository {
	return &MemMessageRepository{
		byID:   make(map[domain.MessageID]*domain.Message),
		byTask: make(map[domain.TaskID][]*domain.Message),
	}
}

// Append stores a message. It fails if the id is already taken, since the
// message log is append-only and ids must stay unique.
func (r *MemMessageRepository) Append(_ context.Context, m *domain.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[m.ID]; ok {
		return fmt.Errorf("%w: message %s", ports.ErrAlreadyExists, m.ID)
	}
	cp := m.Clone()
	r.byID[m.ID] = cp
	r.byTask[m.TaskID] = append(r.byTask[m.TaskID], cp)
	return nil
}

// Get returns a deep copy of the message with the given id.
func (r *MemMessageRepository) Get(_ context.Context, id domain.MessageID) (*domain.Message, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: message %s", ports.ErrNotFound, id)
	}
	return m.Clone(), nil
}

// ListByTask returns a task's messages in the order they were appended.
func (r *MemMessageRepository) ListByTask(_ context.Context, id domain.TaskID) ([]*domain.Message, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	msgs := r.byTask[id]
	out := make([]*domain.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Clone())
	}
	return out, nil
}

// CountByTask returns the number of messages recorded for a task.
func (r *MemMessageRepository) CountByTask(_ context.Context, id domain.TaskID) (int, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.byTask[id]), nil
}

// MemEventRepository is an in-memory, goroutine-safe ports.EventRepository.
type MemEventRepository struct {
	mu   sync.RWMutex
	all  []domain.Event
	byID map[domain.TaskID][]domain.Event
}

// NewMemEventRepository constructs an empty event repository.
func NewMemEventRepository() *MemEventRepository {
	return &MemEventRepository{byID: make(map[domain.TaskID][]domain.Event)}
}

// cloneEvent deep-copies an event's map field so stored state cannot be
// mutated through a returned value.
func cloneEvent(e domain.Event) domain.Event {
	if e.Details != nil {
		cp := make(map[string]string, len(e.Details))
		for k, v := range e.Details {
			cp[k] = v
		}
		e.Details = cp
	}
	return e
}

// Append records an event. The event log is append-only.
func (r *MemEventRepository) Append(_ context.Context, e domain.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := cloneEvent(e)
	r.all = append(r.all, cp)
	r.byID[e.TaskID] = append(r.byID[e.TaskID], cp)
	return nil
}

// ListByTask returns a task's events in chronological (insertion) order.
func (r *MemEventRepository) ListByTask(_ context.Context, id domain.TaskID) ([]domain.Event, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	evts := r.byID[id]
	out := make([]domain.Event, 0, len(evts))
	for _, e := range evts {
		out = append(out, cloneEvent(e))
	}
	return out, nil
}

// ListAll returns every event across every task, in chronological order.
func (r *MemEventRepository) ListAll(_ context.Context) ([]domain.Event, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]domain.Event, 0, len(r.all))
	for _, e := range r.all {
		out = append(out, cloneEvent(e))
	}
	return out, nil
}
