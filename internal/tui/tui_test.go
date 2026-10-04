package tui

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// fakeService is a minimal, in-memory ports.Service used only by these
// tests. It records every call so tests can assert the TUI reached the
// service layer, and it returns canned data so View() has something real to
// render.
type fakeService struct {
	mu sync.Mutex

	tasks     []*domain.Task
	messages  map[domain.TaskID][]*domain.Message
	events    map[domain.TaskID][]domain.Event
	decisions map[domain.TaskID][]*domain.Decision
	agents    []domain.Agent
	backlog   map[domain.AgentID]int
	models    []ports.ModelInfo

	lastRecruit ports.RecruitInput
	recruits    []ports.RecruitInput
	checkResult *ports.ModelInfo

	handlers []domain.EventHandler

	createErr error
	created   *domain.Task

	calls map[string]int
	lastReason,
	lastNote,
	lastInstruction string
	lastTo domain.AgentID
}

func newFakeService() *fakeService {
	return &fakeService{
		messages:  map[domain.TaskID][]*domain.Message{},
		events:    map[domain.TaskID][]domain.Event{},
		decisions: map[domain.TaskID][]*domain.Decision{},
		calls:     map[string]int{},
	}
}

func (f *fakeService) record(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[name]++
}

func (f *fakeService) callCount(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[name]
}

func (f *fakeService) CreateTask(ctx context.Context, in ports.CreateTaskInput) (*domain.Task, error) {
	f.record("CreateTask")
	if f.createErr != nil {
		return nil, f.createErr
	}
	t := &domain.Task{
		ID:             domain.FormatTaskID(len(f.tasks) + 1),
		Title:          in.Title,
		Description:    in.Description,
		AuthorityAgent: in.Authority,
		Participants:   in.Participants,
		State:          domain.TaskCreated,
	}
	f.tasks = append(f.tasks, t)
	f.created = t
	return t, nil
}

func (f *fakeService) RunTask(ctx context.Context, id domain.TaskID) error {
	f.record("RunTask")
	return nil
}

func (f *fakeService) ListTasks(ctx context.Context) ([]*domain.Task, error) {
	f.record("ListTasks")
	return f.tasks, nil
}

func (f *fakeService) GetTask(ctx context.Context, id domain.TaskID) (*domain.Task, error) {
	f.record("GetTask")
	for _, t := range f.tasks {
		if t.ID == id {
			return t, nil
		}
	}
	return nil, ports.ErrNotFound
}

func (f *fakeService) TaskMessages(ctx context.Context, id domain.TaskID) ([]*domain.Message, error) {
	f.record("TaskMessages")
	return f.messages[id], nil
}

func (f *fakeService) TaskEvents(ctx context.Context, id domain.TaskID) ([]domain.Event, error) {
	f.record("TaskEvents")
	return f.events[id], nil
}

func (f *fakeService) TaskDecisions(ctx context.Context, id domain.TaskID) ([]*domain.Decision, error) {
	f.record("TaskDecisions")
	return f.decisions[id], nil
}

func (f *fakeService) Agents(ctx context.Context) ([]domain.Agent, error) {
	f.record("Agents")
	return f.agents, nil
}

func (f *fakeService) Pause(ctx context.Context, id domain.TaskID, reason string) error {
	f.record("Pause")
	f.lastReason = reason
	return nil
}

func (f *fakeService) Resume(ctx context.Context, id domain.TaskID) error {
	f.record("Resume")
	return nil
}

func (f *fakeService) Cancel(ctx context.Context, id domain.TaskID, reason string) error {
	f.record("Cancel")
	f.lastReason = reason
	return nil
}

func (f *fakeService) Approve(ctx context.Context, id domain.TaskID, note string) error {
	f.record("Approve")
	f.lastNote = note
	return nil
}

func (f *fakeService) Reject(ctx context.Context, id domain.TaskID, note string) error {
	f.record("Reject")
	f.lastNote = note
	return nil
}

func (f *fakeService) SendInstruction(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	f.record("SendInstruction")
	f.lastTo = to
	f.lastInstruction = content
	return nil
}

