// Package tui implements the human-facing terminal UI for the orchestrator.
//
// The package is a pure presentation layer: it renders state obtained from
// ports.Service and turns key presses into calls on that same interface. It
// must never import the domain's mutating internals directly, never talk
// HTTP, and never touch storage. Every state-changing action goes:
//
//	keypress -> Action -> tea.Cmd -> ports.Service method -> result tea.Msg -> Model
//
// so that the only goroutine ever mutating the Bubble Tea Model is the
// Bubble Tea event loop itself (commands run off-goroutine but communicate
// back only via messages, per the Elm architecture Bubble Tea implements).
package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Options configures a TUI run.
type Options struct {
	// DemoOnStart immediately creates the built-in demo task so the screen
	// has something to show without requiring the human to fill in the
	// creation form first.
	DemoOnStart bool
	// DefaultAuthority pre-selects the authority agent in the task-creation
	// form and is used as the authority for the demo task.
	DefaultAuthority domain.AgentID
	// WorkspaceNotice explains a workspace restriction (orch launched at a
	// global location), shown in the header until the human acts on it.
	WorkspaceNotice string
	// Notices are things to know at start: a restore that is possible, a
	// deleted project's backups, folders renamed after a name change.
	Notices []string
}

// mode selects which top-level interaction surface is active. Only one is
// ever active at a time; the view and the key router both switch on it.
type mode int

const (
	modeNormal mode = iota
	modeHelp
	modeCreateForm
	modePrompt
	// modeCommand is the ":" command line. It exists because the action
	// surface has outgrown single letters: "instruct codex …" needs an
	// argument, "filter agent=x" needs a value, and binding a key to each
	// would exhaust the keyboard long before it covered the verbs.
	modeCommand
)

// centerTab selects what the centre column shows.
//
// The conversation is the default: the messages the agents exchanged, one
// compact entry each, with the selected one shown in full in the inspector.
// activity is the agents' raw live output, events the audit log, decisions
// the durable decision record. The last three tabs exist only when the
// screen is too narrow for the column that normally holds them.
type centerTab int

const (
	tabConversation centerTab = iota
	tabActivity
	tabEvents
	tabDecisions
	tabTasks
	tabAgents
	tabInspector
	// tabTeam lists the team and the models to recruit from (team.go).
	tabTeam
)

// String names the tab for the tab bar and the :tab command.
func (t centerTab) String() string {
	switch t {
	case tabActivity:
		return "activity"
	case tabEvents:
		return "events"
	case tabDecisions:
		return "decisions"
	case tabTasks:
		return "tasks"
	case tabAgents:
		return "agents"
	case tabInspector:
		return "inspector"
	case tabTeam:
		return "team"
	default:
		return "conversation"
	}
}

// inspectSubject is what the inspector is showing. It follows the last list
// the human moved in: the conversation, the agents, the tasks, or the
// decisions.
type inspectSubject int

const (
	inspectMessage inspectSubject = iota
	inspectAgent
	inspectTask
	inspectDecision
)

// layout is the responsive mode chosen from the terminal width.
type layout int

const (
	// layoutWide shows all three columns: tasks, centre, and the right-hand
	// column (agents, decision, inspector).
	layoutWide layout = iota
	// layoutMedium drops the task column; the tasks become a centre tab.
	layoutMedium
	// layoutNarrow drops the right-hand column too: one pane at a time.
	layoutNarrow
)

// Width thresholds for the responsive layout: below wideMinWidth the task
// column costs the conversation more than it gives, below mediumMinWidth the
// inspector is too narrow to read a message in.
const (
	wideMinWidth   = 130
	mediumMinWidth = 96
)

// commandState holds the ":" command line.
type commandState struct {
	input string
	// selected is the index into the filtered completion list.
	selected int
	// history is the human's previously run commands, newest last.
	history []string
	// histIdx walks history; len(history) means "not walking".
	histIdx int
}

