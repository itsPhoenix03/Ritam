package domain

import "time"

// The request ledger: who is waiting on whom, and for what.
//
// A message that asks for something (a QUESTION, a DELEGATE, a human's
// instruction) opens a request; the recipient replying to the asker closes
// it. Requests chain into threads: when a human asks scout something and scout
// asks codex to check part of it, codex's request belongs to the human's
// thread, and the thread is only finished when scout reports back.
//
// The ledger is derived from the message history every time it is needed,
// never stored. It therefore cannot drift out of step with what was actually
// said, and needs no migration when the rules for reading it change.

// Request is one message that asked for a reply, and what became of it.
type Request struct {
	Message *Message
	// Thread is the id of the request at the root of the chain this one
	// belongs to -- its own id when nobody was waiting on the asker.
	Thread MessageID
	// Parent is the request the asker was serving when it asked, if any.
	Parent MessageID
	// Answer is the reply that closed the request, nil while it is open.
	Answer *Message
	// Abandoned marks a request the orchestrator closed because the agent
	// owing the reply stayed silent after a reminder. Answer is then the
	// orchestrator's closing note, not a reply.
	Abandoned bool
}

// MetaCloses is the metadata key on the orchestrator's closing note naming the
// request it closes as unanswered.
const MetaCloses = "closes"

// Open reports whether the request is still waiting for a reply.
func (r Request) Open() bool { return r.Answer == nil }

// Age reports how long the request has been (or was) open.
func (r Request) Age(now time.Time) time.Duration {
	end := now
	if r.Answer != nil {
		end = r.Answer.Timestamp
	}
	return end.Sub(r.Message.Timestamp)
}

// opensRequest reports whether m asks someone for a reply.
//
// Broadcasts are left out: "everyone, please look" has no single party whose
// silence is a stall. So is the opening TASK_ASSIGNMENT, which the authority
// answers by working rather than by replying to the human, and every note of
// the orchestrator's (BuildLedger skips those before asking).
func opensRequest(m *Message) bool {
	if m.ToAgent == BroadcastAgentID || m.ToAgent == "" || m.ToAgent == OrchestratorID || m.FromAgent == m.ToAgent {
		return false
	}
	switch m.Type {
	case MsgTaskAssignment, MsgError, MsgFinalDecision:
		return false
	}
	if m.FromAgent == HumanAgentID {
		return m.RequiresResponse
	}
	return m.RequiresResponse || m.Type.ExpectsResponse()
}

// answers reports whether m, a message from r's addressee that reaches r's
// asker, is the reply r was waiting for. Most messages are: agents do not
// pick their reply's type reliably, so a strict table of which type closes
// which request would leave requests open for ever. Two are not:
//
//   - A QUESTION back asks about the request; it does not answer it. Codex
//     asking opus to clarify a DELEGATE closed the DELEGATE, so nothing
//     reminded codex of it, a resume did not ask for it again, and the
//     follow-up it belonged to could complete with the work undone.
//   - A STATUS_UPDATE ("working on it") does not finish work an agent asked
//     for (DELEGATE, IMPLEMENT, REQUEST_CHANGE). It still answers a QUESTION,
//     and still answers the human, to whom agents commonly report "done: ..."
//     as a status update; leaving that open would bring back reminders the
//     human did not need.
func answers(r *Request, m *Message) bool {
	switch m.Type {
	case MsgQuestion:
		return false
	case MsgStatusUpdate:
		if r.Message.FromAgent == HumanAgentID {
			return true
		}
		switch r.Message.Type {
		case MsgDelegate, MsgImplement, MsgRequestChange:
			return false
		}
	}
	return true
}

