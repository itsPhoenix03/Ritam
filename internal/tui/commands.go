package tui

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// Action is a user-triggered intent, decoupled from the specific key that
// produced it. All key handling funnels through Action so the mapping from
// keys to behaviour lives in exactly one place (see keymap below) instead of
// being scattered across the view/update code.
type Action string

const (
	ActionCreateTask   Action = "create_task"
	ActionPauseTask    Action = "pause_task"
	ActionSetAuthority Action = "set_authority"
	ActionRecruit      Action = "recruit"
	// ActionSetAccess is the confirmation prompt for raising the access
	// level. It is never bound to a key.
	ActionSetAccess        Action = "set_access"
	ActionResumeTask       Action = "resume_task"
	ActionCancelTask       Action = "cancel_task"
	ActionApprove          Action = "approve"
	ActionReject           Action = "reject"
	ActionSendInstruction  Action = "send_instruction"
	ActionInspectDecisions Action = "inspect_decisions"
	ActionInspectEvents    Action = "inspect_events"
	ActionNextTask         Action = "next_task"
	ActionPrevTask         Action = "prev_task"
	ActionScrollUp         Action = "scroll_up"
	ActionScrollDown       Action = "scroll_down"
	ActionPageUp           Action = "page_up"
	ActionPageDown         Action = "page_down"
	ActionScrollTop        Action = "scroll_top"
	ActionScrollBottom     Action = "scroll_bottom"
	// ActionNextPane/ActionPrevPane move the scroll target between visible
	// panes. Without them only one region of the screen would be reachable.
	ActionNextPane Action = "next_pane"
	ActionPrevPane Action = "prev_pane"
	ActionQuit     Action = "quit"
	ActionHelp     Action = "help"

	// ActionNextTab/ActionPrevTab move the centre column between its tabs.
	// They are distinct from the pane actions: panes are regions that are all
	// on screen at once, tabs are alternatives for one region.
	ActionNextTab Action = "next_tab"
	ActionPrevTab Action = "prev_tab"
	// ActionCursorUp/ActionCursorDown move the agent-roster cursor, which is
	// also what the inspector follows.
	ActionCursorUp   Action = "cursor_up"
	ActionCursorDown Action = "cursor_down"
	// ActionInspect points the inspector at the agent under the cursor and
	// makes sure the inspector is visible.
	ActionInspect Action = "inspect"
	// ActionToggleFollow unpins the stream so a run that is still producing
	// output can be scrolled back through.
	ActionToggleFollow Action = "toggle_follow"
	// ActionToggleThinking filters the agents' scratch reasoning out of the
	// merged stream.
	ActionToggleThinking  Action = "toggle_thinking"
	ActionToggleInspector Action = "toggle_inspector"
	ActionToggleHelpBar   Action = "toggle_help_bar"
	// ActionCommand opens the ":" command line; ActionFilter opens it
	// pre-filled with the filter verb, which is the one completion worth a
	// dedicated key.
	ActionCommand Action = "command"
	// ActionFollowUp is the result action of a follow-up that reopened a
	// completed task. It is never bound to a key: `i` on a completed task
	// produces it.
	ActionFollowUp Action = "follow_up"
	ActionFilter   Action = "filter"
	// ActionBack is esc: leave the zoomed inspector or the focused agent, or
	// hand keyboard focus back to the conversation.
	ActionBack Action = "back"

	// ActionFocusAgent expands one agent's pane to fill the whole body, or
	// returns to the split view. Its keyBinding.Arg carries the 1-based
	// agent index to focus, or 0 to clear focus -- Action alone cannot carry
	// "which agent", so this is the one action that consults Arg.
	ActionFocusAgent Action = "focus_agent"

	// Internal navigation actions used while a form/prompt is active. They
	// are not part of the global keymap table (they only apply in their own
	// mode) but are still funnelled through Action/dispatch for consistency.
	ActionSubmit Action = "submit"
	ActionCancel Action = "cancel_input"
)

// keyBinding pairs a literal key (as produced by tea.KeyMsg.String()) with
// the Action it triggers in normal mode, plus a short help label.
type keyBinding struct {
	Key    string
	Action Action
	Help   string
	// Arg is auxiliary data dispatch needs alongside the Action. Only
	// ActionFocusAgent uses it today (see its doc comment).
	Arg int
}

