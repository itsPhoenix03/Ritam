package tui

import (
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Update satisfies tea.Model. It is intentionally thin: key handling in
// modeNormal funnels through dispatch(Action); the form/prompt modes have
// their own small, self-contained key handlers because they need to accept
// arbitrary text input rather than single-key actions.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	mm, ok := next.(Model)
	if !ok || !(mm.needsModels() || mm.formNeedsModels()) {
		return next, cmd
	}
	// The team tab lists the models; fetch them the first time it shows.
	mm.modelsLoading = true
	return mm, tea.Batch(cmd, loadModelsCmd(mm.ctx, mm.svc))
}

func (m Model) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case modelsLoadedMsg:
		m.modelsLoading = false
		m.models, m.modelsErr = msg.models, msg.err
		if msg.models == nil && msg.err == nil {
			m.models = []ports.ModelInfo{}
		}
		return m, nil

	case modelCheckedMsg:
		return m.applyModelCheck(msg), nil

	case teamChangedMsg:
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.setStatus("%s", msg.status)
		return m, loadAgentsCmd(m.ctx, m.svc, m.currentTaskID)

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.MouseMsg:
		return m.handleMouse(msg)

	case tea.KeyMsg:
		switch m.mode {
		case modeCreateForm:
			return m.updateCreateForm(msg)
		case modePrompt:
			return m.updatePrompt(msg)
		case modeCommand:
			return m.updateCommandLine(msg)
		case modeHelp:
			// The reference is longer than most terminals, so the scroll keys
			// keep working inside it; any other key closes it.
			if a, _, ok := lookupAction(msg.String()); ok && isScrollAction(a) {
				return m.dispatch(a, 0)
			}
			m.mode = modeNormal
			return m, nil
		default:
			action, arg, ok := lookupAction(msg.String())
			if !ok {
				return m, nil
			}
			return m.dispatch(action, arg)
		}

	case agentsLoadedMsg:
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.agents = msg.agents
		m = m.trackLive(msg.agents)
		return m.startTickerIfNeeded()

	case tickMsg:
		if !m.wantsTicker() {
			// Nothing left to animate or to clock; let the ticker die rather
			// than rescheduling, per startTickerIfNeeded's contract.
			m.tickerActive = false
			return m, nil
		}
		m.spinnerFrame++
		// Rolling the throughput window here rather than only on new activity
		// is what makes a quiet minute actually decay to zero on screen
		// instead of freezing at the last busy sample.
		m.met.advance(time.Time(msg))
		return m, tickCmd(m.anyAgentWorking())

	case exportDoneMsg:
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.setStatus("%s", msg)
		return m, nil

	case tasksLoadedMsg:
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.tasks = msg.tasks
		var cmds []tea.Cmd
		if m.currentTask() == nil && len(m.tasks) > 0 {
			m.currentTaskID = m.tasks[0].ID
			cmds = append(cmds, loadTaskDetailCmd(m.ctx, m.svc, m.currentTaskID))
		}
		// A live task drives the header's elapsed clock even with every agent
		// momentarily idle, so the ticker is (re)armed here too.
		m, tick := m.startTickerIfNeeded()
		if tick != nil {
			cmds = append(cmds, tick)
		}
		return m, tea.Batch(cmds...)

	case taskDetailMsg:
		if msg.taskID != m.currentTaskID {
			// Stale response for a task we've since navigated away from.
			return m, nil
		}
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		_, wasPending := m.pendingDecision()
		m.messages = msg.messages
		m.events = msg.events
		m.decisions = msg.decisions
		m.backlog = msg.backlog
		// A decision that starts waiting on the human is put in front of
		// them: the run holds until they answer it.
		if _, pending := m.pendingDecision(); pending && !wasPending {
			m = m.inspectNow(inspectDecision)
		}
		return m, nil

	case taskCreatedMsg:
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.setStatus("created %s", msg.task.ID)
		m.currentTaskID = msg.task.ID
		m.mode = modeNormal
		return m, tea.Batch(
			loadTasksCmd(m.ctx, m.svc),
			loadTaskDetailCmd(m.ctx, m.svc, msg.task.ID),
		)

	case actionResultMsg:
		if msg.err != nil {
			m.setError(msg.err)
			return m, nil
		}
		m.setStatus("%s ok for %s", msg.action, msg.taskID)
		return m, tea.Batch(
			loadTasksCmd(m.ctx, m.svc),
			loadTaskDetailCmd(m.ctx, m.svc, msg.taskID),
		)

	case domainEventMsg:
		ev := domain.Event(msg)

		// AgentActivity is the one event type that is never persisted (see
		// ports.AgentActivity's doc comment) and arrives roughly every
		// 80ms per active agent. It must not flow into the durable event
		// log, and it must not trigger a task-detail reload -- doing either
		// would mean hitting Service on every text fragment of every
		// streaming agent, for data that TaskEvents/TaskMessages do not
		// even contain.
		if ev.Type == domain.EvtAgentActivity {
			return m.applyAgentActivity(ev)
		}

		m.globalEvents = append(m.globalEvents, ev)
		if len(m.globalEvents) > maxGlobalEvents {
			m.globalEvents = m.globalEvents[len(m.globalEvents)-maxGlobalEvents:]
		}
		if isFailureEvent(ev.Type) {
			m.met.recordError(eventTime(ev))
		}
		var cmds []tea.Cmd
		if ev.TaskID == m.currentTaskID && m.currentTaskID != "" {
			cmds = append(cmds, loadTaskDetailCmd(m.ctx, m.svc, m.currentTaskID))
		}
		switch ev.Type {
		case domain.EvtTaskCreated, domain.EvtTaskStateChanged, domain.EvtTaskPaused,
			domain.EvtTaskResumed, domain.EvtTaskCancelled, domain.EvtTaskCompleted,
			domain.EvtTaskFailed, domain.EvtTaskReopened:
			cmds = append(cmds, loadTasksCmd(m.ctx, m.svc))
		case domain.EvtAgentRegistered, domain.EvtAgentStarted, domain.EvtAgentStopped,
			domain.EvtAgentStateChanged, domain.EvtAgentFailed:
			// The agent pane's state dot and the spinner both read
			// m.agents, so a lifecycle change needs a fresh copy.
			if ev.TaskID == "" || ev.TaskID == m.currentTaskID {
				cmds = append(cmds, loadAgentsCmd(m.ctx, m.svc, m.currentTaskID))
			}
		}
		if len(cmds) == 0 {
			return m, nil
		}
		return m, tea.Batch(cmds...)
	}

	return m, nil
}

