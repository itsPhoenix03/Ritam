package domain

import "time"

// DeliveryStatus is where one message stands on its way to one recipient.
type DeliveryStatus string

const (
	// DeliveryQueued waits in the recipient's inbox.
	DeliveryQueued DeliveryStatus = "queued"
	// DeliverySent was handed to the recipient's adapter in a turn that has
	// not ended yet.
	DeliverySent DeliveryStatus = "sent"
	// DeliveryAcknowledged was carried by a turn that ended without failing.
	DeliveryAcknowledged DeliveryStatus = "acknowledged"
)

// TurnID identifies one agent turn: one hand-over to an adapter and what the
// agent did with it.
type TurnID string

// NewTurnID returns a fresh turn identifier.
func NewTurnID() TurnID { return TurnID("TURN-" + randomSuffix()) }

// Delivery is one stored message on its way to one recipient. Inboxes used
// to live only in memory, and every end of a run dropped what was in them: a
// reply that reached a busy agent was lost to a pause, and the agent was told
// to carry on without it.
type Delivery struct {
	MessageID MessageID
	Recipient AgentID
	TaskID    TaskID
	Status    DeliveryStatus
	// TurnID is the turn that last carried the message. It is kept when the
	// message goes back to the queue, so the next delivery can say the agent
	// may have seen it before.
	TurnID   TurnID
	QueuedAt time.Time
	SentAt   *time.Time
}

// Clone returns a copy of the delivery.
func (d *Delivery) Clone() *Delivery {
	if d == nil {
		return nil
	}
	cp := *d
	if d.SentAt != nil {
		at := *d.SentAt
		cp.SentAt = &at
	}
	return &cp
}