// keymap is the single source of truth for normal-mode key handling. Both
// the dispatcher and the generated help line read from this table.
var keymap = []keyBinding{
	{Key: "n", Action: ActionCreateTask, Help: "new task"},
	{Key: "p", Action: ActionPauseTask, Help: "pause"},
	{Key: "r", Action: ActionResumeTask, Help: "resume"},
	{Key: "c", Action: ActionCancelTask, Help: "cancel"},
	{Key: "a", Action: ActionApprove, Help: "approve"},
	{Key: "x", Action: ActionReject, Help: "reject"},
	{Key: "i", Action: ActionSendInstruction, Help: "instruct"},
	{Key: "tab", Action: ActionNextTab, Help: "next view: conversation, activity, events, decisions (shift+tab back)"},
	{Key: "shift+tab", Action: ActionPrevTab},
	{Key: "d", Action: ActionInspectDecisions, Help: "decisions view"},
	{Key: "e", Action: ActionInspectEvents, Help: "events view (the audit log)"},
	{Key: "}", Action: ActionNextTask, Help: "next task ({ previous)"},
	{Key: "{", Action: ActionPrevTask},
	{Key: "up", Action: ActionScrollUp, Help: "move up: selection in a list, scroll elsewhere (k)"},
	{Key: "k", Action: ActionScrollUp},
	{Key: "down", Action: ActionScrollDown, Help: "move down: selection in a list, scroll elsewhere (j)"},
	{Key: "j", Action: ActionScrollDown},
	{Key: "pgup", Action: ActionPageUp, Help: "page up"},
	{Key: "pgdown", Action: ActionPageDown, Help: "page down"},
	{Key: "home", Action: ActionScrollTop, Help: "oldest"},
	{Key: "g", Action: ActionScrollTop},
	{Key: "end", Action: ActionScrollBottom, Help: "newest"},
	{Key: "G", Action: ActionScrollBottom},
	{Key: "]", Action: ActionNextPane, Help: "next pane: tasks, centre, agents, decision, inspector ([ prev)"},
	{Key: "[", Action: ActionPrevPane},
	{Key: ">", Action: ActionNextTab},
	{Key: "<", Action: ActionPrevTab},
	{Key: "K", Action: ActionCursorUp, Help: "move the agent selection from any pane (J down)"},
	{Key: "J", Action: ActionCursorDown},
	{Key: "enter", Action: ActionInspect, Help: "open the selection in the inspector; in the inspector, zoom"},
	{Key: "f", Action: ActionToggleFollow, Help: "follow the newest line"},
	{Key: "t", Action: ActionToggleThinking, Help: "show/hide agent thinking"},
	{Key: "v", Action: ActionToggleInspector, Help: "zoom the inspector to the whole screen, and back"},
	{Key: "h", Action: ActionToggleHelpBar, Help: "help bar"},
	{Key: ":", Action: ActionCommand, Help: "command line"},
	{Key: "/", Action: ActionFilter, Help: "filter the stream by agent"},
	{Key: "1", Action: ActionFocusAgent, Help: "focus agent 1..9 / 0,esc: back to split view", Arg: 1},
	{Key: "2", Action: ActionFocusAgent, Arg: 2},
	{Key: "3", Action: ActionFocusAgent, Arg: 3},
	{Key: "4", Action: ActionFocusAgent, Arg: 4},
	{Key: "5", Action: ActionFocusAgent, Arg: 5},
	{Key: "6", Action: ActionFocusAgent, Arg: 6},
	{Key: "7", Action: ActionFocusAgent, Arg: 7},
	{Key: "8", Action: ActionFocusAgent, Arg: 8},
	{Key: "9", Action: ActionFocusAgent, Arg: 9},
	{Key: "0", Action: ActionFocusAgent, Arg: 0},
	{Key: "esc", Action: ActionBack, Help: "back: leave zoom or a focused agent"},
	{Key: "?", Action: ActionHelp, Help: "help"},
	{Key: "q", Action: ActionQuit, Help: "quit"},
	{Key: "ctrl+c", Action: ActionQuit},
}