// applyAgentActivity folds one AgentActivity event into the target agent's
// buffer. Activity for an agent id the TUI does not know about is dropped
// rather than causing a lookup failure: adapters are allowed to race ahead
// of Agents() during startup/shutdown, and this stream is best-effort
// display only, never something the orchestrator depends on being seen.
func (m Model) applyAgentActivity(ev domain.Event) (tea.Model, tea.Cmd) {
	if _, ok := m.agentByID(ev.AgentID); !ok {
		return m, nil
	}
	kind := ports.ActivityKind(ev.Details["kind"])
	at := eventTime(ev)
	m.activityFor(ev.AgentID).appendTask(kind, ev.Summary, at, ev.TaskID)
	if kind == ports.ActivityTool {
		m.met.recordTool(at)
	}
	// A CLI that dies reports it as a status line, and no EvtAgentFailed need
	// follow, so failures seen only as activity would never reach the errors
	// series. (When both arrive the failure counts twice; undercounting a
	// failing run is the worse error.)
	if kind == ports.ActivityStatus && isFailureStatus(ev.Summary) {
		m.met.recordError(at)
	}
	return m.startTickerIfNeeded()
}

// isFailureStatus reports whether an activity status line reports a failure.
func isFailureStatus(s string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "failed:")
}

// eventTime returns an event's timestamp, substituting now for the zero value.
// Adapters are allowed to omit it, and an unstamped event sorted to the epoch
// would sit at the top of the merged stream forever.
func eventTime(ev domain.Event) time.Time {
	if ev.Timestamp.IsZero() {
		return time.Now()
	}
	return ev.Timestamp
}

