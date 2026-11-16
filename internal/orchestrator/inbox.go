package orchestrator

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/ports"
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
//   - Durable. A message waiting in an inbox is a queued delivery in the
//     store, written with the message. Handed over, it is sent in a turn;
//     when that turn ends well it is acknowledged, and when the turn is
//     killed or fails it goes back to the queue. A run starts by refilling
//     the inboxes from the queue, so a pause, a crash or a restart loses
//     nothing that was stored. A retried turn re-sends its own batch, which
//     stays with the retry and is not queued a second time beside it.

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
	// seen marks messages handed to an agent before, in a turn that never
	// finished: the agent may have read them.
	seen map[domain.MessageID]bool

	// tmu guards the turns: each agent's turns handed over and not yet
	// ended (oldest first), its last ended one, whether it reports usage,
	// and what its last turn carried (what a retry sends again).
	tmu           sync.Mutex
	open          map[domain.AgentID][]*turnRec
	last          map[domain.AgentID]*turnRec
	reportsUsage  map[domain.AgentID]bool
	lastDelivered map[domain.AgentID][]domain.MessageID
}

func newTaskInbox() *taskInbox {
	return &taskInbox{
		pending:       map[domain.AgentID][]*domain.Message{},
		heldSince:     map[domain.AgentID]time.Time{},
		flushing:      map[domain.AgentID]bool{},
		seen:          map[domain.MessageID]bool{},
		open:          map[domain.AgentID][]*turnRec{},
		last:          map[domain.AgentID]*turnRec{},
		reportsUsage:  map[domain.AgentID]bool{},
		lastDelivered: map[domain.AgentID][]domain.MessageID{},
	}
}

