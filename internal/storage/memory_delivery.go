package storage

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// MemDeliveryRepository is an in-memory, goroutine-safe
// ports.DeliveryRepository.
type MemDeliveryRepository struct {
	mu sync.RWMutex
	// rows is in queue order. A write replaces a row rather than changing it
	// in place, so a transaction's snapshot of the slice stays valid.
	rows []*domain.Delivery
}

// NewMemDeliveryRepository constructs an empty delivery repository.
func NewMemDeliveryRepository() *MemDeliveryRepository { return &MemDeliveryRepository{} }

func (r *MemDeliveryRepository) find(id domain.MessageID, to domain.AgentID) int {
	for i, d := range r.rows {
		if d.MessageID == id && d.Recipient == to {
			return i
		}
	}
	return -1
}

// Enqueue records a waiting delivery.
func (r *MemDeliveryRepository) Enqueue(_ context.Context, d *domain.Delivery) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.find(d.MessageID, d.Recipient) >= 0 {
		return fmt.Errorf("%w: delivery of %s to %s", ports.ErrAlreadyExists, d.MessageID, d.Recipient)
	}
	cp := d.Clone()
	if cp.Status == "" {
		cp.Status = domain.DeliveryQueued
	}
	r.rows = append(slices.Clip(r.rows), cp)
	return nil
}

// update replaces every row keep selects with what change makes of a copy.
func (r *MemDeliveryRepository) update(keep func(*domain.Delivery) bool, change func(*domain.Delivery)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rows := slices.Clone(r.rows)
	for i, d := range rows {
		if keep(d) {
			cp := d.Clone()
			change(cp)
			rows[i] = cp
		}
	}
	r.rows = rows
}

// MarkSent records the turn that carried ids to recipient.
func (r *MemDeliveryRepository) MarkSent(_ context.Context, task domain.TaskID, to domain.AgentID, ids []domain.MessageID, turn domain.TurnID, at time.Time) error {
	r.update(func(d *domain.Delivery) bool {
		return d.TaskID == task && d.Recipient == to && slices.Contains(ids, d.MessageID)
	}, func(d *domain.Delivery) {
		sent := at
		d.Status, d.TurnID, d.SentAt = domain.DeliverySent, turn, &sent
	})
	return nil
}

// SettleTurn moves what turn carried, and is not queued again, to status.
func (r *MemDeliveryRepository) SettleTurn(_ context.Context, turn domain.TurnID, status domain.DeliveryStatus) error {
	r.update(func(d *domain.Delivery) bool {
		return d.TurnID == turn && d.Status != domain.DeliveryQueued
	}, func(d *domain.Delivery) { d.Status = status })
	return nil
}

// ListByTask returns a task's deliveries in queue order.
func (r *MemDeliveryRepository) ListByTask(_ context.Context, task domain.TaskID) ([]*domain.Delivery, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := []*domain.Delivery{}
	for _, d := range r.rows {
		if d.TaskID == task {
			out = append(out, d.Clone())
		}
	}
	return out, nil
}

func (r *MemDeliveryRepository) remove(drop func(*domain.Delivery) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rows = slices.DeleteFunc(slices.Clone(r.rows), drop)
}

// Discard forgets what still waits for recipient.
func (r *MemDeliveryRepository) Discard(_ context.Context, task domain.TaskID, to domain.AgentID) error {
	r.remove(func(d *domain.Delivery) bool {
		return d.TaskID == task && d.Recipient == to && d.Status == domain.DeliveryQueued
	})
	return nil
}

// Clear forgets every unacknowledged delivery of the task.
func (r *MemDeliveryRepository) Clear(_ context.Context, task domain.TaskID) error {
	r.remove(func(d *domain.Delivery) bool {
		return d.TaskID == task && d.Status != domain.DeliveryAcknowledged
	})
	return nil
}

func (r *MemDeliveryRepository) snapshot() func() {
	r.mu.RLock()
	rows := r.rows
	r.mu.RUnlock()
	return func() {
		r.mu.Lock()
		r.rows = rows
		r.mu.Unlock()
	}
}

type gatedDeliveries struct {
	d memDoor
	r *MemDeliveryRepository
}

func (g *gatedDeliveries) Enqueue(ctx context.Context, d *domain.Delivery) error {
	return through0(g.d, func() error { return g.r.Enqueue(ctx, d) })
}
func (g *gatedDeliveries) MarkSent(ctx context.Context, task domain.TaskID, to domain.AgentID, ids []domain.MessageID, turn domain.TurnID, at time.Time) error {
	return through0(g.d, func() error { return g.r.MarkSent(ctx, task, to, ids, turn, at) })
}
func (g *gatedDeliveries) SettleTurn(ctx context.Context, turn domain.TurnID, status domain.DeliveryStatus) error {
	return through0(g.d, func() error { return g.r.SettleTurn(ctx, turn, status) })
}
func (g *gatedDeliveries) ListByTask(ctx context.Context, task domain.TaskID) ([]*domain.Delivery, error) {
	return through(g.d, func() ([]*domain.Delivery, error) { return g.r.ListByTask(ctx, task) })
}
func (g *gatedDeliveries) Discard(ctx context.Context, task domain.TaskID, to domain.AgentID) error {
	return through0(g.d, func() error { return g.r.Discard(ctx, task, to) })
}
func (g *gatedDeliveries) Clear(ctx context.Context, task domain.TaskID) error {
	return through0(g.d, func() error { return g.r.Clear(ctx, task) })
}

var _ ports.DeliveryRepository = (*gatedDeliveries)(nil)