// pageStep is how many lines page-up/page-down move.
const pageStep = 10

// paneContentLines is how many display lines a text pane holds, so
// scrolling stops at its last line instead of running into empty space.
func (m Model) paneContentLines(p pane) int {
	if m.mode == modeHelp {
		return len(m.helpLines())
	}
	inner, _ := m.paneBox(p)
	switch p {
	case paneInspector:
		_, lines := m.inspectorContent(inner)
		return len(lines)
	case paneAgent:
		if id, ok := m.focusedAgentID(); ok {
			if buf, ok := m.activity[id]; ok {
				return buf.countIn(m.scopeFor(id)) * 6
			}
		}
		return 0
	case paneConversation:
		switch m.centerOrDefault() {
		case tabActivity:
			return len(m.streamLines(m.mergedStream(), inner))
		case tabEvents:
			return len(m.eventLines(inner))
		}
	}
	return 0
}

// maxScroll is the largest useful offset for a text pane: for a
// top-anchored one, the offset that brings its last line to the bottom.
func (m Model) maxScroll(p pane) int {
	n := m.paneContentLines(p)
	if m.anchorTop(p) {
		_, rows := m.paneBox(p)
		n -= rows
	}
	return maxInt(0, n)
}

// scrollActive scrolls the pane the keys act on.
func (m Model) scrollActive(delta int) Model { return m.scrollPane(m.scrollTarget(), delta) }

// scrollPane moves text pane p by delta lines (positive = towards earlier
// content), clamped to its content.
func (m Model) scrollPane(p pane, delta int) Model {
	cur := m.scrollOf(p)
	top := m.anchorTop(p)
	if top {
		// Earlier content is a smaller offset in a top-anchored pane.
		delta = -delta
	}
	next := clampInt(cur+delta, 0, m.maxScroll(p))
	// Scrolling a log back means the reader wants to stay there; arriving
	// back at the newest line means they have caught up.
	if p == paneConversation && !top && m.mode == modeNormal {
		m.follow = next == 0
	}
	m.setScroll(p, next)
	return m
}

