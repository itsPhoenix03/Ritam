package tui

import (
	"time"

	"ritam/internal/domain"
)

// The conversation lists what agents have said, so an agent that has been
// handed work but not yet written anything would look idle. While it works,
// it has one "thinking" entry in the conversation, at the time its turn
// began, drawn like any other entry; the message it posts replaces it.

// liveKey keys liveSince: an agent's instance on one task.
func liveKey(task domain.TaskID, agent domain.AgentID) string {
	return string(task) + "/" + string(agent)
}

// trackLive records when each agent on the current task started working, so
// its thinking entry sits where its turn began. It is called with each
// fresh agent list.
func (m Model) trackLive(agents []domain.Agent) Model {
	if m.liveSince == nil {
		m.liveSince = map[string]time.Time{}
	}
	now := time.Now()
	for _, a := range agents {
		k := liveKey(m.currentTaskID, a.ID)
		switch _, seen := m.liveSince[k]; {
		case agentIsLive(a.State) && !seen:
			m.liveSince[k] = now
		case !agentIsLive(a.State) && seen:
			delete(m.liveSince, k)
		}
	}
	return m
}

// workingItems are the thinking entries: one per participant of the
// selected task that is working and has posted nothing since its turn
// began.
func (m Model) workingItems() []convItem {
	t := m.currentTask()
	if t == nil || t.State.IsTerminal() {
		return nil
	}
	var out []convItem
	for _, a := range m.agents {
		if !agentIsLive(a.State) || !(a.ID == t.AuthorityAgent || containsAgent(t.Participants, a.ID)) {
			continue
		}
		since, ok := m.liveSince[liveKey(m.currentTaskID, a.ID)]
		if !ok {
			since = m.lastSentTo(a.ID, t)
		}
		if m.postedSince(a.ID, since) {
			continue
		}
		out = append(out, convItem{at: since, working: a.ID})
	}
	return out
}

// lastSentTo is when agent was last sent something, the best guess at when
// a turn began that the TUI did not see start; the task's last update when
// nothing was.
func (m Model) lastSentTo(agent domain.AgentID, t *domain.Task) time.Time {
	for i := len(m.messages) - 1; i >= 0; i-- {
		if msg := m.messages[i]; msg != nil && (msg.ToAgent == agent || msg.ToAgent == domain.BroadcastAgentID) {
			return msg.Timestamp
		}
	}
	return t.UpdatedAt
}

// postedSince reports whether agent has posted a message at or after t.
func (m Model) postedSince(agent domain.AgentID, t time.Time) bool {
	for i := len(m.messages) - 1; i >= 0; i-- {
		msg := m.messages[i]
		if msg == nil {
			continue
		}
		if msg.Timestamp.Before(t) {
			return false
		}
		if msg.FromAgent == agent {
			return true
		}
	}
	return false
}