// createForm holds the in-progress state of the "new task" form. It is a
// simple focused-field form; no external form/widget library is used.
type createForm struct {
	focus       int // a field* constant (formteam.go)
	title       string
	description string

	// recruit holds how many agents of each Recruit entry (by index into
	// recruitEntries()) the task takes; recruitCursor is the selected entry.
	recruit       map[int]int
	recruitCursor int
	// cursor is the selected row in Participants (formMembers()), and
	// authority the participant the human ticked as authority.
	cursor    int
	authority domain.AgentID
}

// formFields is how many fields the new-task form has.
const formFields = 4

// promptState holds the in-progress state of a single-line text prompt used
// for pause/cancel/approve/reject reasons and for send-instruction content.
type promptState struct {
	action       Action
	label        string
	input        string
	taskID       domain.TaskID
	participants []domain.AgentID // candidate recipients, for ActionSendInstruction
	targetIdx    int
	recruit      ports.ModelInfo // the model being recruited, for ActionRecruit
}

// Model is the Bubble Tea model for the whole application. All fields are
// unexported: outside code interacts with the TUI only via Run.
type Model struct {
	ctx context.Context
	svc ports.Service

	opts Options

	width  int
	height int

	agents []domain.Agent
	// liveSince is when each agent began its current stretch of work on the
	// selected task (see working.go), keyed by liveKey.
	liveSince map[string]time.Time

	tasks         []*domain.Task
	currentTaskID domain.TaskID

	messages  []*domain.Message
	events    []domain.Event // events for the current task (task detail)
	decisions []*domain.Decision

	// globalEvents is the chronological, cross-task event log fed by
	// svc.Subscribe. It is what the event-log pane renders.
	globalEvents []domain.Event

	conversationScroll int

	// paneScroll holds a per-pane scroll offset, counted back from the newest
	// line. Scrolling used to be wired only to the conversation and the
	// focused agent, so content in the decisions, event-log and task panes was
	// simply unreachable.
	paneScroll map[int]int
	// activePane is the pane the scroll keys act on.
	activePane pane

	// convSel is the selected conversation entry (an index into
	// convItems), decisionSel the selected decision in the decisions tab.
	// While follow is on, the conversation selection rides the newest entry.
	convSel     int
	decisionSel int
	// The team tab: the models the providers offer, and the selected one.
	models        []ports.ModelInfo
	modelsLoading bool
	modelsErr     error
	modelSel      int
	// inspect is what the inspector shows; zoom gives the inspector the
	// whole body, for reading a long message.
	inspect inspectSubject
	// backlog is how many messages wait undelivered for each agent of the
	// selected task, while it runs.
	backlog map[domain.AgentID]int
	// inputSelected is true while the whole text of the field being edited
	// is selected (ctrl+a): typing replaces it, backspace clears it.
	inputSelected bool
	zoom          bool

	// activity holds each agent's live, ephemeral output stream (see
	// ports.AgentActivity). It is populated only from the live event
	// subscription -- it is never fetched from Service and never survives a
	// restart, matching the backend's guarantee that this data is not
	// persisted.
	activity map[domain.AgentID]*agentActivity

	// focusedAgent is the index into m.agents of a single agent expanded to
	// fill the whole body, or -1 when the split multi-column view is active.
	// It is intentionally -1 rather than the zero value 0 so a freshly
	// constructed Model defaults to the split view instead of appearing to
	// have focused agent 0 on startup.
	focusedAgent int
	// focusScroll counts lines back from the newest, for the focused pane,
	// the same way conversationScroll does for the conversation pane.
	focusScroll int

	// spinnerFrame drives the animated "working" indicator. It only advances
	// while tickerActive is true, and tickerActive is only kept true while
	// at least one agent is WORKING, so an idle screen stops repainting.
	spinnerFrame int
	tickerActive bool

	// center is the active centre-column tab.
	center centerTab
	// agentCursor is the row selected in the agents pane, as an index into
	// identities(). It drives the inspector: "what is this one agent doing"
	// is a question asked constantly during a run, and it should not require
	// leaving the overview to answer.
	agentCursor int

	// showHelpBar lets the human reclaim a row on a small terminal.
	showHelpBar bool
	// showThinking filters the agent's scratch reasoning out of the merged
	// stream. It is on by default: watching an agent reason is most of the
	// value of watching at all.
	showThinking bool
	// follow pins the stream to its newest line. Turning it off is what lets
	// the reader scroll back through a run that is still producing output.
	follow bool
	// streamFilter restricts the merged stream to one agent, "" for all.
	streamFilter domain.AgentID

	// met is the rolling throughput window behind the THROUGHPUT pane.
	met metrics

	mode   mode
	form   createForm
	prompt promptState
	cmd    commandState

	// frameIdents is the identity table for the frame being rendered. It is
	// set only on View()'s private copy of the Model (a value receiver), so it
	// can never go stale in the model Update returns; it exists so the ~8
	// renderers that colour an agent share one computation per frame instead of
	// each rebuilding the table.
	frameIdents []agentIdent
	// frameGeom is the layout for the frame being rendered, for the same
	// reason: every box asks whether it is the active pane, and each answer
	// needs the geometry.
	frameGeom *geom

	statusLine string
	err        error

	quitting bool
}

