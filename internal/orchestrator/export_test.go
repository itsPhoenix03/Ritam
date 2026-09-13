package orchestrator

import (
	"context"
	"time"

	"projectorch/internal/domain"
)

// SetFollowUpSettleInterval shortens the follow-up settle check for tests.
func SetFollowUpSettleInterval(d time.Duration) { followUpSettleInterval = d }

// SetHumanAnswerGrace shortens the wait before reminding an agent of the
// human's unanswered question, for tests.
func SetHumanAnswerGrace(d time.Duration) { humanAnswerGrace = d }

// IsProgress exposes the stall classifier for tests.
func (o *Orchestrator) IsProgress(ctx context.Context, t *domain.Task, m *domain.Message) Progress {
	return o.isProgress(ctx, t, m)
}

// OpenInbox starts holding messages for a task without running it, so tests
// can drive delivery step by step.
func (o *Orchestrator) OpenInbox(id domain.TaskID) { o.openInbox(id) }

// Dispatch hands over every inbox whose agent is free and not held.
func (o *Orchestrator) Dispatch(ctx context.Context, id domain.TaskID) { o.dispatch(ctx, id) }

// SetMaxHold shortens how long a hold may last, for tests.
func SetMaxHold(d time.Duration) { maxHold = d }

// AttachRecapForTest exposes the session recap for tests.
func AttachRecapForTest(o *Orchestrator, ctx context.Context, t *domain.Task, m *domain.Message) {
	o.attachRecap(ctx, t, m)
}
