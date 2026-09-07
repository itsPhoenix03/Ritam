package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Delivery: each agent's inbox.
//
// An agent CLI takes a turn per delivery, a minute or more each. Handing it
// every message the moment it was routed meant a busy agent built up a queue
// of stale turns: in a real run codex received 64 messages, took 16 turns,
// and was 48 messages behind at the end. It spent its turns answering
// messages ten minutes old, posted revisions that had already been
// superseded, and never reached the human's question at all.
//
// So messages for a running task wait in the recipient's inbox, and the
// inbox is handed over when the agent is free:
//
//   - Combined. Everything waiting goes over in one turn (for adapters that
//     read a prompt; see ports.BatchCapable), so the agent answers with all
//     of it in view and the human's messages first.
//   - Held while the agent waits on a co-worker. An agent that asked another
//     for something, while that agent is still working on it, is not woken
//     by the chatter in between: its messages wait for the answers, so it
//     acts on them together and on the same footing as everyone else. The
//     human's messages and requests addressed to the agent always go
//     through, and a hold never outlasts maxHold.
//   - With the team in view. Every delivery carries a status board: who is
//     working, how much each has unread, who is waiting on whom. Every agent
//     can read it; only the authority may act on it.

// maxHold bounds how long an agent's messages wait for the answers it needs.
var maxHold = 4 * time.Minute

// dispatchInterval is how often a running task's inboxes are checked for an
// agent that has become free.
var dispatchInterval = 250 * time.Millisecond

// taskInbox holds one running task's undelivered messages.
type taskInbox struct {
	mu        sync.Mutex
	pending   map[domain.AgentID][]*domain.Message
	heldSince map[domain.AgentID]time.Time
	flushing  map[domain.AgentID]bool
}

func newTaskInbox() *taskInbox {
	return &taskInbox{
		pending:   map[domain.AgentID][]*domain.Message{},
		heldSince: map[domain.AgentID]time.Time{},
		flushing:  map[domain.AgentID]bool{},
	}
}

// unread reports how many messages wait for agent.
func (ib *taskInbox) unread(agent domain.AgentID) int {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	return len(ib.pending[agent])
}

// openInbox starts holding messages for a task about to run.
func (o *Orchestrator) openInbox(id domain.TaskID) {
	o.mu.Lock()
	o.inboxes[id] = newTaskInbox()
	o.mu.Unlock()
}

// closeInbox stops holding messages for a task whose run has ended. Anything
// undelivered is dropped: the messages are in the history, and a resumed run
// asks again for whatever is still owed.
func (o *Orchestrator) closeInbox(id domain.TaskID) {
	o.mu.Lock()
	delete(o.inboxes, id)
	o.mu.Unlock()
}

func (o *Orchestrator) inbox(id domain.TaskID) *taskInbox {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.inboxes[id]
}

// Backlog reports how many messages wait in each agent's inbox for a running
// task.
func (o *Orchestrator) Backlog(id domain.TaskID) map[domain.AgentID]int {
	ib := o.inbox(id)
	if ib == nil {
		return nil
	}
	ib.mu.Lock()
	defer ib.mu.Unlock()
	out := map[domain.AgentID]int{}
	for a, msgs := range ib.pending {
		if len(msgs) > 0 {
			out[a] = len(msgs)
		}
	}
	return out
}

// handoff is the bus's Handoff hook: it puts a message for a running task in
// the recipient's inbox and tries to hand the inbox over at once.
func (o *Orchestrator) handoff(ctx context.Context, t *domain.Task, m *domain.Message) bool {
	ib := o.inbox(t.ID)
	if ib == nil {
		return false
	}
	ib.mu.Lock()
	ib.pending[m.ToAgent] = append(ib.pending[m.ToAgent], m)
	ib.mu.Unlock()
	o.dispatch(ctx, t.ID)
	return true
}

// dispatch hands over every inbox whose agent is free and not held. Agents
// waiting on nobody go first, so that an agent another is waiting on is seen
// working before the waiting one is considered.
func (o *Orchestrator) dispatch(ctx context.Context, id domain.TaskID) {
	ib := o.inbox(id)
	if ib == nil {
		return
	}
	ib.mu.Lock()
	var agents []domain.AgentID
	for a, msgs := range ib.pending {
		if len(msgs) > 0 {
			agents = append(agents, a)
		}
	}
	ib.mu.Unlock()
	if len(agents) == 0 {
		return
	}
	t, err := o.Tasks.Get(ctx, id)
	if err != nil {
		return
	}
	ledger, err := o.ledger(ctx, id)
	if err != nil {
		return
	}
	sort.Slice(agents, func(i, j int) bool {
		wi, wj := len(waitsOn(ledger, t, agents[i])) > 0, len(waitsOn(ledger, t, agents[j])) > 0
		if wi != wj {
			return !wi
		}
		return agents[i] < agents[j]
	})
	for _, a := range agents {
		o.flush(ctx, t, ledger, ib, a)
	}
}