// maxGlobalEvents bounds the in-memory event log so a long-running session
// does not grow the Model unboundedly.
const maxGlobalEvents = 500

// Init satisfies tea.Model. It fires off the initial data loads and,
// optionally, creation of the demo task.
func (m Model) Init() tea.Cmd {
	cmds := []tea.Cmd{
		loadAgentsCmd(m.ctx, m.svc, m.currentTaskID),
		loadTasksCmd(m.ctx, m.svc),
	}
	if m.opts.DemoOnStart {
		cmds = append(cmds, createTaskCmd(m.ctx, m.svc, ports.CreateTaskInput{
			Title:       "Fix authentication cookie bug",
			Description: "Cookies are not consistently scoped; investigate and resolve.",
			Authority:   m.opts.DefaultAuthority,
			Participants: []domain.AgentID{
				m.opts.DefaultAuthority,
			},
		}))
	}
	return tea.Batch(cmds...)
}

// layout chooses the responsive mode for the current terminal width.
func (m Model) layout() layout {
	switch {
	case m.width >= wideMinWidth:
		return layoutWide
	case m.width >= mediumMinWidth:
		return layoutMedium
	default:
		return layoutNarrow
	}
}

// centerTabs lists the tabs available in the centre column. The list grows as
// the terminal narrows: panes that no longer have a column of their own fold
// in here rather than becoming unreachable.
func (m Model) centerTabs() []centerTab {
	tabs := []centerTab{tabConversation, tabActivity, tabEvents, tabDecisions, tabTeam}
	switch m.layout() {
	case layoutMedium:
		tabs = append(tabs, tabTasks)
	case layoutNarrow:
		tabs = append(tabs, tabTasks, tabAgents, tabInspector)
	}
	return tabs
}

// centerOrDefault returns the active tab, falling back to the stream when the
// stored one is not available at this width (e.g. the window was widened and
// the inspector went back to being its own pane).
func (m Model) centerOrDefault() centerTab {
	for _, t := range m.centerTabs() {
		if t == m.center {
			return t
		}
	}
	return tabConversation
}

// cycleCenterTab moves the centre column to the next/previous tab.
func (m Model) cycleCenterTab(delta int) Model {
	tabs := m.centerTabs()
	cur := 0
	active := m.centerOrDefault()
	for i, t := range tabs {
		if t == active {
			cur = i
		}
	}
	next := (cur + delta) % len(tabs)
	if next < 0 {
		next += len(tabs)
	}
	m.center = tabs[next]
	return m
}

// selectedIdent returns the agent the agents-pane cursor is on, which is also
// the agent the inspector describes.
func (m Model) selectedIdent() (agentIdent, bool) {
	idents := m.rosterIdents()
	if len(idents) == 0 {
		return agentIdent{}, false
	}
	i := m.agentCursor
	if i < 0 || i >= len(idents) {
		i = 0
	}
	return idents[i], true
}