// dispatch is the single place normal-mode Actions turn into model changes
// and/or commands that call ports.Service. Every action the keymap can
// produce is handled here. arg carries keyBinding.Arg for the one action
// that needs it (ActionFocusAgent); every other action ignores it.
func (m Model) dispatch(a Action, arg int) (tea.Model, tea.Cmd) {
	switch a {
	case ActionQuit:
		m.quitting = true
		return m, tea.Quit

	case ActionHelp:
		m.mode = modeHelp
		return m, nil

	case ActionCreateTask:
		m.mode = modeCreateForm
		// The team starts empty: the human picks it under Recruit and ticks
		// the authority among the participants (formteam.go).
		m.form = createForm{recruit: map[int]int{}}
		return m, nil

	case ActionNextTask, ActionPrevTask:
		if len(m.tasks) == 0 {
			m.setStatus("no tasks")
			return m, nil
		}
		step := 1
		if a == ActionPrevTask {
			step = -1
		}
		idx := (m.currentTaskIndex() + step + len(m.tasks)) % len(m.tasks)
		return m.selectTask(idx)

	case ActionInspectDecisions:
		m.center, m.activePane, m.zoom = tabDecisions, paneConversation, false
		m.decisionSel = len(m.decisions) - 1
		if len(m.decisions) > 0 {
			m = m.inspectNow(inspectDecision)
		}
		return m, nil

	case ActionInspectEvents:
		m.center, m.activePane, m.zoom = tabEvents, paneConversation, false
		return m, nil

	case ActionNextPane:
		return m.cycleActivePane(1), nil

	case ActionPrevPane:
		return m.cycleActivePane(-1), nil

	case ActionNextTab:
		m.zoom = false
		m.activePane = paneConversation
		return m.cycleCenterTab(1), nil

	case ActionPrevTab:
		m.zoom = false
		m.activePane = paneConversation
		return m.cycleCenterTab(-1), nil

	case ActionCursorUp:
		return m.moveAgentCursor(-1).inspectNow(inspectAgent), nil

	case ActionCursorDown:
		return m.moveAgentCursor(+1).inspectNow(inspectAgent), nil

	case ActionInspect:
		if m.scrollTarget() == paneConversation && m.centerOrDefault() == tabTeam {
			return m.startRecruit(), nil
		}
		return m.open(), nil

	case ActionBack:
		switch {
		case m.zoom:
			m.zoom = false
		case m.focusedAgent >= 0:
			return m.setFocus(0)
		case m.activeOrDefault() != paneConversation:
			m.activePane = paneConversation
		}
		return m, nil

	case ActionToggleFollow:
		m.follow = !m.follow
		if m.follow {
			// Re-pinning also snaps back to the newest line.
			m.setScroll(paneConversation, 0)
			if m.centerOrDefault() == tabConversation {
				m = m.inspectNow(inspectMessage)
			}
		}
		m.setStatus("follow %s", onOff(m.follow))
		return m, nil

	case ActionToggleThinking:
		m.showThinking = !m.showThinking
		m.setStatus("agent thinking %s", onOff(m.showThinking))
		return m, nil

	case ActionToggleInspector:
		// v zooms the inspector to the whole body, for reading a long
		// message or decision, and back.
		m.zoom = !m.zoom
		if m.zoom {
			m.setStatus("reading in full: j/k scroll, v or esc returns")
		}
		return m, nil

	case ActionToggleHelpBar:
		m.showHelpBar = !m.showHelpBar
		return m, nil

	case ActionCommand:
		m.mode = modeCommand
		m.cmd = commandState{history: m.cmd.history, histIdx: len(m.cmd.history)}
		return m, nil

	case ActionFilter:
		m.mode = modeCommand
		m.cmd = commandState{
			input:   "filter ",
			history: m.cmd.history,
			histIdx: len(m.cmd.history),
		}
		return m, nil

	// Scrolling acts on the active pane, whichever that is. Every visible
	// pane is scrollable; previously only the conversation and a focused
	// agent were, so content elsewhere could not be reached at all.
	// j/k, the arrows and page up/down all go through navigate, which moves
	// the selection in a list pane and scrolls everywhere else.
	case ActionScrollUp:
		return m.navigate(m.scrollTarget(), +1)

	case ActionScrollDown:
		return m.navigate(m.scrollTarget(), -1)

	case ActionPageUp:
		return m.navigate(m.scrollTarget(), +pageStep)

	case ActionPageDown:
		return m.navigate(m.scrollTarget(), -pageStep)

	// "Oldest" and "newest" are ends of the content, not of the offset scale,
	// so which extreme they map to depends on which end the pane is anchored
	// at (see anchorsTop).
	case ActionScrollTop:
		p := m.scrollTarget()
		if m.isListPane(p) {
			return m.navigate(p, 1<<20)
		}
		v := m.maxScroll(p)
		if m.anchorTop(p) {
			v = 0
		}
		m.setScroll(p, v)
		if p == paneConversation && v > 0 {
			m.follow = false
		}
		return m, nil

	case ActionScrollBottom:
		p := m.scrollTarget()
		if m.isListPane(p) {
			return m.navigate(p, -(1 << 20))
		}
		v := 0
		if m.anchorTop(p) {
			v = m.maxScroll(p)
		}
		m.setScroll(p, v)
		return m, nil

	case ActionFocusAgent:
		return m.setFocus(arg)

	// Pause/resume/cancel/approve/reject are single-key, immediate human
	// overrides: each maps straight onto one ports.Service call. A richer
	// reason/note prompt (like the one send-instruction uses) is deliberately
	// left for a later phase -- see TODO(phase-next) below -- because the
	// override must never be one keypress away from being blocked on typing.
	case ActionPauseTask:
		t := m.currentTask()
		if t == nil {
			m.setStatus("no task selected")
			return m, nil
		}
		m.setStatus("pausing %s...", t.ID)
		return m, pauseCmd(m.ctx, m.svc, t.ID, domain.PauseReasonHuman)

	case ActionResumeTask:
		t := m.currentTask()
		if t == nil {
			m.setStatus("no task selected")
			return m, nil
		}
		m.setStatus("resuming %s...", t.ID)
		return m, resumeCmd(m.ctx, m.svc, t.ID)

	case ActionCancelTask:
		t := m.currentTask()
		if t == nil {
			m.setStatus("no task selected")
			return m, nil
		}
		m.setStatus("cancelling %s...", t.ID)
		// TODO(phase-next): prompt for a cancellation reason instead of "".
		return m, cancelCmd(m.ctx, m.svc, t.ID, "")

	case ActionApprove:
		t := m.currentTask()
		if t == nil {
			m.setStatus("no task selected")
			return m, nil
		}
		m.setStatus("approving %s...", t.ID)
		// TODO(phase-next): prompt for an approval note instead of "".
		return m, approveCmd(m.ctx, m.svc, t.ID, "")

	case ActionReject:
		t := m.currentTask()
		if t == nil {
			m.setStatus("no task selected")
			return m, nil
		}
		m.setStatus("rejecting %s...", t.ID)
		// TODO(phase-next): prompt for a rejection note instead of "".
		return m, rejectCmd(m.ctx, m.svc, t.ID, "")

	case ActionSendInstruction:
		t := m.currentTask()
		if t == nil {
			m.setStatus("no task selected")
			return m, nil
		}
		if len(t.Participants) == 0 {
			m.setStatus("task has no participants to instruct")
			return m, nil
		}
		label := "Instruction (tab=change recipient, enter=send, esc=cancel)"
		if t.State == domain.TaskCompleted {
			// Its agents have stopped; sending will reopen the task and
			// resume their sessions, so say so before the human types.
			label = "Follow-up, reopens " + string(t.ID) + " (tab=change recipient, enter=send, esc=cancel)"
		}
		m.mode = modePrompt
		m.prompt = promptState{
			action:       ActionSendInstruction,
			label:        label,
			taskID:       t.ID,
			participants: t.Participants,
		}
		return m, nil
	}

	// Anything reaching here is a recognised Action with no behaviour wired
	// up yet in this phase.
	// TODO(phase-next): wire remaining actions (e.g. artifact inspection,
	// knowledge search UI) once the corresponding panes exist.
	m.setStatus("%s: not implemented in this phase", a)
	return m, nil
}

