package storage

import (
	"context"
	"fmt"
	"sync"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// MemDisagreementRepository is an in-memory, goroutine-safe
// ports.DisagreementRepository. Disagreements are resolved in place and never
// removed, so the record of what was claimed, what was disputed and how it was
// settled survives the task.
type MemDisagreementRepository struct {
	mu     sync.RWMutex
	byID   map[domain.DisagreementID]*domain.Disagreement
	byTask map[domain.TaskID][]domain.DisagreementID
}

// NewMemDisagreementRepository constructs an empty disagreement repository.
func NewMemDisagreementRepository() *MemDisagreementRepository {
	return &MemDisagreementRepository{
		byID:   make(map[domain.DisagreementID]*domain.Disagreement),
		byTask: make(map[domain.TaskID][]domain.DisagreementID),
	}
}

// Create stores a new disagreement.
func (r *MemDisagreementRepository) Create(_ context.Context, d *domain.Disagreement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[d.ID]; ok {
		return fmt.Errorf("%w: disagreement %s", ports.ErrAlreadyExists, d.ID)
	}
	r.byID[d.ID] = d.Clone()
	r.byTask[d.TaskID] = append(r.byTask[d.TaskID], d.ID)
	return nil
}

// Update overwrites an existing disagreement, e.g. to record its resolution.
func (r *MemDisagreementRepository) Update(_ context.Context, d *domain.Disagreement) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[d.ID]; !ok {
		return fmt.Errorf("%w: disagreement %s", ports.ErrNotFound, d.ID)
	}
	r.byID[d.ID] = d.Clone()
	return nil
}

// Get returns a deep copy of one disagreement.
func (r *MemDisagreementRepository) Get(_ context.Context, id domain.DisagreementID) (*domain.Disagreement, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	d, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: disagreement %s", ports.ErrNotFound, id)
	}
	return d.Clone(), nil
}

// ListByTask returns every disagreement of a task, in the order raised.
func (r *MemDisagreementRepository) ListByTask(_ context.Context, id domain.TaskID) ([]*domain.Disagreement, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := r.byTask[id]
	out := make([]*domain.Disagreement, 0, len(ids))
	for _, did := range ids {
		out = append(out, r.byID[did].Clone())
	}
	return out, nil
}

// ListOpen returns the unresolved disagreements of a task.
func (r *MemDisagreementRepository) ListOpen(ctx context.Context, id domain.TaskID) ([]*domain.Disagreement, error) {
	all, err := r.ListByTask(ctx, id)
	if err != nil {
		return nil, err
	}
	open := make([]*domain.Disagreement, 0, len(all))
	for _, d := range all {
		if !d.Resolved {
			open = append(open, d)
		}
	}
	return open, nil
}
