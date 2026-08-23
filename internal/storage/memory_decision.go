package storage

import (
	"context"
	"fmt"
	"sync"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// MemDecisionRepository is an in-memory, goroutine-safe ports.DecisionRepository.
type MemDecisionRepository struct {
	mu     sync.RWMutex
	byID   map[domain.DecisionID]*domain.Decision
	byTask map[domain.TaskID][]domain.DecisionID
	seq    int
}

// NewMemDecisionRepository constructs an empty decision repository.
func NewMemDecisionRepository() *MemDecisionRepository {
	return &MemDecisionRepository{
		byID:   make(map[domain.DecisionID]*domain.Decision),
		byTask: make(map[domain.TaskID][]domain.DecisionID),
	}
}

// Create stores a new decision. Decisions are durable: this is the only way
// a decision id is introduced (superseding creates a new one; it never
// mutates ids).
func (r *MemDecisionRepository) Create(_ context.Context, d *domain.Decision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[d.ID]; ok {
		return fmt.Errorf("%w: decision %s", ports.ErrAlreadyExists, d.ID)
	}
	r.byID[d.ID] = d.Clone()
	r.byTask[d.TaskID] = append(r.byTask[d.TaskID], d.ID)
	return nil
}

// Update overwrites an existing decision (e.g. status transitions).
func (r *MemDecisionRepository) Update(_ context.Context, d *domain.Decision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[d.ID]; !ok {
		return fmt.Errorf("%w: decision %s", ports.ErrNotFound, d.ID)
	}
	r.byID[d.ID] = d.Clone()
	return nil
}

// Get returns a deep copy of the decision with the given id.
func (r *MemDecisionRepository) Get(_ context.Context, id domain.DecisionID) (*domain.Decision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: decision %s", ports.ErrNotFound, id)
	}
	return d.Clone(), nil
}

// ListByTask returns a task's decisions in creation order, including
// superseded ones: history is never hidden.
func (r *MemDecisionRepository) ListByTask(_ context.Context, id domain.TaskID) ([]*domain.Decision, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := r.byTask[id]
	out := make([]*domain.Decision, 0, len(ids))
	for _, did := range ids {
		out = append(out, r.byID[did].Clone())
	}
	return out, nil
}

// NextSequence returns 1, 2, 3, ... on successive calls, used to mint "D-00N"
// ids.
func (r *MemDecisionRepository) NextSequence(_ context.Context) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seq++
	return r.seq, nil
}