// elapsed reports how long the current task has been running. Tasks carry a
// CreatedAt, so this is real wall-clock time rather than time since the TUI
// happened to be started.
func (m Model) elapsed() (time.Duration, bool) {
	t := m.currentTask()
	if t == nil || t.CreatedAt.IsZero() {
		return 0, false
	}
	end := time.Now()
	if t.CompletedAt != nil {
		end = *t.CompletedAt
	}
	d := end.Sub(t.CreatedAt)
	if d < 0 {
		return 0, false
	}
	return d, true
}

// pendingDecision returns the decision awaiting a human answer, if any.
//
// This is the one place in the UI that is allowed to take over the centre of
// the screen: the run is blocked on it, and a blocked run that looks like a
// running one is the worst failure mode this tool has.
func (m Model) pendingDecision() (*domain.Decision, bool) {
	for i := len(m.decisions) - 1; i >= 0; i-- {
		if m.decisions[i] != nil && m.decisions[i].Status == domain.DecisionProposed {
			return m.decisions[i], true
		}
	}
	return nil, false
}

// agentSummary counts agents by the states the footer reports.
func (m Model) agentSummary() (working, errored, enabled int) {
	for _, a := range m.agents {
		switch a.State {
		case domain.AgentStateWorking:
			working++
		case domain.AgentStateError, domain.AgentStateUnavailable:
			errored++
		}
		if a.Enabled {
			enabled++
		}
	}
	return working, errored, enabled
}

// currentTask returns the task matching currentTaskID, or nil.
func (m Model) currentTask() *domain.Task {
	for _, t := range m.tasks {
		if t.ID == m.currentTaskID {
			return t
		}
	}
	return nil
}

// currentTaskIndex returns the index of the current task in m.tasks, or -1.
func (m Model) currentTaskIndex() int {
	for i, t := range m.tasks {
		if t.ID == m.currentTaskID {
			return i
		}
	}
	return -1
}

// taskScope decides which task-stamped items (orchestrator events, live agent
// activity) belong on the screen of the task being viewed.
//
// Agents are shared by every task, and both the event feed and the activity
// feed are global, so without a scope task B's tool calls show up in task A's
// stream. The rule: an item stamped with the current task belongs; an item
// stamped with another task does not; an item with no task stamp (adapters and
// a few lifecycle events omit it) belongs only if it concerns an agent that
// takes part in the current task, since that is the only case where showing it
// is plausible rather than noise.
type taskScope struct {
	id          domain.TaskID
	participant bool
	// all disables scoping (used where a caller wants the raw buffer).
	all bool
}

// has reports whether an item stamped with task belongs in the scope.
func (s taskScope) has(task domain.TaskID) bool {
	if s.all || task == s.id {
		return true
	}
	return task == "" && s.participant
}

// scopeFor builds the scope for items concerning one agent.
func (m Model) scopeFor(agent domain.AgentID) taskScope {
	sc := taskScope{id: m.currentTaskID, participant: true}
	if t := m.currentTask(); t != nil {
		sc.participant = t.AuthorityAgent == agent || containsAgent(t.Participants, agent)
	}
	return sc
}

// agentByID looks up a known agent by id.
func (m Model) agentByID(id domain.AgentID) (domain.Agent, bool) {
	for _, a := range m.agents {
		if a.ID == id {
			return a, true
		}
	}
	return domain.Agent{}, false
}

// activityFor returns the activity buffer for id, creating an empty one on
// first use. It has a pointer receiver so it can lazily initialise the
// activity map itself; callers with an addressable Model (any local
// variable, which is what every Update branch operates on) get this for
// free without needing to pre-populate the map.
func (m *Model) activityFor(id domain.AgentID) *agentActivity {
	if m.activity == nil {
		m.activity = map[domain.AgentID]*agentActivity{}
	}
	a, ok := m.activity[id]
	if !ok {
		a = &agentActivity{}
		m.activity[id] = a
	}
	return a
}

