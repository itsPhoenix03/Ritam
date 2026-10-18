package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The ":" command line.
//
// Single-key bindings covered the original action set, but several of the
// things a human needs to do mid-run take an argument -- instruct *which*
// agent, filter to *which* agent, export to *which* path -- and binding a
// key per verb would exhaust the keyboard long before it covered them. The
// command line is the escape hatch, with completion so it is discoverable
// rather than something you have to already know.
//
// Every command here maps onto a ports.Service call or a local view change.
// Nothing in this file reaches past that interface.

// command is one entry in the palette.
type command struct {
	name string
	// usage is the completion-menu label, including argument shape.
	usage string
	help  string
	// argHint is what tab-completion offers after the verb: "agent" expands
	// to the registered agent ids.
	argHint string
}

// commands is the palette, in the order it is offered.
var commands = []command{
	{name: "instruct", usage: `instruct <agent> "..."`, help: "inject a human turn into the round", argHint: "agent"},
	{name: "pause", usage: "pause [reason]", help: "stop issuing turns, keep the processes"},
	{name: "resume", usage: "resume", help: "continue from the current round"},
	{name: "cancel", usage: "cancel [reason]", help: "end the task, terminate child CLIs"},
	{name: "approve", usage: "approve [note]", help: "accept the pending FINAL_DECISION"},
	{name: "reject", usage: "reject [note]", help: "refuse the pending FINAL_DECISION"},
	{name: "task", usage: `task new "..."`, help: "create and start another task"},
	{name: "filter", usage: "filter <agent|all>", help: "restrict the stream to one agent"},
	{name: "follow", usage: "follow <on|off>", help: "pin the stream to its newest line"},
	{name: "thinking", usage: "thinking <on|off>", help: "show or hide agent reasoning"},
	{name: "tab", usage: "tab <name>", help: "switch the centre column"},
	{name: "inspect", usage: "inspect <agent>", help: "point the inspector at an agent", argHint: "agent"},
	{name: "authority", usage: "authority <agent>", help: "hand the task's authority to another participant", argHint: "agent"},
	{name: "models", usage: "models", help: "list every model you can recruit (the team tab)"},
	{name: "recruit", usage: "recruit <provider> <model> [name] [role] [writes]", help: "add an agent running a model to the team"},
	{name: "dismiss", usage: "dismiss <agent>", help: "remove a recruited agent from the team", argHint: "agent"},
	{name: "check", usage: "check [provider model]", help: "check a model runs on your account (one short paid call)"},
	{name: "export", usage: "export jsonl [path]", help: "dump the event log seen by this UI"},
	{name: "help", usage: "help", help: "show the key and command reference"},
	{name: "quit", usage: "quit", help: "leave the TUI (agents keep running)"},
}

// matchCommands returns the palette entries matching what has been typed so
// far. Once a complete verb plus a space has been entered the menu collapses
// to that one entry, so the list stops competing with the argument.
func matchCommands(input string) []command {
	verb := input
	if i := strings.IndexByte(input, ' '); i >= 0 {
		verb = input[:i]
		for _, c := range commands {
			if c.name == verb {
				return []command{c}
			}
		}
		return nil
	}
	var out []command
	for _, c := range commands {
		if strings.HasPrefix(c.name, strings.ToLower(verb)) {
			out = append(out, c)
		}
	}
	return out
}

// completeCommand expands the command line one tab-press worth: the verb
// first, then an agent id where the command takes one.
func (m Model) completeCommand() Model {
	input := m.cmd.input
	if i := strings.IndexByte(input, ' '); i < 0 {
		matches := matchCommands(input)
		if len(matches) == 0 {
			return m
		}
		sel := m.cmd.selected
		if sel < 0 || sel >= len(matches) {
			sel = 0
		}
		m.cmd.input = matches[sel].name + " "
		m.cmd.selected = 0
		return m
	}

	verb, rest := splitVerb(input)
	var spec command
	for _, c := range commands {
		if c.name == verb {
			spec = c
		}
	}
	if spec.argHint != "agent" || strings.Contains(strings.TrimSpace(rest), " ") {
		return m
	}
	for _, a := range m.agents {
		if strings.HasPrefix(string(a.ID), strings.TrimSpace(rest)) {
			m.cmd.input = verb + " " + string(a.ID) + " "
			return m
		}
	}
	return m
}

// splitVerb splits "instruct codex do the thing" into ("instruct", "codex do
// the thing").
func splitVerb(s string) (verb, rest string) {
	s = strings.TrimLeft(s, " ")
	if i := strings.IndexByte(s, ' '); i >= 0 {
		return s[:i], strings.TrimLeft(s[i+1:], " ")
	}
	return s, ""
}

