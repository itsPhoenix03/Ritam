package orchestrator

import (
	"fmt"
	"strings"

	"ritam/internal/domain"
)

// The team roster: one line per participant saying what it is and what it
// may do, rendered the same way in every delivery's team board.
//
// The authority used to delegate blind, given a list of ids and nothing else:
// not which model each one ran, which could write or run a build, or what it
// had cost. So it could not send routine reading to a cheap model, could not
// pick the agent that runs tests, and asked read-only agents to implement.

// rosterColumns are the roster's columns, in order.
var rosterColumns = []string{"id", "model", "role", "files", "commands", "state", "spent"}

// roster renders the participants of t for self as an aligned table. A
// column whose value is the same for everyone says nothing about who to ask,
// and is left out; the id column always stays.
func (o *Orchestrator) roster(t *domain.Task, self domain.AgentID) string {
	agents := map[domain.AgentID]domain.Agent{}
	for _, a := range o.Agents.Statuses() {
		agents[a.ID] = a
	}
	rights, _ := o.Agents.(RightsReporter)

	rows := make([][]string, 0, len(t.Participants))
	for _, p := range t.Participants {
		a := agents[p]
		id := string(p)
		if p == self {
			id += " (you)"
		}
		files, commands := "?", "?"
		if rights != nil {
			if r, known := rights.AgentRights(t, p); known {
				files, commands = "read-only", "no"
				if r.Writes {
					files = "write"
				}
				if r.Builds {
					commands = "build/test"
				}
			}
		}
		rows = append(rows, []string{id, modelOf(a), roleOf(t, a, p), files, commands, o.stateOf(t, p), spentOf(t, p)})
	}

	keep := []int{0}
	for c := 1; c < len(rosterColumns); c++ {
		for _, r := range rows[1:] {
			if r[c] != rows[0][c] {
				keep = append(keep, c)
				break
			}
		}
	}
	width := make([]int, len(rosterColumns))
	for c := range rosterColumns {
		width[c] = len(rosterColumns[c])
		for _, r := range rows {
			width[c] = max(width[c], len(r[c]))
		}
	}
	var b strings.Builder
	line := func(cells []string) {
		var out []string
		for i, c := range keep {
			if i == len(keep)-1 {
				out = append(out, cells[c])
			} else {
				out = append(out, fmt.Sprintf("%-*s", width[c], cells[c]))
			}
		}
		b.WriteString(strings.TrimRight(strings.Join(out, "  "), " "))
		b.WriteString("\n")
	}
	line(rosterColumns)
	for _, r := range rows {
		line(r)
	}
	return strings.TrimRight(b.String(), "\n")
}

// modelOf names an agent's provider and model ("claude opus").
func modelOf(a domain.Agent) string {
	model := a.Model
	if model == "" {
		model = "default"
	}
	switch a.Kind {
	case "":
		return model
	case domain.AgentKindClaudeCode:
		return "claude " + model
	}
	return string(a.Kind) + " " + model
}

// roleOf is the agent's role in this task, with the role it was recruited
// for where that says something: implementer is every agent's default.
func roleOf(t *domain.Task, a domain.Agent, p domain.AgentID) string {
	role := "supporting"
	if p == t.AuthorityAgent {
		role = "authority"
	}
	if a.Role == domain.RoleReviewer || a.Role == domain.RoleObserver {
		role += ", " + strings.ToLower(string(a.Role))
	}
	return role
}

// stateOf is what an agent is doing on the task now.
func (o *Orchestrator) stateOf(t *domain.Task, p domain.AgentID) string {
	if t.IsDropped(p) {
		return "dropped"
	}
	if ad, ok := o.Agents.AdapterFor(t.ID, p); ok {
		return strings.ToLower(strings.TrimPrefix(string(ad.Status()), "AGENT_"))
	}
	return "stopped"
}

// spentOf is what an agent's turns on the task have cost: "-" before its
// first turn, "n/a" when its CLI reports no cost.
func spentOf(t *domain.Task, p domain.AgentID) string {
	u := t.AgentUsage[p]
	switch {
	case u.CostUSD > 0:
		return fmt.Sprintf("$%.2f", u.CostUSD)
	case u.Turns > 0:
		return "n/a"
	}
	return "-"
}

// rosterGuidance is the authority's advice on using the roster, for its
// opening brief.
func (o *Orchestrator) rosterGuidance(t *domain.Task) string {
	s := "Send routine reading and searching to the cheapest capable agent."
	rights, ok := o.Agents.(RightsReporter)
	if !ok {
		return s
	}
	known, builders := false, 0
	for _, p := range t.Participants {
		if r, k := rights.AgentRights(t, p); k {
			known = true
			if r.Builds {
				builders++
			}
		}
	}
	switch {
	case !known:
	case builders == 0:
		s += " No agent can run commands: ask the human for build and test output."
	default:
		s += " Only agents marked build/test can run commands: ask one of them, or the human, for test output."
	}
	return s
}