// updateCommandLine handles key input while the ":" command line is active.
func (m Model) updateCommandLine(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.inputSelected = false
		m.setStatus("cancelled")
		return m, nil

	case "enter":
		input := m.cmd.input
		m.mode = modeNormal
		m.inputSelected = false
		mm, cmd := m.runCommand(input)
		return mm, cmd

	case "tab":
		return m.completeCommand(), nil

	case "down":
		if n := len(matchCommands(m.cmd.input)); n > 0 {
			m.cmd.selected = (m.cmd.selected + 1) % n
		}
		return m, nil

	case "up":
		if n := len(matchCommands(m.cmd.input)); n > 0 {
			m.cmd.selected = (m.cmd.selected - 1 + n) % n
		}
		return m, nil

	case "ctrl+p":
		// History walks on ctrl+p/ctrl+n so up/down can stay on the menu.
		if m.cmd.histIdx > 0 {
			m.cmd.histIdx--
			m.cmd.input = m.cmd.history[m.cmd.histIdx]
		}
		return m, nil

	case "ctrl+n":
		if m.cmd.histIdx < len(m.cmd.history)-1 {
			m.cmd.histIdx++
			m.cmd.input = m.cmd.history[m.cmd.histIdx]
		} else {
			m.cmd.histIdx = len(m.cmd.history)
			m.cmd.input = ""
		}
		return m, nil

	}

	if v, sel, ok := editField(m.cmd.input, m.inputSelected, msg); ok {
		m.cmd.input, m.inputSelected = v, sel
		m.cmd.selected = 0
		return m, nil
	}
	m.inputSelected = false
	return m, nil
}

