package storage

import (
	"context"
	"fmt"
	"sync"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// MemArtifactRepository is an in-memory, goroutine-safe ports.ArtifactRepository.
type MemArtifactRepository struct {
	mu     sync.RWMutex
	byID   map[domain.ArtifactID]*domain.Artifact
	byTask map[domain.TaskID][]domain.ArtifactID
}

// NewMemArtifactRepository constructs an empty artifact repository.
func NewMemArtifactRepository() *MemArtifactRepository {
	return &MemArtifactRepository{
		byID:   make(map[domain.ArtifactID]*domain.Artifact),
		byTask: make(map[domain.TaskID][]domain.ArtifactID),
	}
}

// Create stores a new artifact record.
func (r *MemArtifactRepository) Create(_ context.Context, a *domain.Artifact) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.byID[a.ID]; ok {
		return fmt.Errorf("%w: artifact %s", ports.ErrAlreadyExists, a.ID)
	}
	r.byID[a.ID] = a.Clone()
	r.byTask[a.TaskID] = append(r.byTask[a.TaskID], a.ID)
	return nil
}

// Get returns a deep copy of the artifact with the given id.
func (r *MemArtifactRepository) Get(_ context.Context, id domain.ArtifactID) (*domain.Artifact, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byID[id]
	if !ok {
		return nil, fmt.Errorf("%w: artifact %s", ports.ErrNotFound, id)
	}
	return a.Clone(), nil
}

// ListByTask returns a task's artifacts in creation order.
func (r *MemArtifactRepository) ListByTask(_ context.Context, id domain.TaskID) ([]*domain.Artifact, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ids := r.byTask[id]
	out := make([]*domain.Artifact, 0, len(ids))
	for _, aid := range ids {
		out = append(out, r.byID[aid].Clone())
	}
	return out, nil
}