func (f *fakeService) FollowUp(ctx context.Context, id domain.TaskID, to domain.AgentID, content string) error {
	f.record("FollowUp")
	f.lastTo = to
	f.lastInstruction = content
	return nil
}

func (f *fakeService) Models(ctx context.Context) ([]ports.ModelInfo, error) {
	f.record("Models")
	return f.models, nil
}

func (f *fakeService) Recruit(ctx context.Context, in ports.RecruitInput) (domain.Agent, error) {
	f.record("Recruit")
	f.mu.Lock()
	f.lastRecruit = in
	f.recruits = append(f.recruits, in)
	f.mu.Unlock()
	return domain.Agent{ID: in.Name}, nil
}

func (f *fakeService) Dismiss(ctx context.Context, id domain.AgentID) error {
	f.record("Dismiss")
	f.lastTo = id
	return nil
}

func (f *fakeService) CheckModel(ctx context.Context, provider, model string) (ports.ModelInfo, error) {
	f.record("CheckModel")
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.checkResult != nil {
		r := *f.checkResult
		r.Provider, r.ID = provider, model
		return r, nil
	}
	return ports.ModelInfo{Provider: provider, ID: model, Checked: true}, nil
}

func (f *fakeService) TaskAgents(ctx context.Context, id domain.TaskID) ([]domain.Agent, error) {
	return f.Agents(ctx)
}

func (f *fakeService) SetAuthority(ctx context.Context, id domain.TaskID, to domain.AgentID) error {
	f.record("SetAuthority")
	f.lastTo = to
	return nil
}

func (f *fakeService) Backlog(id domain.TaskID) map[domain.AgentID]int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.backlog
}

func (f *fakeService) SearchKnowledge(ctx context.Context, q domain.KnowledgeQuery) ([]domain.KnowledgeHit, error) {
	f.record("SearchKnowledge")
	return nil, ports.ErrNotImplemented
}

func (f *fakeService) Subscribe(h domain.EventHandler) (unsubscribe func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
	return func() {}
}

var _ ports.Service = (*fakeService)(nil)

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

func fixtureAgents() []domain.Agent {
	return []domain.Agent{
		{ID: "opus", DisplayName: "Opus", Kind: domain.AgentKindClaudeCode, Role: domain.RoleAuthority, State: domain.AgentStateWorking},
		{ID: "codex", DisplayName: "Codex", Kind: domain.AgentKindCodex, Role: domain.RoleImplementer, State: domain.AgentStateIdle},
	}
}

func fixtureTask() *domain.Task {
	return &domain.Task{
		ID:             "TASK-001",
		Title:          "Fix authentication cookie bug",
		AuthorityAgent: "opus",
		Participants:   []domain.AgentID{"opus", "codex"},
		State:          domain.TaskDeliberating,
	}
}

func fixtureMessages() []*domain.Message {
	return []*domain.Message{
		{ID: "MSG-1", TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "Investigate cookie handling", Timestamp: time.Now()},
		{ID: "MSG-2", TaskID: "TASK-001", FromAgent: "codex", ToAgent: "opus", Type: domain.MsgAnswer, Content: "Found inconsistent cookie Path", Timestamp: time.Now()},
	}
}

// populatedModel builds a Model with fixture data already loaded, bypassing
// Init/Run so tests can inspect View() and dispatch() synchronously.
// It goes through newModel so the fixture starts in the same state Run()
// produces: a fixture that disagrees with the real constructor tests a screen
// nobody ever sees.
func populatedModel(svc ports.Service) Model {
	m := newModel(context.Background(), svc, Options{})
	m.width, m.height = 100, 40
	m.agents = fixtureAgents()
	m.tasks = []*domain.Task{fixtureTask()}
	m.currentTaskID = "TASK-001"
	m.messages = fixtureMessages()
	m.statusLine = ""
	return m
}

// ---------------------------------------------------------------------------
// Keymap tests
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Create-form tests
//
// These are regression tests. The form used to open with no participants
// selected, so walking through it and pressing enter produced a task whose
// only participant was the authority agent. That task could never make
// progress: the authority had nobody to delegate to, and the run sat idle
// until the timeout paused it.
// ---------------------------------------------------------------------------