// unquote strips one layer of surrounding double quotes, which is how the
// palette's usage strings tell the human to pass multi-word text.
func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// runCommand executes one command line.
//
// Unknown verbs and bad arguments set the status line rather than failing
// silently: a command that appears to do nothing is indistinguishable from a
// command that did something invisible.
func (m Model) runCommand(input string) (Model, tea.Cmd) {
	input = strings.TrimSpace(input)
	if input == "" {
		return m, nil
	}
	m.cmd.history = append(m.cmd.history, input)
	m.cmd.histIdx = len(m.cmd.history)

	verb, rest := splitVerb(input)
	t := m.currentTask()
	needTask := func() bool {
		if t == nil {
			m.setStatus("no task selected")
			return false
		}
		return true
	}

	switch verb {
	case "instruct":
		if !needTask() {
			return m, nil
		}
		to, text := splitVerb(rest)
		if to == "" {
			m.setStatus(`usage: instruct <agent> "..."`)
			return m, nil
		}
		target := domain.AgentID(to)
		if !t.HasParticipant(target) {
			m.setStatus("%s is not a participant of %s", to, t.ID)
			return m, nil
		}
		if unquote(text) == "" {
			// No content typed: fall through to the interactive prompt with
			// the recipient already chosen, rather than sending an empty turn.
			m.mode = modePrompt
			m.prompt = promptState{
				action: ActionSendInstruction, label: "instruction",
				taskID: t.ID, participants: []domain.AgentID{target},
			}
			return m, nil
		}
		return m.instruct(t.ID, target, unquote(text))

	case "pause":
		if !needTask() {
			return m, nil
		}
		reason := unquote(rest)
		if reason == "" {
			reason = domain.PauseReasonHuman
		}
		m.setStatus("pausing %s...", t.ID)
		return m, pauseCmd(m.ctx, m.svc, t.ID, reason)

	case "resume":
		if !needTask() {
			return m, nil
		}
		m.setStatus("resuming %s...", t.ID)
		return m, resumeCmd(m.ctx, m.svc, t.ID)

	case "cancel":
		if !needTask() {
			return m, nil
		}
		m.setStatus("cancelling %s...", t.ID)
		return m, cancelCmd(m.ctx, m.svc, t.ID, unquote(rest))

	case "approve":
		if !needTask() {
			return m, nil
		}
		m.setStatus("approving %s...", t.ID)
		return m, approveCmd(m.ctx, m.svc, t.ID, unquote(rest))

	case "reject":
		if !needTask() {
			return m, nil
		}
		m.setStatus("rejecting %s...", t.ID)
		return m, rejectCmd(m.ctx, m.svc, t.ID, unquote(rest))

	case "task":
		sub, title := splitVerb(rest)
		if sub != "new" || unquote(title) == "" {
			m.setStatus(`usage: task new "title"`)
			return m, nil
		}
		if len(m.agents) < 2 {
			m.setStatus("need at least two agents to start a task")
			return m, nil
		}
		// Participants default to everyone. Narrowing them is a decision the
		// form exists to make; a one-line command should not make it silently.
		authority := m.agents[0].ID
		if t != nil && t.AuthorityAgent != "" {
			authority = t.AuthorityAgent
		}
		var participants []domain.AgentID
		for _, a := range m.agents {
			participants = append(participants, a.ID)
		}
		m.setStatus("creating task...")
		return m, createTaskCmd(m.ctx, m.svc, ports.CreateTaskInput{
			Title: unquote(title), Authority: authority, Participants: participants,
		})

	case "filter":
		arg := strings.TrimSpace(rest)
		arg = strings.TrimPrefix(arg, "agent=")
		if arg == "" || arg == "all" || arg == "*" {
			m.streamFilter = ""
			m.setStatus("stream filter cleared")
			return m, nil
		}
		if _, ok := m.agentByID(domain.AgentID(arg)); !ok {
			m.setStatus("no such agent: %s", arg)
			return m, nil
		}
		m.streamFilter = domain.AgentID(arg)
		m.setStatus("stream filtered to %s", arg)
		return m, nil

	case "follow":
		m.follow = parseToggle(rest, !m.follow)
		m.setStatus("follow %s", onOff(m.follow))
		return m, nil

	case "thinking":
		m.showThinking = parseToggle(rest, !m.showThinking)
		m.setStatus("thinking %s", onOff(m.showThinking))
		return m, nil

	case "tab":
		name := strings.TrimSpace(rest)
		for _, tb := range m.centerTabs() {
			if tb.String() == name {
				m.center, m.activePane, m.zoom = tb, paneConversation, false
				return m, nil
			}
		}
		m.setStatus("no such tab: %q", name)
		return m, nil

	case "authority":
		if !needTask() {
			return m, nil
		}
		to := domain.AgentID(strings.TrimSpace(rest))
		if to == "" {
			m.setStatus("usage: authority <agent>")
			return m, nil
		}
		if !t.HasParticipant(to) {
			m.setStatus("%s is not a participant of %s", to, t.ID)
			return m, nil
		}
		if to == t.AuthorityAgent {
			m.setStatus("%s is already the authority", to)
			return m, nil
		}
		m.setStatus("making %s the authority of %s...", to, t.ID)
		return m, setAuthorityCmd(m.ctx, m.svc, t.ID, to)

	case "models":
		m.center, m.activePane, m.zoom = tabTeam, paneConversation, false
		if m.modelsErr != nil {
			m.models, m.modelsErr = nil, nil // ask again
		}
		return m, nil

	case "recruit":
		in, err := m.parseRecruit(rest)
		if err != nil {
			m.setStatus("%v", err)
			return m, nil
		}
		m.setStatus("recruiting %s...", in.Name)
		return m, recruitCmd(m.ctx, m.svc, in)

	case "check":
		f := strings.Fields(rest)
		if len(f) >= 2 {
			p := f[0]
			switch strings.ToLower(p) {
			case "claude":
				p = "claude-code"
			case "openai":
				p = "codex"
			}
			return m.checking(ports.ModelInfo{Provider: p, ID: f[1]})
		}
		if mi, ok := m.selectedModel(); ok && m.centerOrDefault() == tabTeam {
			return m.checking(mi)
		}
		m.setStatus("usage: check <provider> <model>, or select a model in the team tab")
		return m, nil

	case "dismiss":
		id := domain.AgentID(strings.TrimSpace(rest))
		if id == "" {
			m.setStatus("usage: dismiss <agent>")
			return m, nil
		}
		m.setStatus("dismissing %s...", id)
		return m, dismissCmd(m.ctx, m.svc, id)

	case "inspect":
		arg := strings.TrimSpace(rest)
		for i, id := range m.rosterIdents() {
			if string(id.agent.ID) == arg {
				m.agentCursor = i
				m = m.inspectNow(inspectAgent)
				m.setStatus("inspecting %s", arg)
				return m, nil
			}
		}
		m.setStatus("no such agent: %s", arg)
		return m, nil

	case "export":
		format, path := splitVerb(rest)
		if format != "jsonl" && format != "" {
			m.setStatus("only jsonl is supported")
			return m, nil
		}
		path = strings.TrimSpace(unquote(path))
		if path == "" {
			path = "ritam-events.jsonl"
		}
		m.setStatus("exporting to %s...", path)
		return m, exportEventsCmd(path, m.globalEvents)

	case "help":
		m.mode = modeHelp
		return m, nil

	case "quit":
		m.quitting = true
		return m, tea.Quit
	}

	m.setStatus("unknown command: %s", verb)
	return m, nil
}

