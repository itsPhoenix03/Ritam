package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/review"
)

// approvals holds the tool calls waiting for the human. It is the review's
// Asker: a question is published as an event for the TUI, and the call's
// hook waits until the human answers, the question times out, or the turn
// goes away. Nobody is asked until a human is attending (the TUI is up):
// a headless run refuses such calls at once.
type approvals struct {
	mu       sync.Mutex
	waiting  map[string]*waitingCall
	attended bool
	publish  func(domain.Event)
}

type waitingCall struct {
	q      review.Question
	answer chan review.Answer
}

func newApprovals(publish func(domain.Event)) *approvals {
	return &approvals{waiting: map[string]*waitingCall{}, publish: publish}
}

// attend says whether a human is there to answer.
func (a *approvals) attend(on bool) {
	a.mu.Lock()
	a.attended = on
	a.mu.Unlock()
}

// Ask implements review.Asker.
func (a *approvals) Ask(ctx context.Context, q review.Question) (review.Answer, error) {
	a.mu.Lock()
	if !a.attended {
		a.mu.Unlock()
		return review.Answer{}, review.ErrNobody
	}
	w := &waitingCall{q: q, answer: make(chan review.Answer, 1)}
	a.waiting[q.ID] = w
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.waiting, q.ID)
		a.mu.Unlock()
	}()

	if a.publish != nil {
		a.publish(approvalEvent(q))
	}
	select {
	case ans := <-w.answer:
		return ans, nil
	case <-ctx.Done():
		return review.Answer{}, ctx.Err()
	}
}

// approvalEvent is the event asking the human about a call.
func approvalEvent(q review.Question) domain.Event {
	what := q.Tool
	if q.Summary != "" {
		what += ": " + q.Summary
	}
	return domain.Event{
		Type:    domain.EvtToolApproval,
		TaskID:  q.Task,
		AgentID: q.Agent,
		Summary: fmt.Sprintf("%s asks to run %s", q.Agent, what),
		Details: map[string]string{
			"approval": q.ID,
			"tool":     q.Tool,
			"severity": q.Severity,
			"reasons":  strings.Join(q.Reasons, "; "),
			"deadline": q.Deadline.Format(time.RFC3339),
		},
	}
}

// answer delivers the human's decision on a waiting call.
func (a *approvals) answer(id string, ans review.Answer) error {
	a.mu.Lock()
	w, ok := a.waiting[id]
	if ok {
		delete(a.waiting, id)
	}
	a.mu.Unlock()
	if !ok {
		return fmt.Errorf("no tool call is waiting as %s: it was answered, timed out or withdrawn", id)
	}
	w.answer <- ans
	return nil
}

// pending lists the waiting calls, oldest first.
func (a *approvals) pending() []ports.Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]ports.Approval, 0, len(a.waiting))
	for _, w := range a.waiting {
		q := w.q
		out = append(out, ports.Approval{ID: q.ID, Task: q.Task, Agent: q.Agent, Tool: q.Tool, Summary: q.Summary,
			Reasons: slices.Clone(q.Reasons), Severity: q.Severity, Manual: q.Manual, Deadline: q.Deadline})
	}
	slices.SortFunc(out, func(x, y ports.Approval) int { return x.Deadline.Compare(y.Deadline) })
	return out
}