// isScrollAction reports whether an action only moves a viewport. The help
// overlay uses it to keep the scroll keys live while treating every other key
// as "close".
func isScrollAction(a Action) bool {
	switch a {
	case ActionScrollUp, ActionScrollDown, ActionPageUp, ActionPageDown,
		ActionScrollTop, ActionScrollBottom:
		return true
	}
	return false
}

// lookupAction resolves a raw key string to the Action bound to it, along
// with any Arg the binding carries.
func lookupAction(key string) (Action, int, bool) {
	for _, kb := range keymap {
		if kb.Key == key {
			return kb.Action, kb.Arg, true
		}
	}
	return "", 0, false
}

// ---------------------------------------------------------------------------
// Result messages. Every tea.Cmd below returns exactly one of these.
// ---------------------------------------------------------------------------

// agentsLoadedMsg carries the result of Service.Agents.
type agentsLoadedMsg struct {
	agents []domain.Agent
	err    error
}

// tasksLoadedMsg carries the result of Service.ListTasks.
type tasksLoadedMsg struct {
	tasks []*domain.Task
	err   error
}

// taskDetailMsg carries the messages/events/decisions of one task, fetched
// together so the conversation, event-log and decisions panes stay in sync.
type taskDetailMsg struct {
	taskID    domain.TaskID
	messages  []*domain.Message
	events    []domain.Event
	decisions []*domain.Decision
	// backlog is how many messages wait undelivered for each agent.
	backlog map[domain.AgentID]int
	err     error
}

// taskCreatedMsg carries the result of creating (and starting) a new task.
type taskCreatedMsg struct {
	task *domain.Task
	err  error
}

// actionResultMsg carries the result of a human-override call (pause,
// resume, cancel, approve, reject, send-instruction).
type actionResultMsg struct {
	action Action
	taskID domain.TaskID
	err    error
}

// domainEventMsg wraps a domain.Event delivered by svc.Subscribe.
type domainEventMsg domain.Event

// tickMsg drives the animated "working" spinner. It is only ever scheduled
// while at least one agent is WORKING (see Model.startTickerIfNeeded), so an
// idle UI does not keep repainting itself forever.
type tickMsg time.Time

// spinnerInterval is fast enough to look alive without generating more
// redraw churn than three simultaneously streaming agents already do.
const spinnerInterval = 120 * time.Millisecond

// clockInterval is the tick rate when nothing is animating: the header's
// elapsed clock and the throughput decay only change once a second, so waking
// the renderer 8 times a second for them is pure waste.
const clockInterval = time.Second

// tickInterval is the tick rate for the current screen: the spinner needs a
// fast tick, the clock does not.
func tickInterval(working bool) time.Duration {
	if working {
		return spinnerInterval
	}
	return clockInterval
}

// tickCmd schedules the next tick.
func tickCmd(working bool) tea.Cmd {
	return tea.Tick(tickInterval(working), func(t time.Time) tea.Msg { return tickMsg(t) })
}

// ---------------------------------------------------------------------------
// tea.Cmd constructors. Each runs off the UI goroutine and talks to
// ports.Service only; none of them touch Model directly.
// ---------------------------------------------------------------------------

// loadAgentsCmd loads the agents as they stand on the selected task (each
// task has its own instance of each agent), or overall when none is.
func loadAgentsCmd(ctx context.Context, svc ports.Service, task domain.TaskID) tea.Cmd {
	return func() tea.Msg {
		var agents []domain.Agent
		var err error
		if task != "" {
			agents, err = svc.TaskAgents(ctx, task)
		} else {
			agents, err = svc.Agents(ctx)
		}
		return agentsLoadedMsg{agents: agents, err: err}
	}
}

func loadTasksCmd(ctx context.Context, svc ports.Service) tea.Cmd {
	return func() tea.Msg {
		tasks, err := svc.ListTasks(ctx)
		return tasksLoadedMsg{tasks: tasks, err: err}
	}
}

// loadTaskDetailCmd fetches everything the conversation/decisions/event-log
// panes need for one task in a single round trip through the Cmd pipeline.
func loadTaskDetailCmd(ctx context.Context, svc ports.Service, id domain.TaskID) tea.Cmd {
	return func() tea.Msg {
		msgs, err := svc.TaskMessages(ctx, id)
		if err != nil {
			return taskDetailMsg{taskID: id, err: err}
		}
		evs, err := svc.TaskEvents(ctx, id)
		if err != nil {
			return taskDetailMsg{taskID: id, err: err}
		}
		decs, err := svc.TaskDecisions(ctx, id)
		if err != nil {
			return taskDetailMsg{taskID: id, err: err}
		}
		return taskDetailMsg{taskID: id, messages: msgs, events: evs, decisions: decs, backlog: svc.Backlog(id)}
	}
}