// updateCreateForm handles key input while the new-task form is active.
func (m Model) updateCreateForm(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.inputSelected = false
		m.setStatus("task creation cancelled")
		return m, nil

	case "tab":
		m.form.focus = (m.form.focus + 1) % formFields
		m.inputSelected = false
		return m, nil

	case "shift+tab":
		m.inputSelected = false
		m.form.focus--
		if m.form.focus < 0 {
			m.form.focus = formFields - 1
		}
		return m, nil

	case "enter":
		m.inputSelected = false
		if m.form.focus < fieldRecruit {
			m.form.focus++
			return m, nil
		}
		return m.submitCreateForm()
	}

	if m.form.focus == fieldRecruit && msg.String() == "c" {
		if entries := m.recruitEntries(); len(entries) > 0 {
			mi := entries[clampInt(m.form.recruitCursor, 0, len(entries)-1)]
			if mi.ID == "" {
				m.setStatus("%s runs its CLI's default model; nothing to check", mi.Provider)
				return m, nil
			}
			return m.checking(mi)
		}
		return m, nil
	}
	if m.form.focus >= fieldRecruit {
		return m.updateFormTeam(msg), nil
	}

	// Title and description are text fields.
	if m.form.focus == 0 || m.form.focus == 1 {
		field := &m.form.title
		if m.form.focus == 1 {
			field = &m.form.description
		}
		if v, sel, ok := editField(*field, m.inputSelected, msg); ok {
			*field, m.inputSelected = v, sel
			return m, nil
		}
		m.inputSelected = false
	}
	return m, nil
}

// submitCreateForm validates and fires the CreateTask command.
func (m Model) submitCreateForm() (tea.Model, tea.Cmd) {
	if m.form.title == "" {
		m.setStatus("title is required")
		return m, nil
	}
	// Participants are the existing agents in m.agents order (so the order
	// is deterministic) with the authority once, then the recruits. The
	// authority may itself be a recruit.
	if n := len(m.formMembers()); n < 2 {
		m.setStatus("recruit at least two agents: the authority needs somebody to delegate to")
		return m, nil
	}
	authority, participants, recruits := m.formSubmission()
	if authority == "" {
		m.setStatus("tick one participant as the authority")
		return m, nil
	}

	in := ports.CreateTaskInput{
		Title:        m.form.title,
		Description:  m.form.description,
		Authority:    authority,
		Participants: participants,
	}
	m.mode = modeNormal
	if len(recruits) > 0 {
		m.setStatus("recruiting %d agent(s) and creating the task...", len(recruits))
		return m, createTaskWithRecruitsCmd(m.ctx, m.svc, in, recruits)
	}
	m.setStatus("creating task...")
	return m, createTaskCmd(m.ctx, m.svc, in)
}

// updatePrompt handles key input for the text prompt used by
// pause/cancel/approve/reject/send-instruction.
func (m Model) updatePrompt(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeNormal
		m.inputSelected = false
		m.setStatus("cancelled")
		return m, nil

	case "tab":
		if m.prompt.action == ActionSendInstruction && len(m.prompt.participants) > 0 {
			m.prompt.targetIdx = (m.prompt.targetIdx + 1) % len(m.prompt.participants)
		}
		return m, nil

	case "enter":
		m.inputSelected = false
		return m.submitPrompt()
	}

	if v, sel, ok := editField(m.prompt.input, m.inputSelected, msg); ok {
		m.prompt.input, m.inputSelected = v, sel
		return m, nil
	}
	m.inputSelected = false
	return m, nil
}

