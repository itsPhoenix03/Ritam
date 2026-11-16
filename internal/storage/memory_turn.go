package storage

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// MemTurnRepository is an in-memory, goroutine-safe ports.TurnRepository.
type MemTurnRepository struct {
	mu     sync.RWMutex
	byID   map[domain.TurnID]*domain.Turn
	byTask map[domain.TaskID][]domain.TurnID
}

// NewMemTurnRepository constructs an empty turn repository.
func NewMemTurnRepository() *MemTurnRepository {
	return &MemTurnRepository{byID: map[domain.TurnID]*domain.Turn{}, byTask: map[domain.TaskID][]domain.TurnID{}}
}

// Create records a new turn.
func (r *MemTurnRepository) Create(_ context.Context, t *domain.Turn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[t.ID]; ok {
		return fmt.Errorf("%w: turn %s", ports.ErrAlreadyExists, t.ID)
	}
	r.byID[t.ID] = t.Clone()
	r.byTask[t.TaskID] = append(r.byTask[t.TaskID][:len(r.byTask[t.TaskID]):len(r.byTask[t.TaskID])], t.ID)
	return nil
}

// Update rewrites a recorded turn.
func (r *MemTurnRepository) Update(_ context.Context, t *domain.Turn) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[t.ID]; !ok {
		return fmt.Errorf("%w: turn %s", ports.ErrNotFound, t.ID)
	}
	r.byID[t.ID] = t.Clone()
	return nil
}

// ListByTask returns a task's turns in the order they started.
func (r *MemTurnRepository) ListByTask(_ context.Context, task domain.TaskID) ([]*domain.Turn, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := r.byTask[task]
	out := make([]*domain.Turn, 0, len(ids))
	for _, id := range ids {
		out = append(out, r.byID[id].Clone())
	}
	return out, nil
}

func (r *MemTurnRepository) snapshot() func() {
	r.mu.RLock()
	byID, byTask := maps.Clone(r.byID), maps.Clone(r.byTask)
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.byID, r.byTask = byID, byTask
		r.mu.Unlock()
	}
}

type gatedTurns struct {
	d memDoor
	r *MemTurnRepository
}

func (g *gatedTurns) Create(ctx context.Context, t *domain.Turn) error {
	return through0(g.d, func() error { return g.r.Create(ctx, t) })
}
func (g *gatedTurns) Update(ctx context.Context, t *domain.Turn) error {
	return through0(g.d, func() error { return g.r.Update(ctx, t) })
}
func (g *gatedTurns) ListByTask(ctx context.Context, task domain.TaskID) ([]*domain.Turn, error) {
	return through(g.d, func() ([]*domain.Turn, error) { return g.r.ListByTask(ctx, task) })
}

var _ ports.TurnRepository = (*gatedTurns)(nil)