// TestTheInspectorShowsTheBackingKind is a regression test. A run against
// scripted mock agents used to look identical on screen to a run against the
// real CLIs. The roster now marks mocks, and the inspector names the CLI.
func TestTheInspectorShowsTheBackingKind(t *testing.T) {
	m := populatedModel(newFakeService()).inspectNow(inspectAgent)
	_, lines := m.inspectorContent(60)
	if !strings.Contains(plainLines(lines), string(domain.AgentKindClaudeCode)) {
		t.Errorf("the agent inspector must show the backing kind, got:\n%s", plainLines(lines))
	}
	if strings.Contains(m.View(), "MOCK AGENTS") {
		t.Error("the mock banner must not appear when agents are real")
	}
}

func TestTheHeaderWarnsWhenEverythingIsAMock(t *testing.T) {
	m := populatedModel(newFakeService())
	m.agents = []domain.Agent{
		{ID: "opus", Role: domain.RoleAuthority, Kind: domain.AgentKindMock, State: domain.AgentStateIdle},
		{ID: "codex", Role: domain.RoleImplementer, Kind: domain.AgentKindMock, State: domain.AgentStateIdle},
	}
	view := m.View()
	if !strings.Contains(view, "MOCK AGENTS") {
		t.Fatalf("running entirely on mocks must be stated on screen, got:\n%s", view)
	}
	if !strings.Contains(view, "-real") {
		t.Error("the banner should tell the user how to switch to real agents")
	}
}

func TestTheMockBannerIsQuietWhenOnlySomeAgentsAreMocks(t *testing.T) {
	m := populatedModel(newFakeService())
	m.agents = []domain.Agent{
		{ID: "opus", Role: domain.RoleAuthority, Kind: domain.AgentKindClaudeCode},
		{ID: "sim", Role: domain.RoleImplementer, Kind: domain.AgentKindMock},
	}
	if strings.Contains(m.View(), "MOCK AGENTS") {
		t.Error("the banner claims everything is mocked; it must only fire when that is true")
	}
}

// plainLines joins rendered lines' plain text, for assertions.
func plainLines(ls []line) string {
	var b strings.Builder
	for _, l := range ls {
		b.WriteString(l.plain())
		b.WriteString("\n")
	}
	return b.String()
}

// The new-task form's team (formteam.go): nothing is a participant until it
// is picked under Recruit, and the authority is ticked by the human.

func TestTheFormStartsWithNoParticipants(t *testing.T) {
	m := populatedModel(newFakeService())
	next, _ := m.dispatch(ActionCreateTask, 0)
	mm := next.(Model)
	if mm.mode != modeCreateForm {
		t.Fatalf("mode = %v, want the create form", mm.mode)
	}
	if n := len(mm.formMembers()); n != 0 {
		t.Errorf("no agent may be a participant before it is recruited, got %d", n)
	}
	if view := mm.renderCreateForm(100); !strings.Contains(view, "none yet") {
		t.Errorf("the form should say how to build the team:\n%s", view)
	}
}

// TestRecruitListsOnlyTheCatalogs: the agents already on the team add no
// entries of their own; every participant is picked by model.
func TestRecruitListsOnlyTheCatalogs(t *testing.T) {
	m := populatedModel(newFakeService())
	m.mode = modeCreateForm
	if entries := m.recruitEntries(); len(entries) != 0 {
		t.Fatalf("with no model lists there is nothing to recruit, got %+v", entries)
	}
}