// add puts m in agent's inbox unless it is there already.
func (ib *taskInbox) add(agent domain.AgentID, m *domain.Message) {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	for _, p := range ib.pending[agent] {
		if p.ID == m.ID {
			return
		}
	}
	ib.pending[agent] = append(ib.pending[agent], m)
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

// closeInbox stops holding messages for a task whose run has ended. It only
// releases memory: what was undelivered is queued in the store, and the next
// run hands it over. It used to be dropped here, so a reply that reached a
// busy agent was lost to a pause, and the agent told to carry on without it.
// A task that has ended needs none of it any more.
func (o *Orchestrator) closeInbox(id domain.TaskID) {
	o.mu.Lock()
	delete(o.inboxes, id)
	o.mu.Unlock()
	if t, err := o.Tasks.Get(context.Background(), id); err == nil && t.State.IsTerminal() {
		o.clearDeliveries(context.Background(), id)
	}
}

// durable reports whether the store keeps deliveries (see MessageBus.Holds).
func (o *Orchestrator) durable() bool { return o.storage.Deliveries != nil }

// clearDeliveries forgets what still waits for a task that has ended: it
// will never be handed over, and the table would otherwise grow with every
// finished task. What was acknowledged stays as the record of what each
// agent was given.
func (o *Orchestrator) clearDeliveries(ctx context.Context, id domain.TaskID) {
	if !o.durable() {
		return
	}
	if err := o.storage.Deliveries.Clear(ctx, id); err != nil {
		o.log.Warn("clear the deliveries of a finished task", "taskId", string(id), "error", err)
	}
}

// deferredKey marks a context whose routes are recorded and queued for the
// task's next run rather than handed to an inbox (deliverOrQueue).
type deferredKey struct{}

func deferToNextRun(ctx context.Context) context.Context {
	return context.WithValue(ctx, deferredKey{}, true)
}

func deferred(ctx context.Context) bool {
	b, _ := ctx.Value(deferredKey{}).(bool)
	return b
}

// holds is the bus's Holds hook: a message waits in an inbox, durably, when
// the task's run is up or it is queued for the next one.
func (o *Orchestrator) holds(ctx context.Context, t *domain.Task, _ domain.AgentID) bool {
	return deferred(ctx) || o.inbox(t.ID) != nil
}

// queuedRows reads what is queued for a task, first giving back to the queue
// what a turn of an earlier process carried and never finished: that process
// stopped mid-turn, so the turn is as good as killed.
func (o *Orchestrator) queuedRows(ctx context.Context, id domain.TaskID) []*domain.Delivery {
	if !o.durable() {
		return nil
	}
	rows, err := o.storage.Deliveries.ListByTask(ctx, id)
	if err != nil {
		o.log.Warn("read the queued deliveries", "taskId", string(id), "error", err)
		return nil
	}
	var queued []*domain.Delivery
	interrupted := map[domain.TurnID]bool{}
	for _, d := range rows {
		if d.Status == domain.DeliverySent {
			if !interrupted[d.TurnID] {
				interrupted[d.TurnID] = true
				if err := o.storage.Deliveries.SettleTurn(ctx, d.TurnID, domain.DeliveryQueued); err != nil {
					o.log.Warn("requeue an interrupted turn", "taskId", string(id), "error", err)
				}
			}
			d.Status = domain.DeliveryQueued
		}
		if d.Status == domain.DeliveryQueued {
			queued = append(queued, d)
		}
	}
	return queued
}

// humanQueued reports whether a message of the human's waits for the task's
// next run.
func (o *Orchestrator) humanQueued(ctx context.Context, id domain.TaskID) bool {
	for _, d := range o.queuedRows(ctx, id) {
		if m, err := o.storage.Messages.Get(ctx, d.MessageID); err == nil && m.FromAgent == domain.HumanAgentID {
			return true
		}
	}
	return false
}

// refillInbox puts what is queued (rows, from queuedRows) in the inboxes of
// the run that is starting: what waited in an inbox when the last run ended,
// the human's messages sent while no run was alive, and what was in a turn
// that never finished, which the agent may have seen. It reports whether any
// of it is from the human.
func (o *Orchestrator) refillInbox(ctx context.Context, t *domain.Task, rows []*domain.Delivery) (fromHuman bool) {
	ib := o.inbox(t.ID)
	if ib == nil {
		return false
	}
	for _, d := range rows {
		if t.IsDropped(d.Recipient) {
			o.discardInbox(t.ID, d.Recipient)
			continue
		}
		m, err := o.storage.Messages.Get(ctx, d.MessageID)
		if err != nil {
			o.log.Warn("refill an inbox", "taskId", string(t.ID), "messageId", string(d.MessageID), "error", err)
			continue
		}
		// As the bus hands it over: addressed to this recipient, with the
		// context it would have been given.
		m.ToAgent = d.Recipient
		if o.Bus.Enrich != nil {
			o.Bus.Enrich(ctx, t, m)
		}
		if m.FromAgent == domain.HumanAgentID {
			fromHuman = true
		}
		if d.TurnID != "" {
			ib.mu.Lock()
			ib.seen[m.ID] = true
			ib.mu.Unlock()
		}
		ib.add(d.Recipient, m)
	}
	return fromHuman
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

// discardInbox throws away what waits for agent: it was dropped from the
// task and will never be handed it.
func (o *Orchestrator) discardInbox(id domain.TaskID, agent domain.AgentID) {
	if o.durable() {
		if err := o.storage.Deliveries.Discard(context.Background(), id, agent); err != nil {
			o.log.Warn("discard a dropped agent's deliveries", "taskId", string(id), "agentId", string(agent), "error", err)
		}
	}
	ib := o.inbox(id)
	if ib == nil {
		return
	}
	ib.mu.Lock()
	delete(ib.pending, agent)
	delete(ib.heldSince, agent)
	ib.mu.Unlock()
}

// handoff is the bus's Handoff hook: it puts a message for a running task in
// the recipient's inbox and tries to hand the inbox over at once.
//
// A message routed for the next run (deferred) is already queued in the
// store, where that run finds it; it is not put in an inbox that may belong
// to a run still starting.
func (o *Orchestrator) handoff(ctx context.Context, t *domain.Task, m *domain.Message) bool {
	if deferred(ctx) {
		return true
	}
	ib := o.inbox(t.ID)
	if ib == nil {
		return false
	}
	ib.add(m.ToAgent, m)
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
	o.closeIdleTurns(ctx, id, ib)
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
	if t.IsDropped(agent) {
		// Taken off the task: what waits for it is never delivered.
		o.discardInbox(t.ID, agent)
		return
	}
	ad, ok := o.Agents.AdapterFor(t.ID, agent)
	if !ok || ad.Status() == domain.AgentStateWorking {
		return
	}
	// With the decision accepted and only the human's questions left, only
	// the agents that owe the human a reply take another turn.
	if t.Closing && !owesHuman(ledger, agent) {
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
		// The human never waits. A notice is not the human and waits like any
		// other message: it says nothing the agent needs before the answers it
		// is waiting for, and it is read together with them.
		if m.FromAgent == domain.HumanAgentID {
			return nil
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
		if ad, ok := o.Agents.AdapterFor(t.ID, y); ok && ad.Status() == domain.AgentStateWorking || o.isRetrying(t.ID, y) {
			busy = append(busy, y)
		}
	}
	// Only a co-worker still working -- or about to be tried again -- will
	// produce the answer; waiting on an idle one would wait for nothing (the
	// reminders deal with silence). The note that a co-worker is being
	// retried thus reaches the agent with the answer, in one turn.
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
	var carries [][]domain.MessageID
	if batch {
		deliveries = []*domain.Message{combine(msgs)}
		carries = [][]domain.MessageID{messageIDs(msgs)}
	} else {
		deliveries = msgs
		for _, m := range msgs {
			carries = append(carries, []domain.MessageID{m.ID})
		}
	}
	ib := o.inbox(t.ID)
	for i, d := range deliveries {
		if board != "" {
			if d.Metadata == nil {
				d.Metadata = map[string]string{}
			}
			d.Metadata[ports.TeamMetadataKey] = board
		}
		if ib != nil && ib.takeSeen(carries[i]) {
			// Handed over before in a turn that never finished: the agent
			// may have read it, and should not be puzzled to read it again.
			d.Content = redeliveryNote + "\n\n" + d.Content
		}
		// A send that fails leaves the messages queued in the store, for the
		// next run; they used to be lost with it.
		if err := o.sendTurn(ctx, t, agent, d, carries[i]); err != nil {
			o.log.Error("deliver message", "taskId", string(t.ID), "agentId", string(agent), "error", err)
			o.Events.emit(ctx, domain.EvtAgentFailed, t.ID, agent, fmt.Sprintf("delivery to %s failed: %v", agent, err), nil)
			return
		}
		// Kept as the turn a retry sends again should this one fail.
		o.noteDelivery(t.ID, agent, d)
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
		// A notice is the orchestrator's, never the human's. Tagged [HUMAN],
		// with "answer those first (to=human)" above it, a notice about an
		// agent's own unreadable reply had agents writing to the human about
		// their format mistakes.
		what, from := string(m.Type), string(m.FromAgent)
		switch {
		case domain.FromOrchestrator(m):
			what, from = "NOTICE", string(domain.OrchestratorID)
		case m.FromAgent == domain.HumanAgentID:
			from = "[HUMAN] human"
			env.Metadata[ports.BatchHumanMetadataKey] = "true"
		}
		fmt.Fprintf(&b, "=== %d of %d · %s from %s · %s · %s ===\n", i+1, len(msgs), what, from,
			m.Timestamp.Format("15:04:05"), m.ID)
		if note := strings.TrimSpace(m.Metadata[ports.ThreadMetadataKey]); note != "" {
			fmt.Fprintf(&b, "(This is part of a question from the human: %s. If you can answer it, answer the human directly.)\n", note)
		}
		b.WriteString(ports.QuoteFraming(strings.TrimSpace(m.Content)))
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

// teamBoard describes every participant for the agent being delivered to: the
// roster (who each one is, what it may do, its state and spending), then
// unread messages and who waits on and owes whom.
func (o *Orchestrator) teamBoard(t *domain.Task, ledger []domain.Request, ib *taskInbox, self domain.AgentID) string {
	now := time.Now()
	var b strings.Builder
	b.WriteString(o.roster(t, self))
	b.WriteString("\n")
	for _, p := range t.Participants {
		var parts []string
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
		if len(parts) > 0 {
			fmt.Fprintf(&b, "- %s: %s\n", p, strings.Join(parts, " · "))
		}
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

// redeliveryNote heads a delivery of messages an agent was handed before, in
// a turn that was cut off.
const redeliveryNote = "(Note from the orchestrator: delivered again -- you may have seen these before your last turn was cut off.)"

func messageIDs(msgs []*domain.Message) []domain.MessageID {
	ids := make([]domain.MessageID, len(msgs))
	for i, m := range msgs {
		ids[i] = m.ID
	}
	return ids
}

// takeSeen reports whether any of ids may have been seen before, and forgets
// it: it is said once.
func (ib *taskInbox) takeSeen(ids []domain.MessageID) bool {
	ib.mu.Lock()
	defer ib.mu.Unlock()
	seen := false
	for _, id := range ids {
		if ib.seen[id] {
			seen = true
			delete(ib.seen, id)
		}
	}
	return seen
}

// ---------------------------------------------------------------------------
// Turns
// ---------------------------------------------------------------------------
//
// A turn is one hand-over to an adapter. Adapters say nothing of turns: an
// agent's outbound is its messages, then a failure if the turn failed, then
// its usage -- each turn's after the one before, since an adapter runs one
// at a time. So a usage report ends the oldest open turn, a failure ends it
// failed, and an agent seen idle has ended every turn it had. A turn seen to
// end before its usage was read keeps a place for it (pending): the report
// is still on its way. Only an agent that reports usage at all keeps one,
// or every later report would be taken for the turn before.

// turnRec is one turn, from the hand-over until its outcome is known.
type turnRec struct {
	id      domain.TurnID
	agent   domain.AgentID
	msgs    []domain.MessageID
	started time.Time
	ended   time.Time
	outcome domain.TurnOutcome
	kind    domain.FailureKind
	usage   ports.Usage
	billed  bool
	pending bool
}

// sendTurn hands d, which carries the stored messages ids, to agent as one
// turn. The deliveries are marked sent only once the adapter has taken it:
// a send that fails leaves them queued.
func (o *Orchestrator) sendTurn(ctx context.Context, t *domain.Task, agent domain.AgentID, d *domain.Message, ids []domain.MessageID) error {
	send := func() error {
		if d.Type == domain.MsgTaskAssignment {
			return o.Agents.SendTask(ctx, t, *d)
		}
		return o.Agents.Send(ctx, t, *d)
	}
	ib := o.inbox(t.ID)
	if ib == nil {
		return send()
	}
	ib.tmu.Lock()
	defer ib.tmu.Unlock()
	rec := &turnRec{id: domain.NewTurnID(), agent: agent, msgs: ids, started: time.Now()}
	// Opened before the send, so a turn that ends at once still finds it;
	// tmu keeps its end waiting until it is marked sent.
	ib.open[agent] = append(ib.open[agent], rec)
	if err := send(); err != nil {
		ib.open[agent] = ib.open[agent][:len(ib.open[agent])-1]
		return err
	}
	ib.lastDelivered[agent] = ids
	if o.durable() && len(ids) > 0 {
		if err := o.storage.Deliveries.MarkSent(ctx, t.ID, agent, ids, rec.id, rec.started); err != nil {
			o.log.Warn("mark deliveries sent", "taskId", string(t.ID), "agentId", string(agent), "error", err)
		}
	}
	return nil
}

// lastDeliveredIDs is what agent's last turn carried: what a retry of it
// carries again.
func (o *Orchestrator) lastDeliveredIDs(id domain.TaskID, agent domain.AgentID) []domain.MessageID {
	ib := o.inbox(id)
	if ib == nil {
		return nil
	}
	ib.tmu.Lock()
	defer ib.tmu.Unlock()
	return ib.lastDelivered[agent]
}

// endTurn records how rec ended and settles what it carried. Called with
// ib.tmu held.
func (o *Orchestrator) endTurn(ctx context.Context, ib *taskInbox, id domain.TaskID, rec *turnRec, outcome domain.TurnOutcome, kind domain.FailureKind) {
	rec.ended, rec.outcome, rec.kind = time.Now(), outcome, kind
	rec.pending = !rec.billed && ib.reportsUsage[rec.agent]
	ib.last[rec.agent] = rec
	o.settle(ctx, id, rec)
}

// settle moves what rec carried on by how it ended: acknowledged when it
// completed, back in the queue when it was killed or failed. A malformed
// reply is the exception: the agent took its turn on the batch and has it in
// its session; it is asked to reply again, not handed the batch again.
//
// A failed turn the policy retries goes back to the queue only until the
// retry is sent, which carries the same messages again (sendTurn marks them
// sent with it): the in-memory inbox never gets them back, so the batch is
// not delivered a second time beside its retry. If the run ends before the
// retry is sent, they are where the next run finds them.
func (o *Orchestrator) settle(ctx context.Context, id domain.TaskID, rec *turnRec) {
	if !o.durable() || len(rec.msgs) == 0 {
		return
	}
	status := domain.DeliveryQueued
	if rec.outcome == domain.TurnCompleted || (rec.outcome == domain.TurnFailed && rec.kind == domain.FailureMalformed) {
		status = domain.DeliveryAcknowledged
	}
	if err := o.storage.Deliveries.SettleTurn(ctx, rec.id, status); err != nil {
		o.log.Warn("settle a turn's deliveries", "taskId", string(id), "turn", string(rec.id), "error", err)
	}
}

// popOpen takes agent's oldest open turn, or nil. Called with ib.tmu held.
func (ib *taskInbox) popOpen(agent domain.AgentID) *turnRec {
	q := ib.open[agent]
	if len(q) == 0 {
		return nil
	}
	if len(q) == 1 {
		delete(ib.open, agent)
	} else {
		ib.open[agent] = q[1:]
	}
	return q[0]
}

// turnUsage takes one usage report of agent's: it belongs to the turn that
// ended without its report, or else ends the oldest open turn.
func (o *Orchestrator) turnUsage(ctx context.Context, id domain.TaskID, agent domain.AgentID, u ports.Usage) {
	ib := o.inbox(id)
	if ib == nil {
		return
	}
	ib.tmu.Lock()
	defer ib.tmu.Unlock()
	ib.reportsUsage[agent] = true
	last := ib.last[agent]
	if last != nil && last.pending {
		last.pending = false
		addTurnUsage(last, u)
		return
	}
	if rec := ib.popOpen(agent); rec != nil {
		addTurnUsage(rec, u)
		o.endTurn(ctx, ib, id, rec, domain.TurnCompleted, "")
		return
	}
	if last != nil {
		addTurnUsage(last, u)
	}
}

func addTurnUsage(rec *turnRec, u ports.Usage) {
	rec.billed = true
	rec.usage.InputTokens += u.InputTokens
	rec.usage.OutputTokens += u.OutputTokens
	rec.usage.CacheReadTokens += u.CacheReadTokens
	rec.usage.CostUSD += u.CostUSD
}

// turnFailure takes a failed turn of agent's: the one already taken as done
// -- seen idle -- whose failure was still on its way, or else the oldest open
// one. Cancelled is a turn the orchestrator ended itself: killed.
func (o *Orchestrator) turnFailure(ctx context.Context, id domain.TaskID, agent domain.AgentID, kind domain.FailureKind) {
	ib := o.inbox(id)
	if ib == nil {
		return
	}
	outcome := domain.TurnFailed
	if kind == domain.FailureCancelled {
		outcome = domain.TurnKilled
	}
	ib.tmu.Lock()
	defer ib.tmu.Unlock()
	if last := ib.last[agent]; last != nil && last.pending && last.outcome == domain.TurnCompleted {
		last.outcome, last.kind = outcome, kind
		o.settle(ctx, id, last)
		return
	}
	if rec := ib.popOpen(agent); rec != nil {
		o.endTurn(ctx, ib, id, rec, outcome, kind)
	}
}

// closeIdleTurns ends the open turns of every agent no longer working: it
// has finished them, whether or not it said anything.
func (o *Orchestrator) closeIdleTurns(ctx context.Context, id domain.TaskID, ib *taskInbox) {
	ib.tmu.Lock()
	defer ib.tmu.Unlock()
	for agent := range ib.open {
		if ad, ok := o.Agents.AdapterFor(id, agent); ok && ad.Status() == domain.AgentStateWorking {
			continue
		}
		for rec := ib.popOpen(agent); rec != nil; rec = ib.popOpen(agent) {
			o.endTurn(ctx, ib, id, rec, domain.TurnCompleted, "")
		}
	}
}

// killTurns ends every open turn as the run stops: killed when its agent is
// still working -- what it carried goes back to the queue -- and completed
// when the agent had finished it.
func (o *Orchestrator) killTurns(ctx context.Context, id domain.TaskID) {
	ib := o.inbox(id)
	if ib == nil {
		return
	}
	ib.tmu.Lock()
	defer ib.tmu.Unlock()
	for agent := range ib.open {
		outcome := domain.TurnCompleted
		if ad, ok := o.Agents.AdapterFor(id, agent); ok && ad.Status() == domain.AgentStateWorking {
			outcome = domain.TurnKilled
		}
		for rec := ib.popOpen(agent); rec != nil; rec = ib.popOpen(agent) {
			o.endTurn(ctx, ib, id, rec, outcome, "")
		}
	}
}