// BuildLedger reads the requests out of a task's message history, oldest
// first.
//
// A reply from A to B closes every request B has open with A -- agents
// routinely answer several questions in one message -- unless it names the
// exact request it answers through InReplyTo. A broadcast from A counts as a
// reply to everyone, the asker included. A QUESTION back, and a status
// update on work, are not replies (see answers).
//
// The orchestrator's notes are not part of the conversation: they neither
// answer a request nor open one. Only the human answers a question put to
// the human; a checkpoint arriving first used to count as the answer, and a
// decision that should have waited for the human's reply went through.
func BuildLedger(history []*Message) []Request {
	var reqs []Request
	index := map[MessageID]int{}
	// children maps a request to the requests made in service of it, so an
	// answer can be traced back up the chain that led to the agent giving it.
	children := map[MessageID][]int{}

	// heldBy returns the oldest open request addressed to agent, which is the
	// one a new request from that agent is most likely in service of.
	heldBy := func(agent AgentID) (int, bool) {
		for i := range reqs {
			if reqs[i].Open() && reqs[i].Message.ToAgent == agent {
				return i, true
			}
		}
		return -1, false
	}

	for _, m := range history {
		if m == nil {
			continue
		}
		// The orchestrator's note closing a request nobody answered. It is
		// nothing else: not an answer from its sender, not a request.
		if id := MessageID(m.Metadata[MetaCloses]); id != "" {
			if i, ok := index[id]; ok && reqs[i].Open() {
				reqs[i].Answer = m
				reqs[i].Abandoned = true
			}
			continue
		}
		if FromOrchestrator(m) {
			continue
		}
		// First: does m answer something?
		// InReplyTo pins the answer to one request -- but only when it names a
		// request at all. Pointing at an ordinary message (an answer, a
		// reminder) says nothing about which request is answered, so the usual
		// matching applies. And it says which request m responds to, not that
		// it answers it: an agent passing the human's question on to another
		// agent replies "in reply to" it, but to someone else. Only a reply
		// that reaches the asker answers.
		// A message to everyone answers everyone who asked its sender, linked
		// or not.
		i, pinned := index[m.InReplyTo]
		if m.InReplyTo != "" && pinned {
			if reqs[i].Open() && reqs[i].Message.ToAgent == m.FromAgent &&
				(m.ToAgent == reqs[i].Message.FromAgent || m.ToAgent == BroadcastAgentID) && answers(&reqs[i], m) {
				reqs[i].Answer = m
			}
		}
		if m.InReplyTo == "" || !pinned || m.ToAgent == BroadcastAgentID {
			for i := range reqs {
				r := &reqs[i]
				if !r.Open() || r.Message.ToAgent != m.FromAgent {
					continue
				}
				if (m.ToAgent == r.Message.FromAgent || m.ToAgent == BroadcastAgentID) && answers(r, m) {
					r.Answer = m
				}
			}
		}

		// Answered along the chain: a reply to whoever started a chain closes
		// every request on the path to the agent replying, however far down
		// it sits. When the human asks codex, codex asks opus, and opus answers
		// the human directly -- the path agents should prefer -- the human's
		// question and codex's question to opus are both answered. Requests on
		// other branches are untouched: those agents still owe their part.
		if m.ToAgent != BroadcastAgentID {
			for i := range reqs {
				r := &reqs[i]
				if !r.Open() || r.Message.FromAgent != m.ToAgent || r.Message.ToAgent == m.FromAgent {
					continue
				}
				if path, ok := chainTo(reqs, children, i, m.FromAgent); ok {
					if answers(r, m) {
						r.Answer = m
					}
					for _, j := range path {
						if reqs[j].Open() && answers(&reqs[j], m) {
							reqs[j].Answer = m
						}
					}
				}
			}
		}

		// Then: does m ask something?
		if !opensRequest(m) {
			continue
		}
		r := Request{Message: m, Thread: m.ID}
		if p, ok := heldBy(m.FromAgent); ok {
			r.Parent = reqs[p].Message.ID
			r.Thread = reqs[p].Thread
		}
		index[m.ID] = len(reqs)
		if r.Parent != "" {
			children[r.Parent] = append(children[r.Parent], len(reqs))
		}
		reqs = append(reqs, r)
	}
	return reqs
}

// chainTo finds the requests leading down from reqs[i] to one addressed to
// agent, returning their indices top-down.
func chainTo(reqs []Request, children map[MessageID][]int, i int, agent AgentID) ([]int, bool) {
	for _, c := range children[reqs[i].Message.ID] {
		if reqs[c].Message.ToAgent == agent {
			return []int{c}, true
		}
		if rest, ok := chainTo(reqs, children, c, agent); ok {
			return append([]int{c}, rest...), true
		}
	}
	return nil, false
}

// OpenRequests returns the requests still waiting for a reply.
func OpenRequests(ledger []Request) []Request {
	var out []Request
	for _, r := range ledger {
		if r.Open() {
			out = append(out, r)
		}
	}
	return out
}