func TestTheAuthorityIsTickedByTheHumanOnly(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.models = []ports.ModelInfo{{Provider: "claude-code", ID: "opus"}, {Provider: "codex", ID: "gpt-6-sol"}}
	m.mode = modeCreateForm
	m.form = createForm{title: "test", recruit: map[int]int{0: 1, 1: 1}}

	next, cmd := m.submitCreateForm()
	if cmd != nil || svc.callCount("CreateTask") != 0 {
		t.Fatal("a task without an authority must not be submitted")
	}
	if mm := next.(Model); !strings.Contains(mm.statusLine, "authority") || mm.mode != modeCreateForm {
		t.Errorf("the form stays open and says what is missing: %q", mm.statusLine)
	}
	if view := m.renderCreateForm(100); !strings.Contains(view, "Tick one participant as the authority") {
		t.Errorf("the form should ask for an authority:\n%s", view)
	}

	m.form.focus = fieldParticipants
	m.form.cursor = 1 // gpt-6-sol-1
	m, _ = key(m, " ")
	if m.form.authority != "gpt-6-sol-1" {
		t.Fatalf("space ticks the authority, got %q", m.form.authority)
	}
	m.form.cursor = 0
	m, _ = key(m, " ")
	if m.form.authority != "opus-1" {
		t.Fatalf("only one participant can be the authority, got %q", m.form.authority)
	}
	m, _ = key(m, " ")
	if m.form.authority != "" {
		t.Fatalf("ticking it again clears it, got %q", m.form.authority)
	}
	m.form.cursor = 1
	m, _ = key(m, " ")

	_, cmd = m.submitCreateForm()
	if cmd == nil {
		t.Fatal("a valid form should produce a create command")
	}
	cmd()
	if svc.created == nil || svc.created.AuthorityAgent != "gpt-6-sol-1" {
		t.Fatalf("authority = %+v", svc.created)
	}
	if got := fmt.Sprint(svc.created.Participants); got != "[opus-1 gpt-6-sol-1]" {
		t.Errorf("participants = %s (each once, in the agent list's order)", got)
	}
}

func TestATaskNeedsTwoParticipants(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.mode = modeCreateForm
	m.form = createForm{title: "test", recruit: map[int]int{0: 1}, authority: "opus"}
	next, cmd := m.submitCreateForm()
	if cmd != nil || svc.callCount("CreateTask") != 0 {
		t.Fatal("an authority-only task must not be submitted: it has nobody to delegate to")
	}
	if mm := next.(Model); !strings.Contains(mm.statusLine, "at least two") {
		t.Errorf("status = %q", mm.statusLine)
	}
}

func TestKeymapMapping(t *testing.T) {
	cases := []struct {
		key  string
		want Action
	}{
		{"n", ActionCreateTask},
		{"p", ActionPauseTask},
		{"r", ActionResumeTask},
		{"c", ActionCancelTask},
		{"a", ActionApprove},
		{"x", ActionReject},
		{"i", ActionSendInstruction},
		{"d", ActionInspectDecisions},
		{"e", ActionInspectEvents},
		{"tab", ActionNextTab},
		{"shift+tab", ActionPrevTab},
		{"}", ActionNextTask},
		{"{", ActionPrevTask},
		{"esc", ActionBack},
		{"up", ActionScrollUp},
		{"k", ActionScrollUp},
		{"down", ActionScrollDown},
		{"j", ActionScrollDown},
		{"?", ActionHelp},
		{"q", ActionQuit},
		{"ctrl+c", ActionQuit},
	}
	for _, c := range cases {
		got, _, ok := lookupAction(c.key)
		if !ok {
			t.Errorf("key %q: expected a binding, found none", c.key)
			continue
		}
		if got != c.want {
			t.Errorf("key %q: got action %q, want %q", c.key, got, c.want)
		}
	}

	if _, _, ok := lookupAction("z"); ok {
		t.Errorf("key %q: expected no binding", "z")
	}
}