// parseToggle reads "on"/"off"/"true"/"false", defaulting to def (which
// callers pass as "the opposite of the current value", so a bare verb flips).
func parseToggle(s string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "true", "yes", "1":
		return true
	case "off", "false", "no", "0":
		return false
	default:
		return def
	}
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

// exportedEvent is the on-disk shape of one exported event. It is defined
// here rather than reusing domain.Event so the export format is a deliberate,
// stable contract instead of whatever the internal struct happens to look
// like today.
type exportedEvent struct {
	Time    time.Time         `json:"time"`
	Type    string            `json:"type"`
	TaskID  string            `json:"task_id,omitempty"`
	AgentID string            `json:"agent_id,omitempty"`
	Summary string            `json:"summary,omitempty"`
	Details map[string]string `json:"details,omitempty"`
}

// exportEventsCmd writes the event log this UI has seen to a JSONL file.
//
// It exports the TUI's own bounded buffer, not the durable log, and says so in
// the status line: the buffer is capped at maxGlobalEvents, so a long run's
// oldest events are genuinely not here to export.
func exportEventsCmd(path string, events []domain.Event) tea.Cmd {
	snapshot := append([]domain.Event(nil), events...)
	return func() tea.Msg {
		abs, err := filepath.Abs(path)
		if err != nil {
			return exportDoneMsg{err: err}
		}
		f, err := os.Create(abs)
		if err != nil {
			return exportDoneMsg{err: err}
		}
		defer f.Close()

		enc := json.NewEncoder(f)
		for _, e := range snapshot {
			if err := enc.Encode(exportedEvent{
				Time: e.Timestamp, Type: string(e.Type), TaskID: string(e.TaskID),
				AgentID: string(e.AgentID), Summary: e.Summary, Details: e.Details,
			}); err != nil {
				return exportDoneMsg{err: err}
			}
		}
		return exportDoneMsg{path: abs, count: len(snapshot)}
	}
}

// exportDoneMsg reports the result of an export.
type exportDoneMsg struct {
	path  string
	count int
	err   error
}

// String is here so a failed export reads the same way in the status line as
// any other error.
func (e exportDoneMsg) String() string {
	if e.err != nil {
		return "export failed: " + e.err.Error()
	}
	return fmt.Sprintf("exported %d events (this session's buffer) to %s", e.count, e.path)
}
