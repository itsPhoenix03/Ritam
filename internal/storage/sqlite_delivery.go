package storage

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// ---------------------------------------------------------------------------
// deliveries
// ---------------------------------------------------------------------------

type sqlDeliveries struct{ db dbtx }

var _ ports.DeliveryRepository = (*sqlDeliveries)(nil)

func (r *sqlDeliveries) Enqueue(ctx context.Context, d *domain.Delivery) error {
	status := d.Status
	if status == "" {
		status = domain.DeliveryQueued
	}
	sent := ""
	if d.SentAt != nil {
		sent = formatTime(*d.SentAt)
	}
	return insert(ctx, r.db, "delivery", string(d.MessageID)+" to "+string(d.Recipient),
		`INSERT INTO deliveries(message_id, recipient, task_id, status, turn_id, queued_at, sent_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		string(d.MessageID), string(d.Recipient), string(d.TaskID), string(status), string(d.TurnID),
		formatTime(d.QueuedAt), sent)
}

func (r *sqlDeliveries) MarkSent(ctx context.Context, task domain.TaskID, to domain.AgentID, ids []domain.MessageID, turn domain.TurnID, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	args := []any{string(domain.DeliverySent), string(turn), formatTime(at), string(task), string(to)}
	for _, id := range ids {
		args = append(args, string(id))
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE deliveries SET status = ?, turn_id = ?, sent_at = ?
		 WHERE task_id = ? AND recipient = ? AND message_id IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return fmt.Errorf("sqlite: mark deliveries sent: %w", err)
	}
	return nil
}

func (r *sqlDeliveries) SettleTurn(ctx context.Context, turn domain.TurnID, status domain.DeliveryStatus) error {
	_, err := r.db.ExecContext(ctx, `UPDATE deliveries SET status = ? WHERE turn_id = ? AND status <> ?`,
		string(status), string(turn), string(domain.DeliveryQueued))
	if err != nil {
		return fmt.Errorf("sqlite: settle the deliveries of %s: %w", turn, err)
	}
	return nil
}

func (r *sqlDeliveries) ListByTask(ctx context.Context, task domain.TaskID) ([]*domain.Delivery, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT message_id, recipient, task_id, status, turn_id, queued_at, sent_at
		 FROM deliveries WHERE task_id = ? ORDER BY seq`, string(task))
	if err != nil {
		return nil, fmt.Errorf("sqlite: list deliveries: %w", err)
	}
	defer rows.Close()
	out := []*domain.Delivery{}
	for rows.Next() {
		var (
			d                                   domain.Delivery
			msg, to, tid, status, turn, qa, sat string
		)
		if err := rows.Scan(&msg, &to, &tid, &status, &turn, &qa, &sat); err != nil {
			return nil, err
		}
		d.MessageID, d.Recipient, d.TaskID = domain.MessageID(msg), domain.AgentID(to), domain.TaskID(tid)
		d.Status, d.TurnID = domain.DeliveryStatus(status), domain.TurnID(turn)
		if d.QueuedAt, err = parseTime(qa); err != nil {
			return nil, fmt.Errorf("sqlite: delivery of %s queued_at: %w", msg, err)
		}
		if sat != "" {
			at, err := parseTime(sat)
			if err != nil {
				return nil, fmt.Errorf("sqlite: delivery of %s sent_at: %w", msg, err)
			}
			d.SentAt = &at
		}
		out = append(out, &d)
	}
	return out, rows.Err()
}

func (r *sqlDeliveries) Discard(ctx context.Context, task domain.TaskID, to domain.AgentID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM deliveries WHERE task_id = ? AND recipient = ? AND status = ?`,
		string(task), string(to), string(domain.DeliveryQueued))
	if err != nil {
		return fmt.Errorf("sqlite: discard deliveries: %w", err)
	}
	return nil
}

func (r *sqlDeliveries) Clear(ctx context.Context, task domain.TaskID) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM deliveries WHERE task_id = ? AND status <> ?`,
		string(task), string(domain.DeliveryAcknowledged))
	if err != nil {
		return fmt.Errorf("sqlite: clear deliveries: %w", err)
	}
	return nil
}