// TestFocusKeysSwitchToSingleAgentViewAndBack covers the new full-screen
// agent focus feature: "1"/"2" focus the Nth agent, and "0"/"esc" return to
// the split view.
func TestFocusKeysSwitchToSingleAgentViewAndBack(t *testing.T) {
	m := populatedModel(newFakeService())
	if m.focusedAgent != -1 {
		t.Fatalf("expected split view by default, focusedAgent = %d", m.focusedAgent)
	}

	action, arg, ok := lookupAction("2")
	if !ok || action != ActionFocusAgent || arg != 2 {
		t.Fatalf("key %q: got (%v, %d, %v), want (ActionFocusAgent, 2, true)", "2", action, arg, ok)
	}
	next, _ := m.dispatch(action, arg)
	mm := next.(Model)
	if mm.focusedAgent != 1 {
		t.Fatalf("focusedAgent = %d, want 1 (0-based index of agent 2)", mm.focusedAgent)
	}
	id, ok := mm.focusedAgentID()
	if !ok || id != mm.agents[1].ID {
		t.Fatalf("focusedAgentID() = (%v, %v), want (%v, true)", id, ok, mm.agents[1].ID)
	}
	view := mm.View()
	if !strings.Contains(view, "FOCUSED") || !strings.Contains(view, string(mm.agents[1].ID)) {
		t.Errorf("focused view should show the focused agent, got:\n%s", view)
	}

	action, arg, ok = lookupAction("0")
	if !ok || action != ActionFocusAgent || arg != 0 {
		t.Fatalf("key %q: got (%v, %d, %v), want (ActionFocusAgent, 0, true)", "0", action, arg, ok)
	}
	next, _ = mm.dispatch(action, arg)
	mm = next.(Model)
	if mm.focusedAgent != -1 {
		t.Fatalf("expected '0' to return to split view, focusedAgent = %d", mm.focusedAgent)
	}

	// esc behaves the same way once focused. (The fixture has only two
	// agents, so focus agent 1 rather than 3 here.)
	next, _ = mm.dispatch(ActionFocusAgent, 1)
	mm = next.(Model)
	if mm.focusedAgent != 0 {
		t.Fatalf("focusedAgent = %d, want 0", mm.focusedAgent)
	}
	action, arg, ok = lookupAction("esc")
	if !ok || action != ActionBack {
		t.Fatalf("key %q: got (%v, %d, %v), want (ActionBack, 0, true)", "esc", action, arg, ok)
	}
	next, _ = mm.dispatch(action, arg)
	mm = next.(Model)
	if mm.focusedAgent != -1 {
		t.Fatalf("expected 'esc' to return to split view, focusedAgent = %d", mm.focusedAgent)
	}
}

// TestFocusAgentOutOfRangeClearsFocus ensures an out-of-range index (e.g. "9"
// pressed with only two agents registered) is treated as "no such agent",
// not a panic or a stuck focus.
func TestFocusAgentOutOfRangeClearsFocus(t *testing.T) {
	m := populatedModel(newFakeService())
	next, _ := m.dispatch(ActionFocusAgent, 9)
	mm := next.(Model)
	if mm.focusedAgent != -1 {
		t.Fatalf("focusedAgent = %d, want -1 for an out-of-range focus target", mm.focusedAgent)
	}
}

// ---------------------------------------------------------------------------
// View tests
// ---------------------------------------------------------------------------

func TestViewPopulatedContainsExpectedContent(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)

	out := m.View()

	want := []string{
		"TASK-001",
		"opus",
		"codex",
		"authority",
		"implementer",
		string(domain.MsgDelegate),
		string(domain.MsgAnswer),
		string(domain.TaskDeliberating),
		"Fix authentication cookie bug",
		"Investigate cookie handling",
	}
	for _, s := range want {
		if !strings.Contains(out, s) {
			t.Errorf("View() output missing %q\n--- output ---\n%s", s, out)
		}
	}
}

func TestViewEmptyModelDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("View() panicked on empty model: %v", r)
		}
	}()
	var m Model
	m.svc = newFakeService()
	m.ctx = context.Background()
	_ = m.View()
}

func TestViewZeroWindowSizeDoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("View() panicked on zero window size: %v", r)
		}
	}()
	svc := newFakeService()
	m := populatedModel(svc)
	m.width = 0
	m.height = 0
	_ = m.View()
}

// ---------------------------------------------------------------------------
// dispatch tests
// ---------------------------------------------------------------------------

func TestDispatchHumanOverridesCallService(t *testing.T) {
	cases := []struct {
		name   string
		action Action
		call   string
	}{
		{"pause", ActionPauseTask, "Pause"},
		{"resume", ActionResumeTask, "Resume"},
		{"cancel", ActionCancelTask, "Cancel"},
		{"approve", ActionApprove, "Approve"},
		{"reject", ActionReject, "Reject"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc := newFakeService()
			m := populatedModel(svc)

			newModel, cmd := m.dispatch(c.action, 0)
			if cmd == nil {
				t.Fatalf("dispatch(%s) returned a nil tea.Cmd", c.action)
			}
			_ = newModel

			msg := cmd()
			res, ok := msg.(actionResultMsg)
			if !ok {
				t.Fatalf("dispatch(%s) cmd produced %T, want actionResultMsg", c.action, msg)
			}
			if res.err != nil {
				t.Fatalf("dispatch(%s) cmd returned error: %v", c.action, res.err)
			}
			if got := svc.callCount(c.call); got != 1 {
				t.Errorf("dispatch(%s): expected fakeService.%s to be called once, got %d", c.action, c.call, got)
			}
		})
	}
}

