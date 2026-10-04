package tui

import (
	"fmt"
	"time"

	"projectorch/internal/domain"
)

// The request ledger as the TUI shows it. The orchestrator and the TUI read
// the same domain.BuildLedger over the same messages, so the screen can never
// disagree with what the run loop believes is outstanding.

// ledger returns the current task's request ledger.
func (m Model) ledger() []domain.Request {
	return domain.BuildLedger(m.messages)
}

// requestLines renders the open requests as indented threads.
func (m Model) requestLines(inner int) []line {
	open := domain.OpenRequests(m.ledger())
	if len(open) == 0 {
		return nil
	}
	idents := m.identities()
	out := []line{blank(), {st("WAITING ON", dimStyle)}}
	depth := map[domain.MessageID]int{}
	for _, r := range open {
		d := 0
		if r.Parent != "" {
			d = depth[r.Parent] + 1
		}
		depth[r.Message.ID] = d
		indent := ""
		for i := 0; i < d; i++ {
			indent += "  "
		}
		l := line{txt(" " + indent)}
		if d > 0 {
			l = l.join(st(gl().arrow+" ", dimStyle))
		}
		l = l.join(agentCell(idents, r.Message.FromAgent), st(" "+gl().arrow+" ", dimStyle),
			agentCell(idents, r.Message.ToAgent))
		age := line{st(fmt.Sprintf("%s %s", r.Message.Type, shortDur(r.Age(time.Now()))), dimStyle)}
		out = append(out, spread(l, age, inner))
	}
	return out
}

// agentCell names an agent in its identity colour; the human is "you".
func agentCell(idents []agentIdent, id domain.AgentID) cell {
	if id == domain.HumanAgentID {
		return st("you", emphStyle)
	}
	if a, ok := identFor(idents, id); ok {
		return st(string(id), a.style())
	}
	return st(string(id), bodyStyle)
}

// requestLabel names the other party of a request; the human is "you".
func requestLabel(id domain.AgentID) string {
	if id == domain.HumanAgentID {
		return "you"
	}
	return string(id)
}

// agentRequests splits the open requests into those agent is waiting on and
// those it owes a reply to.
func (m Model) agentRequests(agent domain.AgentID) (waiting, owes []domain.Request) {
	for _, r := range domain.OpenRequests(m.ledger()) {
		switch agent {
		case r.Message.FromAgent:
			waiting = append(waiting, r)
		case r.Message.ToAgent:
			owes = append(owes, r)
		}
	}
	return waiting, owes
}