// submitPrompt handles the only prompt currently wired up: send-instruction,
// which needs both free-text content and a chosen recipient and so cannot
// have a sane one-key default the way pause/cancel/approve/reject do.
func (m Model) submitPrompt() (tea.Model, tea.Cmd) {
	p := m.prompt
	m.mode = modeNormal
	if p.action == ActionRecruit {
		name := strings.TrimSpace(p.input)
		if name == "" {
			m.setStatus("a recruit needs a name")
			return m, nil
		}
		m.setStatus("recruiting %s...", name)
		return m, recruitCmd(m.ctx, m.svc, ports.RecruitInput{
			Name: domain.AgentID(name), Provider: p.recruit.Provider, Model: p.recruit.ID})
	}
	if p.action != ActionSendInstruction {
		return m, nil
	}
	if len(p.participants) == 0 {
		m.setStatus("no recipient available")
		return m, nil
	}
	to := p.participants[p.targetIdx]
	return m.instruct(p.taskID, to, p.input)
}

// instruct sends a human message to one agent. To a running task that is an
// instruction; to a completed one it is a follow-up, which reopens the task
// and resumes the agents' own sessions -- the agents of a finished task have
// stopped, and a plain instruction would have nobody to reach.
func (m Model) instruct(id domain.TaskID, to domain.AgentID, content string) (Model, tea.Cmd) {
	for _, t := range m.tasks {
		if t.ID == id && t.State == domain.TaskCompleted {
			m.setStatus("reopening %s with a follow-up to %s...", id, to)
			return m, followUpCmd(m.ctx, m.svc, id, to, content)
		}
	}
	m.setStatus("sending instruction to %s...", to)
	return m, sendInstructionCmd(m.ctx, m.svc, id, to, content)
}

// setFocus expands agent n (1-based, matching the "1".."9" keys) to fill the
// whole body, or returns to the split view for any out-of-range n --
// including 0, which is what the "0" and "esc" bindings pass.
func (m Model) setFocus(n int) (tea.Model, tea.Cmd) {
	if n <= 0 || n > len(m.agents) {
		if m.focusedAgent >= 0 {
			m.setStatus("back to split view")
		}
		m.focusedAgent = -1
		return m, nil
	}
	m.focusedAgent = n - 1
	m.focusScroll = 0
	m.setStatus("focused on %s (0 or esc to return)", m.agents[m.focusedAgent].ID)
	return m, nil
}

// typedText extracts the text a key event contributes to a text field.
//
// It reads the runes rather than msg.String(): String() decorates the key
// ("alt+x", "[pasted text]") and would type that decoration into the field.
// It only accepts KeyRunes/KeySpace, and on Windows an unmapped key without a
// character (Ctrl, Alt, Caps Lock, Win) arrives as KeyRunes{0}, which used to
// be stored verbatim -- a title of "test\x00 2" later made the agent CLI fail
// to start, because a NUL cannot be passed in an argv.
func typedText(msg tea.KeyMsg) string {
	if msg.Type != tea.KeyRunes && msg.Type != tea.KeySpace {
		return ""
	}
	return sanitizeText(msg.Runes)
}

// sanitizeText keeps only printable text. Control characters (including NUL)
// and other non-printing runes such as zero-width format characters are
// dropped. Tab and newline become a single space and carriage returns vanish,
// so a multi-line paste into the single-line title still reads as one line
// instead of losing its word breaks.
func sanitizeText(rs []rune) string {
	var b strings.Builder
	for _, r := range rs {
		switch {
		case r == '\n' || r == '\t' || r == ' ':
			b.WriteByte(' ')
		case unicode.IsControl(r) || !unicode.IsPrint(r):
			// Dropped. Non-ASCII spaces (NBSP and friends) also land here:
			// they are not IsPrint, and a lookalike space is not worth keeping.
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// trimLastRune removes the last rune of s, respecting UTF-8 boundaries.
func trimLastRune(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	return string(r[:len(r)-1])
}