func TestDispatchWithNoCurrentTaskDoesNotCallService(t *testing.T) {
	svc := newFakeService()
	m := Model{ctx: context.Background(), svc: svc}

	newModel, cmd := m.dispatch(ActionPauseTask, 0)
	if cmd != nil {
		t.Fatalf("dispatch(pause) with no current task should return a nil cmd")
	}
	mm := newModel.(Model)
	if mm.statusLine == "" {
		t.Errorf("expected a status message explaining there is no task selected")
	}
}

func TestDispatchQuit(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	newModel, cmd := m.dispatch(ActionQuit, 0)
	if cmd == nil {
		t.Fatalf("dispatch(quit) returned nil cmd")
	}
	if msg := cmd(); msg != tea.Quit() {
		// tea.Quit returns a QuitMsg; comparing via the produced message type.
		if _, ok := msg.(tea.QuitMsg); !ok {
			t.Errorf("dispatch(quit) cmd did not produce a QuitMsg, got %T", msg)
		}
	}
	if !newModel.(Model).quitting {
		t.Errorf("expected quitting to be set")
	}
}

func TestDAndEJumpToTheDecisionsAndEventsViews(t *testing.T) {
	m := populatedModel(newFakeService())

	newModel, _ := m.dispatch(ActionInspectDecisions, 0)
	mm := newModel.(Model)
	if mm.centerOrDefault() != tabDecisions {
		t.Errorf("d should show the decisions view, got %s", mm.centerOrDefault())
	}
	newModel, _ = mm.dispatch(ActionInspectEvents, 0)
	mm = newModel.(Model)
	if mm.centerOrDefault() != tabEvents {
		t.Errorf("e should show the events view, got %s", mm.centerOrDefault())
	}
}

// ---------------------------------------------------------------------------
// Event -> Update tests
// ---------------------------------------------------------------------------

func TestDomainEventUpdatesModel(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)

	ev := domain.Event{
		ID:        "EVT-1",
		Type:      domain.EvtAgentMessageSent,
		TaskID:    "TASK-001",
		Timestamp: time.Now(),
		Summary:   "opus -> codex DELEGATE",
	}

	newModel, _ := m.Update(domainEventMsg(ev))
	mm := newModel.(Model)

	if len(mm.globalEvents) != 1 {
		t.Fatalf("expected 1 global event recorded, got %d", len(mm.globalEvents))
	}
	if mm.globalEvents[0].ID != ev.ID {
		t.Errorf("expected event log to contain the dispatched event")
	}

	// The events view shows the task's audit log.
	mm.events = append(mm.events, ev)
	mm.center = tabEvents
	out := mm.View()
	if !strings.Contains(out, "DELEGATE") {
		t.Errorf("expected event log pane to render the event summary; got:\n%s", out)
	}
}

func TestDomainEventForCurrentTaskTriggersDetailReload(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)

	ev := domain.Event{ID: "EVT-2", Type: domain.EvtAgentMessageSent, TaskID: "TASK-001", Timestamp: time.Now()}
	_, cmd := m.Update(domainEventMsg(ev))
	if cmd == nil {
		t.Fatalf("expected a reload command when an event targets the current task")
	}
}

// ---------------------------------------------------------------------------
// Sanity: Update handles WindowSizeMsg without panic and stores dimensions.
// ---------------------------------------------------------------------------

func TestUpdateWindowSize(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	newModel, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 50})
	mm := newModel.(Model)
	if mm.width != 120 || mm.height != 50 {
		t.Errorf("expected window size to be stored, got %dx%d", mm.width, mm.height)
	}
}

// ---------------------------------------------------------------------------
// Live agent activity tests
//
// AgentActivity is ephemeral: it never reaches TaskEvents/TaskMessages, so
// these behaviours can only be exercised through the live event stream
// (domainEventMsg), exactly as the running program receives them.
// ---------------------------------------------------------------------------