// createTaskWithRecruitsCmd recruits the agents chosen in the form, adds them
// to the task's participants, then creates and starts the task.
func createTaskWithRecruitsCmd(ctx context.Context, svc ports.Service, in ports.CreateTaskInput, recruits []ports.RecruitInput) tea.Cmd {
	return func() tea.Msg {
		for _, r := range recruits {
			a, err := svc.Recruit(ctx, r)
			if err != nil {
				return taskCreatedMsg{err: fmt.Errorf("recruiting %s: %w", r.Name, err)}
			}
			in.Participants = append(in.Participants, a.ID)
		}
		t, err := svc.CreateTask(ctx, in)
		if err != nil {
			return taskCreatedMsg{err: err}
		}
		if err := svc.RunTask(ctx, t.ID); err != nil {
			return taskCreatedMsg{task: t, err: err}
		}
		return taskCreatedMsg{task: t}
	}
}

// createTaskCmd creates a task and immediately asks the service to run it,
// mirroring what a human expects from "new task": submit and go.
func createTaskCmd(ctx context.Context, svc ports.Service, in ports.CreateTaskInput) tea.Cmd {
	return func() tea.Msg {
		t, err := svc.CreateTask(ctx, in)
		if err != nil {
			return taskCreatedMsg{err: err}
		}
		if err := svc.RunTask(ctx, t.ID); err != nil {
			return taskCreatedMsg{task: t, err: err}
		}
		return taskCreatedMsg{task: t}
	}
}

func pauseCmd(ctx context.Context, svc ports.Service, id domain.TaskID, reason string) tea.Cmd {
	return func() tea.Msg {
		err := svc.Pause(ctx, id, reason)
		return actionResultMsg{action: ActionPauseTask, taskID: id, err: err}
	}
}

func setAuthorityCmd(ctx context.Context, svc ports.Service, id domain.TaskID, to domain.AgentID) tea.Cmd {
	return func() tea.Msg {
		err := svc.SetAuthority(ctx, id, to)
		return actionResultMsg{action: ActionSetAuthority, taskID: id, err: err}
	}
}

func resumeCmd(ctx context.Context, svc ports.Service, id domain.TaskID) tea.Cmd {
	return func() tea.Msg {
		err := svc.Resume(ctx, id)
		return actionResultMsg{action: ActionResumeTask, taskID: id, err: err}
	}
}

func cancelCmd(ctx context.Context, svc ports.Service, id domain.TaskID, reason string) tea.Cmd {
	return func() tea.Msg {
		err := svc.Cancel(ctx, id, reason)
		return actionResultMsg{action: ActionCancelTask, taskID: id, err: err}
	}
}

func approveCmd(ctx context.Context, svc ports.Service, id domain.TaskID, note string) tea.Cmd {
	return func() tea.Msg {
		err := svc.Approve(ctx, id, note)
		return actionResultMsg{action: ActionApprove, taskID: id, err: err}
	}
}

func rejectCmd(ctx context.Context, svc ports.Service, id domain.TaskID, note string) tea.Cmd {
	return func() tea.Msg {
		err := svc.Reject(ctx, id, note)
		return actionResultMsg{action: ActionReject, taskID: id, err: err}
	}
}

// followUpCmd reopens a completed task with a message to one agent.
func followUpCmd(ctx context.Context, svc ports.Service, id domain.TaskID, to domain.AgentID, content string) tea.Cmd {
	return func() tea.Msg {
		err := svc.FollowUp(ctx, id, to, content)
		return actionResultMsg{action: ActionFollowUp, taskID: id, err: err}
	}
}

func sendInstructionCmd(ctx context.Context, svc ports.Service, id domain.TaskID, to domain.AgentID, content string) tea.Cmd {
	return func() tea.Msg {
		err := svc.SendInstruction(ctx, id, to, content)
		return actionResultMsg{action: ActionSendInstruction, taskID: id, err: err}
	}
}