// flush hands agent its inbox if it is free and not held.
func (o *Orchestrator) flush(ctx context.Context, t *domain.Task, ledger []domain.Request, ib *taskInbox, agent domain.AgentID) {
	ad, ok := o.Agents.AdapterFor(t.ID, agent)
	if !ok || ad.Status() == domain.AgentStateWorking {
		return
	}
	// With the decision accepted and only the human's questions left, only
	// the agents that owe the human a reply take another turn.
	if o.isClosing(t.ID) && !owesHuman(ledger, agent) {
		return
	}
	ib.mu.Lock()
	if ib.flushing[agent] || len(ib.pending[agent]) == 0 {
		ib.mu.Unlock()
		return
	}
	waiting := append([]*domain.Message(nil), ib.pending[agent]...)
	since := ib.heldSince[agent]
	ib.flushing[agent] = true
	ib.mu.Unlock()
	defer func() {
		ib.mu.Lock()
		ib.flushing[agent] = false
		ib.mu.Unlock()
	}()

	if on := o.holdFor(t, ledger, agent, waiting, since); len(on) > 0 {
		ib.mu.Lock()
		if ib.heldSince[agent].IsZero() {
			ib.heldSince[agent] = time.Now()
			ib.mu.Unlock()
			o.Events.emit(ctx, domain.EvtDeliveryHeld, t.ID, agent,
				fmt.Sprintf("%s's %d message(s) wait until %s answer%s", agent, len(waiting),
					listAgents(on), plural(len(on) == 1, "s", "")),
				map[string]string{"waitingOn": listAgents(on)})
		} else {
			ib.mu.Unlock()
		}
		return
	}

	ib.mu.Lock()
	msgs := ib.pending[agent]
	delete(ib.pending, agent)
	delete(ib.heldSince, agent)
	ib.mu.Unlock()

	board := o.teamBoard(t, ledger, ib, agent)
	o.deliverInbox(ctx, t, ad, agent, msgs, board)
}

// holdFor returns the co-workers agent's messages wait for, or nothing when
// they should be handed over now.
func (o *Orchestrator) holdFor(t *domain.Task, ledger []domain.Request, agent domain.AgentID, waiting []*domain.Message, since time.Time) []domain.AgentID {
	if !since.IsZero() && time.Since(since) >= maxHold {
		return nil
	}
	ids := map[domain.MessageID]bool{}
	for _, m := range waiting {
		if m.FromAgent == domain.HumanAgentID {
			return nil // the human never waits
		}
		ids[m.ID] = true
	}
	for _, r := range ledger {
		if r.Open() && r.Message.ToAgent == agent && ids[r.Message.ID] {
			return nil // someone needs this agent: holding it could stall them
		}
	}
	var busy []domain.AgentID
	for _, y := range waitsOn(ledger, t, agent) {
		if ad, ok := o.Agents.AdapterFor(t.ID, y); ok && ad.Status() == domain.AgentStateWorking {
			busy = append(busy, y)
		}
	}
	// Only a co-worker still working will produce the answer; waiting on an
	// idle one would wait for nothing (the reminders deal with silence).
	return busy
}

// owesHuman reports whether agent has a reply to the human outstanding.
func owesHuman(ledger []domain.Request, agent domain.AgentID) bool {
	for _, r := range ledger {
		if r.Open() && r.Message.FromAgent == domain.HumanAgentID && r.Message.ToAgent == agent {
			return true
		}
	}
	return false
}

// waitsOn lists the participants agent has an open request with.
func waitsOn(ledger []domain.Request, t *domain.Task, agent domain.AgentID) []domain.AgentID {
	seen := map[domain.AgentID]bool{}
	var out []domain.AgentID
	for _, r := range ledger {
		q := r.Message
		if !r.Open() || q.FromAgent != agent || q.ToAgent == domain.HumanAgentID || !t.HasParticipant(q.ToAgent) || seen[q.ToAgent] {
			continue
		}
		seen[q.ToAgent] = true
		out = append(out, q.ToAgent)
	}
	return out
}

// deliverInbox hands msgs to agent: as one combined turn when the adapter
// reads a prompt and there is more than one, otherwise one by one.
func (o *Orchestrator) deliverInbox(ctx context.Context, t *domain.Task, ad ports.AgentAdapter, agent domain.AgentID, msgs []*domain.Message, board string) {
	batch := len(msgs) > 1
	if bc, ok := ad.(ports.BatchCapable); !ok || !bc.AcceptsBatches() {
		batch = false
	}
	var deliveries []*domain.Message
	if batch {
		deliveries = []*domain.Message{combine(msgs)}
	} else {
		deliveries = msgs
	}
	for _, d := range deliveries {
		if board != "" {
			if d.Metadata == nil {
				d.Metadata = map[string]string{}
			}
			d.Metadata[ports.TeamMetadataKey] = board
		}
		var err error
		if d.Type == domain.MsgTaskAssignment {
			err = o.Agents.SendTask(ctx, t, *d)
		} else {
			err = o.Agents.Send(ctx, t, *d)
		}
		if err != nil {
			o.log.Error("deliver message", "taskId", string(t.ID), "agentId", string(agent), "error", err)
			o.Events.emit(ctx, domain.EvtAgentFailed, t.ID, agent, fmt.Sprintf("delivery to %s failed: %v", agent, err), nil)
			return
		}
	}
	for _, m := range msgs {
		details := map[string]string{"messageId": string(m.ID)}
		summary := fmt.Sprintf("%s received %s from %s", agent, m.Type, m.FromAgent)
		if batch {
			details["batch"] = fmt.Sprint(len(msgs))
			summary += fmt.Sprintf(" (one of %d in one turn)", len(msgs))
		}
		o.Events.emit(ctx, domain.EvtAgentMessageReceived, t.ID, agent, summary, details)
	}
}