func activityEvent(agent domain.AgentID, kind, text string) domain.Event {
	return domain.Event{
		ID:      "EVT-ACT",
		Type:    domain.EvtAgentActivity,
		TaskID:  "TASK-001",
		AgentID: agent,
		Summary: text,
		Details: map[string]string{"kind": kind},
	}
}

// TestAgentActivityTextFragmentsConcatenate is the core streaming behaviour:
// per ports.AgentActivity's contract, "text" and "thinking" chunks are
// partial sentences arriving ~80ms apart, not whole lines, so consecutive
// chunks of the same kind must merge into one growing entry rather than
// becoming separate lines.
func TestAgentActivityTextFragmentsConcatenate(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)

	m1, _ := m.Update(domainEventMsg(activityEvent("opus", "text", "The cookie ")))
	mm := m1.(Model)
	// Any Cmd here would be startTickerIfNeeded's spinner tick (opus is
	// WORKING in the fixture) -- never a Service call, which is what
	// actually matters: activity must never touch Service.
	if svc.callCount("TaskMessages") != 0 || svc.callCount("TaskEvents") != 0 {
		t.Fatalf("AgentActivity must not trigger a task-detail reload")
	}

	m2, _ := mm.Update(domainEventMsg(activityEvent("opus", "text", "path differs.")))
	mm = m2.(Model)

	buf, ok := mm.activity["opus"]
	if !ok {
		t.Fatalf("expected an activity buffer for opus")
	}
	if len(buf.entries) != 1 {
		t.Fatalf("expected the two text fragments to merge into one entry, got %d entries: %+v", len(buf.entries), buf.entries)
	}
	if want := "The cookie path differs."; buf.entries[0].text != want {
		t.Errorf("merged text = %q, want %q", buf.entries[0].text, want)
	}

	// The event must not leak into the durable/global event log either.
	if len(mm.globalEvents) != 0 {
		t.Errorf("AgentActivity must not be recorded in globalEvents, got %d", len(mm.globalEvents))
	}
}

// TestAgentActivityToolAndStatusAreDiscreteEntries covers the other half of
// the contract: "tool" and "status" chunks are discrete events, never
// merged with each other or with surrounding text.
func TestAgentActivityToolAndStatusAreDiscreteEntries(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)

	m1, _ := m.Update(domainEventMsg(activityEvent("opus", "text", "looking around")))
	m2, _ := m1.(Model).Update(domainEventMsg(activityEvent("opus", "tool", "Read internal/auth.go")))
	m3, _ := m2.(Model).Update(domainEventMsg(activityEvent("opus", "status", "turn completed")))
	mm := m3.(Model)

	buf := mm.activity["opus"]
	if len(buf.entries) != 3 {
		t.Fatalf("expected 3 discrete entries (text, tool, status), got %d: %+v", len(buf.entries), buf.entries)
	}
	if buf.entries[1].kind != ports.ActivityTool || buf.entries[1].text != "Read internal/auth.go" {
		t.Errorf("entry[1] = %+v, want a tool entry with the tool name", buf.entries[1])
	}
	if buf.entries[2].kind != ports.ActivityStatus || buf.entries[2].text != "turn completed" {
		t.Errorf("entry[2] = %+v, want a status entry", buf.entries[2])
	}
}

// TestAgentActivityUnknownAgentIsIgnored ensures a race between an adapter
// starting to stream and Agents() having registered it does not panic or
// otherwise corrupt the model.
func TestAgentActivityUnknownAgentIsIgnored(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("AgentActivity for an unknown agent panicked: %v", r)
		}
	}()
	svc := newFakeService()
	m := populatedModel(svc)

	next, _ := m.Update(domainEventMsg(activityEvent("ghost-agent", "text", "hello")))
	mm := next.(Model)
	if _, ok := mm.activity["ghost-agent"]; ok {
		t.Error("activity for an unregistered agent should be dropped, not buffered")
	}
}

