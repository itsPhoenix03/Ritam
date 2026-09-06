// Package decisions manages the lifecycle of durable decisions: proposing,
// accepting, rejecting and superseding them. Decisions are never deleted or
// silently rewritten; superseding always creates a new record and marks the
// old one, preserving the full history.
package decisions

import (
	"context"
	"fmt"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Manager coordinates decision lifecycle transitions over a
// ports.DecisionRepository.
type Manager struct {
	repo ports.DecisionRepository
}

// New constructs a Manager over the given repository.
func New(repo ports.DecisionRepository) *Manager {
	return &Manager{repo: repo}
}

// Propose records a new decision in PROPOSED status, assigning it a
// sequential "D-00N" id. Any status, id or timestamp the caller set on d is
// overwritten: proposing is the sole entry point for new decision ids.
func (m *Manager) Propose(ctx context.Context, d *domain.Decision) (*domain.Decision, error) {
	n, err := m.repo.NextSequence(ctx)
	if err != nil {
		return nil, fmt.Errorf("propose decision: %w", err)
	}
	cp := d.Clone()
	cp.ID = domain.FormatDecisionID(n)
	cp.Status = domain.DecisionProposed
	cp.SupersededBy = ""
	cp.Timestamp = time.Now()
	if err := m.repo.Create(ctx, cp); err != nil {
		return nil, err
	}
	return cp.Clone(), nil
}

// Accept transitions a PROPOSED decision to ACCEPTED. Accepting anything
// other than a PROPOSED decision is an illegal transition.
func (m *Manager) Accept(ctx context.Context, id domain.DecisionID) (*domain.Decision, error) {
	d, err := m.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Status != domain.DecisionProposed {
		return nil, fmt.Errorf("decision %s: cannot accept from status %s", id, d.Status)
	}
	d.Status = domain.DecisionAccepted
	d.Timestamp = time.Now()
	if err := m.repo.Update(ctx, d); err != nil {
		return nil, err
	}
	return d.Clone(), nil
}

// Reject transitions a PROPOSED decision to REJECTED, recording why in its
// Reasoning. Rejecting anything other than a PROPOSED decision is an illegal
// transition.
func (m *Manager) Reject(ctx context.Context, id domain.DecisionID, reason string) (*domain.Decision, error) {
	d, err := m.repo.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if d.Status != domain.DecisionProposed {
		return nil, fmt.Errorf("decision %s: cannot reject from status %s", id, d.Status)
	}
	d.Status = domain.DecisionRejected
	d.Reasoning = reason
	d.Timestamp = time.Now()
	if err := m.repo.Update(ctx, d); err != nil {
		return nil, err
	}
	return d.Clone(), nil
}

// Supersede creates newDecision as a fresh PROPOSED decision (via Propose)
// that points back at oldID via Supersedes, and marks oldID SUPERSEDED with
// SupersededBy set to the new decision's id. The old decision is never
// deleted or otherwise mutated beyond its status and SupersededBy fields.
//
// Superseding a decision that is already SUPERSEDED is rejected: chains must
// be built from the current head, not an arbitrary past link, so history
// stays a clean, single-threaded chain per lineage.
func (m *Manager) Supersede(ctx context.Context, oldID domain.DecisionID, newDecision *domain.Decision) (*domain.Decision, error) {
	old, err := m.repo.Get(ctx, oldID)
	if err != nil {
		return nil, err
	}
	if old.Status == domain.DecisionSuperseded {
		return nil, fmt.Errorf("decision %s: already superseded by %s", oldID, old.SupersededBy)
	}

	cp := newDecision.Clone()
	cp.TaskID = old.TaskID
	cp.Supersedes = oldID
	created, err := m.Propose(ctx, cp)
	if err != nil {
		return nil, err
	}

	old.Status = domain.DecisionSuperseded
	old.SupersededBy = created.ID
	old.Timestamp = time.Now()
	if err := m.repo.Update(ctx, old); err != nil {
		return nil, err
	}
	return created, nil
}

// Get returns the decision with the given id.
func (m *Manager) Get(ctx context.Context, id domain.DecisionID) (*domain.Decision, error) {
	return m.repo.Get(ctx, id)
}

// ListByTask returns every decision recorded for a task, in creation order,
// including superseded and rejected ones.
func (m *Manager) ListByTask(ctx context.Context, taskID domain.TaskID) ([]*domain.Decision, error) {
	return m.repo.ListByTask(ctx, taskID)
}

// Latest returns the most recently created decision for a task that is not
// SUPERSEDED (i.e. it may be PROPOSED, ACCEPTED or REJECTED). It returns
// ports.ErrNotFound if the task has no such decision.
func (m *Manager) Latest(ctx context.Context, taskID domain.TaskID) (*domain.Decision, error) {
	all, err := m.repo.ListByTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	var latest *domain.Decision
	for _, d := range all {
		if d.Status == domain.DecisionSuperseded {
			continue
		}
		if latest == nil || d.Timestamp.After(latest.Timestamp) {
			latest = d
		}
	}
	if latest == nil {
		return nil, fmt.Errorf("%w: no non-superseded decision for task %s", ports.ErrNotFound, taskID)
	}
	return latest, nil
}