// combine lays several messages out as one delivery, oldest first. The
// result is never recorded: each message is already in the history.
func combine(msgs []*domain.Message) *domain.Message {
	first, last := msgs[0], msgs[len(msgs)-1]
	env := &domain.Message{
		ID:        domain.MessageID("BATCH-" + string(last.ID)),
		TaskID:    first.TaskID,
		ToAgent:   first.ToAgent,
		FromAgent: first.FromAgent,
		Type:      domain.MsgInformation,
		Timestamp: last.Timestamp,
		Metadata:  map[string]string{ports.BatchMetadataKey: fmt.Sprint(len(msgs))},
	}
	var b strings.Builder
	var knowledge []string
	seenKnowledge := map[string]bool{}
	for i, m := range msgs {
		if m.FromAgent != first.FromAgent {
			env.FromAgent = domain.BroadcastAgentID
		}
		if m.Type == domain.MsgTaskAssignment {
			env.Type = domain.MsgTaskAssignment
		}
		from := string(m.FromAgent)
		if m.FromAgent == domain.HumanAgentID {
			from = "[HUMAN] human"
			if m.Metadata["orchestrator"] == "" {
				env.Metadata[ports.BatchHumanMetadataKey] = "true"
			}
		}
		fmt.Fprintf(&b, "=== %d of %d · %s from %s · %s · %s ===\n", i+1, len(msgs), m.Type, from,
			m.Timestamp.Format("15:04:05"), m.ID)
		if note := strings.TrimSpace(m.Metadata[ports.ThreadMetadataKey]); note != "" {
			fmt.Fprintf(&b, "(This is part of a question from the human: %s. If you can answer it, answer the human directly.)\n", note)
		}
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteString("\n\n")
		for k, v := range m.Metadata {
			switch k {
			case ports.RecapMetadataKey:
				if env.Metadata[k] == "" {
					env.Metadata[k] = v
				}
			case ports.FollowUpMetadataKey:
				env.Metadata[k] = v
			case "knowledge":
				if v != "" && !seenKnowledge[v] {
					seenKnowledge[v] = true
					knowledge = append(knowledge, v)
				}
			}
		}
	}
	if len(knowledge) > 0 {
		env.Metadata["knowledge"] = strings.Join(knowledge, "\n")
	}
	env.Content = strings.TrimSpace(b.String())
	return env
}

// teamBoard describes every participant for the agent being delivered to:
// state, unread messages, and who it waits on and owes.
func (o *Orchestrator) teamBoard(t *domain.Task, ledger []domain.Request, ib *taskInbox, self domain.AgentID) string {
	now := time.Now()
	var b strings.Builder
	for _, p := range t.Participants {
		state := "stopped"
		if ad, ok := o.Agents.AdapterFor(t.ID, p); ok {
			state = strings.ToLower(strings.TrimPrefix(string(ad.Status()), "AGENT_"))
		}
		name := string(p)
		if p == self {
			name += " (you)"
		}
		if p == t.AuthorityAgent {
			name += " (authority)"
		}
		parts := []string{state}
		if n := ib.unread(p); n > 0 && p != self {
			parts = append(parts, fmt.Sprintf("%d unread", n))
		}
		for _, r := range ledger {
			q := r.Message
			if !r.Open() {
				continue
			}
			age := shortAge(now.Sub(q.Timestamp))
			if q.FromAgent == p && q.ToAgent != domain.BroadcastAgentID {
				parts = append(parts, fmt.Sprintf("waiting on %s (%s, %s)", q.ToAgent, q.Type, age))
			}
			if q.ToAgent == p {
				parts = append(parts, fmt.Sprintf("owes %s a reply (%s, %s)", q.FromAgent, q.Type, age))
			}
		}
		fmt.Fprintf(&b, "- %s: %s\n", name, strings.Join(parts, " · "))
	}
	var human []string
	for _, r := range ledger {
		if r.Open() && r.Message.FromAgent == domain.HumanAgentID {
			human = append(human, fmt.Sprintf("%s (%s)", r.Message.ToAgent, shortAge(now.Sub(r.Message.Timestamp))))
		}
	}
	if len(human) > 0 {
		fmt.Fprintf(&b, "- human: waiting on %s\n", strings.Join(human, ", "))
	}
	return strings.TrimSpace(b.String())
}

func shortAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}

func listAgents(ids []domain.AgentID) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = string(id)
	}
	return strings.Join(s, " and ")
}

func plural(one bool, single, many string) string {
	if one {
		return single
	}
	return many
}