// focusedAgentID returns the agent currently expanded to full-screen, if
// any.
func (m Model) focusedAgentID() (domain.AgentID, bool) {
	if m.focusedAgent < 0 || m.focusedAgent >= len(m.agents) {
		return "", false
	}
	return m.agents[m.focusedAgent].ID, true
}

// anyAgentWorking reports whether the spinner should be animating.
func (m Model) anyAgentWorking() bool {
	for _, a := range m.agents {
		if a.State == domain.AgentStateWorking {
			return true
		}
	}
	return false
}

// wantsTicker reports whether the screen has anything that changes on its own.
//
// It is broader than "an agent is working": the header carries a live elapsed
// clock and the throughput window decays, so a task that is in flight but
// momentarily quiet still needs repaints. A task in a terminal state, with no
// agent working, does not -- and an idle screen must stop repainting.
func (m Model) wantsTicker() bool {
	if m.anyAgentWorking() {
		return true
	}
	t := m.currentTask()
	return t != nil && !t.State.IsTerminal()
}

// startTickerIfNeeded returns a tea.Cmd that starts the animation clock the
// first time the screen has something live on it. It is idempotent: called
// again while the ticker is already running (tickerActive) it does nothing,
// so repeated activity events do not spawn overlapping tea.Tick loops.
func (m Model) startTickerIfNeeded() (Model, tea.Cmd) {
	if m.tickerActive || !m.wantsTicker() {
		return m, nil
	}
	m.tickerActive = true
	return m, tickCmd(m.anyAgentWorking())
}

// setStatus sets the status/help line to a plain informational message.
// allAgentsAreMocks reports whether every registered agent is a mock. It is
// what drives the mock-mode banner: running against a fixed script is easy to
// mistake for a working system, so it must be visible on screen.
func (m Model) allAgentsAreMocks() bool {
	if len(m.agents) == 0 {
		return false
	}
	for _, a := range m.agents {
		if a.Kind != domain.AgentKindMock {
			return false
		}
	}
	return true
}

func (m *Model) setStatus(format string, args ...any) {
	m.statusLine = fmt.Sprintf(format, args...)
	m.err = nil
}

// setError sets the status/help line to an error message.
func (m *Model) setError(err error) {
	if err == nil {
		return
	}
	m.err = err
	m.statusLine = "error: " + err.Error()
}

// newModel builds a Model with the defaults a human expects on first paint.
//
// Several of those defaults are not the zero value -- the split view is -1 not
// 0, the stream follows, thinking is shown -- so construction goes through
// here rather than through a struct literal, and tests use it too. A fixture
// that disagrees with Run() about the starting state tests a screen nobody
// ever sees.
func newModel(ctx context.Context, svc ports.Service, opts Options) Model {
	return Model{
		ctx:          ctx,
		svc:          svc,
		opts:         opts,
		statusLine:   "loading...",
		focusedAgent: -1,
		showHelpBar:  true,
		showThinking: true,
		follow:       true,
	}
}

// Run starts the TUI and blocks until the user quits (or ctx is done).
//
// It wires svc.Subscribe to forward every domain.Event into the running
// Bubble Tea program as a tea.Msg, so the UI updates live as the
// orchestrator makes progress -- the TUI never polls.
func Run(ctx context.Context, svc ports.Service, opts Options) error {
	m := newModel(ctx, svc, opts)

	// Mouse reporting makes the wheel scroll the pane under the pointer and a
	// click focus it. The cost is that the terminal no longer selects text on
	// a plain drag: hold Shift while dragging to copy (Windows Terminal and
	// most others honour that).
	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())

	unsubscribe := svc.Subscribe(func(e domain.Event) {
		p.Send(domainEventMsg(e))
	})
	defer unsubscribe()

	// If the caller's context is cancelled, quit the program rather than
	// leaving the terminal in alt-screen mode.
	go func() {
		<-ctx.Done()
		p.Quit()
	}()

	_, err := p.Run()
	return err
}