// TestAgentActivityRingBufferIsBounded pushes far more activity than any
// real run would produce and checks memory stays bounded, per
// maxActivityEntries/maxActivityBytes.
func TestAgentActivityRingBufferIsBounded(t *testing.T) {
	var buf agentActivity
	for i := 0; i < 10000; i++ {
		// Alternate kinds so entries cannot all merge into one; this is the
		// worst case for the entry-count cap.
		kind := "tool"
		if i%2 == 0 {
			kind = "status"
		}
		buf.append(ports.ActivityKind(kind), fmt.Sprintf("event number %d with some padding text", i))
	}
	if len(buf.entries) > maxActivityEntries {
		t.Fatalf("entries = %d, want <= %d", len(buf.entries), maxActivityEntries)
	}
	total := 0
	for _, e := range buf.entries {
		total += len(e.text)
	}
	if total > maxActivityBytes {
		t.Fatalf("total buffered bytes = %d, want <= %d", total, maxActivityBytes)
	}
	// The most recent event must have survived the trim.
	last := buf.entries[len(buf.entries)-1]
	if last.text != "event number 9999 with some padding text" {
		t.Errorf("last entry = %q, want the most recent event", last.text)
	}
}

// TestAgentActivityRingBufferBoundsASingleGrowingEntry covers the case the
// entry-count cap alone cannot: one agent that only ever emits "text", so
// every chunk merges into a single ever-growing entry between discrete
// boundaries.
func TestAgentActivityRingBufferBoundsASingleGrowingEntry(t *testing.T) {
	var buf agentActivity
	for i := 0; i < 10000; i++ {
		buf.append(ports.ActivityText, "some more text ")
	}
	if len(buf.entries) != 1 {
		t.Fatalf("expected all fragments to merge into one entry, got %d", len(buf.entries))
	}
	if n := len([]rune(buf.entries[0].text)); n > maxEntryRunes {
		t.Fatalf("entry length = %d runes, want <= %d", n, maxEntryRunes)
	}
}

// TestViewWithActivityShowsRecentText is the end-to-end check that live
// activity actually reaches the screen: an AgentActivity event followed by
// View() must show the agent's latest text.
func TestViewWithActivityShowsRecentText(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)

	next, _ := m.Update(domainEventMsg(activityEvent("opus", "thinking", "the cookie path differs between environments")))
	mm := next.(Model)
	next, _ = mm.Update(domainEventMsg(activityEvent("codex", "status", "working on DELEGATE from opus")))
	mm = next.(Model)

	// The roster shows what a working agent is doing now...
	if out := mm.View(); !strings.Contains(out, "cookie path differs") {
		t.Errorf("the roster should show opus's live activity, got:\n%s", out)
	}
	// ...and the activity view shows everyone's output.
	mm.center = tabActivity
	out := mm.View()
	if !strings.Contains(out, "cookie path differs") || !strings.Contains(out, "working on DELEGATE") {
		t.Errorf("the activity view should show both agents' output, got:\n%s", out)
	}
}

// TestViewDoesNotPanicAcrossSizesAndAgentCounts exercises the layout's
// degrade paths: zero size (before the first WindowSizeMsg), a narrow
// terminal (forces the stacked-panes fallback), and five simultaneous
// agents (more than the create-form/demo fixtures ever use).
func TestViewDoesNotPanicAcrossSizesAndAgentCounts(t *testing.T) {
	mkAgents := func(n int) []domain.Agent {
		var agents []domain.Agent
		for i := 0; i < n; i++ {
			agents = append(agents, domain.Agent{
				ID:    domain.AgentID(fmt.Sprintf("agent-%d", i)),
				Role:  domain.RoleImplementer,
				Kind:  domain.AgentKindMock,
				State: domain.AgentStateWorking,
			})
		}
		return agents
	}

	cases := []struct {
		name          string
		width, height int
		agents        []domain.Agent
	}{
		{"zero size", 0, 0, fixtureAgents()},
		{"40 columns", 40, 24, fixtureAgents()},
		{"five agents", 120, 40, mkAgents(5)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("View() panicked: %v", r)
				}
			}()
			svc := newFakeService()
			m := populatedModel(svc)
			m.width, m.height = c.width, c.height
			m.agents = c.agents
			for _, a := range c.agents {
				m.activityFor(a.ID).append(ports.ActivityStatus, "working on something")
			}
			_ = m.View()
		})
	}
}
