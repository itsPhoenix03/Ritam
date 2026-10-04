package tui

import (
	"github.com/charmbracelet/lipgloss"

	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// Colours are the Campbell 16-colour ANSI palette, addressed by index so the
// UI renders correctly in plain Windows Terminal / PowerShell consoles
// without relying on truecolor support. See theme.go for the index table and
// for the meaning assigned to each hue.
//
// The assignment is deliberately narrow: bright cyan means focus/chair,
// bright green means working/approve, bright yellow means pending/usage,
// bright red means error/reject, bright black is chrome. Anything that does
// not fit one of those meanings stays white.
var (
	colorGreen   = lipgloss.Color(ansiBrGreen)
	colorYellow  = lipgloss.Color(ansiBrYellow)
	colorRed     = lipgloss.Color(ansiBrRed)
	colorGray    = lipgloss.Color(ansiBrBlack)
	colorCyan    = lipgloss.Color(ansiBrCyan)
	colorMagenta = lipgloss.Color(ansiMagenta)
	colorWhite   = lipgloss.Color(ansiWhite)
	colorBright  = lipgloss.Color(ansiBrWhite)
)

var (
	// The product mark is a filled block: at 16 colours a background fill is
	// the only thing loud enough to read as chrome rather than as content.
	//
	// It carries no lipgloss padding on purpose. Padding is invisible to
	// line.width(), so a padded style inside a measured row makes the row two
	// columns wider than the layout believes it is -- which is exactly how a
	// three-column body shears. Callers pass the spaces themselves.
	titleStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipgloss.Color(ansiBlack)).
			Background(colorCyan)

	dividerStyle = lipgloss.NewStyle().Foreground(colorGray)

	sectionHeaderStyle = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)

	// tabActiveStyle marks the selected centre tab. Its brackets carry the
	// meaning under NO_COLOR; the fill is for everyone else.
	tabActiveStyle = lipgloss.NewStyle().Bold(true).Foreground(colorCyan)

	errorStyle = lipgloss.NewStyle().Foreground(colorRed).Bold(true)

	dimStyle = lipgloss.NewStyle().Foreground(colorGray)

	bodyStyle = lipgloss.NewStyle().Foreground(colorWhite)

	emphStyle = lipgloss.NewStyle().Foreground(colorBright)

	labelStyle = lipgloss.NewStyle().Bold(true)

	// reverseStyle is the one and only selection treatment. With six agent
	// colours already in play, marking a selected row by recolouring it would
	// collide with an agent identity; reverse video never can.
	reverseStyle = lipgloss.NewStyle().Reverse(true)

	// Badges are inverted chips used for states that demand attention: the
	// task's run state in the header, WRITE-ENABLED in the inspector, the
	// approve/reject affordances on the decision gate.
	badgeOKStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlack)).Background(colorGreen).Bold(true)
	badgeWarnStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlack)).Background(colorYellow).Bold(true)
	badgeErrStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlack)).Background(colorRed).Bold(true)
	badgeQuietStyle = lipgloss.NewStyle().Foreground(colorGray).Reverse(true)

	warnStyle = lipgloss.NewStyle().Foreground(colorYellow)

	// The human's own messages stand out from agent traffic: a white badge
	// and a bar down the left edge, which carries the meaning under
	// NO_COLOR, with the text in full brightness.
	youBadgeStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlack)).Background(colorBright).Bold(true)
	youBarStyle   = lipgloss.NewStyle().Foreground(colorBright).Bold(true)
	youTextStyle  = lipgloss.NewStyle().Foreground(colorBright)

	// What an agent says to you is marked in orange, the one colour outside
	// the 16-colour palette (256-colour index 208, which every current
	// Windows console renders), so it cannot be mistaken for an agent's
	// identity colour or a state. Under NO_COLOR the bar and the badge
	// still say it.
	colorOrange = lipgloss.Color("208")
	// The final decision is marked in violet (256-colour 141), a colour
	// nothing else uses, with a DECISION badge that carries it under
	// NO_COLOR.
	colorDecision     = lipgloss.Color("141")
	decisionBadge     = lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlack)).Background(colorDecision).Bold(true)
	decisionBarStyle  = lipgloss.NewStyle().Foreground(colorDecision).Bold(true)
	decisionTextStyle = lipgloss.NewStyle().Foreground(colorDecision)
	forYouBadge       = lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlack)).Background(colorOrange).Bold(true)
	forYouBarStyle    = lipgloss.NewStyle().Foreground(colorOrange).Bold(true)
	forYouTextStyle   = lipgloss.NewStyle().Foreground(colorOrange)

	// Mock mode is called out loudly. A scripted reply looks exactly like a
	// real one on screen, and mistaking one for the other wastes real time.
	mockKindStyle   = lipgloss.NewStyle().Foreground(colorYellow).Bold(true)
	mockBannerStyle = lipgloss.NewStyle().Foreground(colorYellow).Bold(true)

	// Per-kind styling for the live agent-activity panes. thinkingStyle is
	// deliberately dimmer than textActivityStyle: "thinking" is the agent's
	// scratch reasoning, "text" is what it is actually saying, and the two
	// must be visually distinguishable at a glance in a fast-scrolling pane.
	thinkingStyle       = lipgloss.NewStyle().Foreground(colorGray)
	textActivityStyle   = lipgloss.NewStyle().Foreground(colorWhite)
	toolStyle           = lipgloss.NewStyle().Foreground(colorBright)
	statusActivityStyle = lipgloss.NewStyle().Foreground(colorCyan)
)

// streamStyle maps a merged-stream row kind to its display style. Thinking is
// dimmed to chrome level on purpose: it is the agent's scratch reasoning, and
// in a fast-scrolling merged view it must not compete with what the agent
// actually said or did.
func streamStyle(k streamKind) lipgloss.Style {
	switch k {
	case streamThinking:
		return thinkingStyle
	case streamTool:
		return toolStyle
	case streamStatus, streamEvent:
		return dimStyle
	case streamError:
		return lipgloss.NewStyle().Foreground(colorRed)
	case streamWarn:
		return warnStyle
	default:
		return bodyStyle
	}
}

// taskStateBadge renders the task's run state as an inverted chip, the one
// piece of the header that must be readable from across a desk.
func taskStateBadge(s domain.TaskState) cell {
	label := " " + gl().working + " " + string(s) + " "
	switch s {
	case domain.TaskCompleted:
		return st(label, badgeOKStyle)
	case domain.TaskFailed, domain.TaskCancelled:
		return st(label, badgeErrStyle)
	case domain.TaskPaused, domain.TaskAwaitingDecision:
		return st(label, badgeWarnStyle)
	case domain.TaskCreated:
		return st(label, badgeQuietStyle)
	default:
		return st(label, badgeOKStyle)
	}
}

// spinnerFrames is the animated "working" indicator. Plain ASCII, since the
// TUI targets plain Windows Terminal / PowerShell consoles.
var spinnerFrames = [...]byte{'|', '/', '-', '\\'}

// spinnerGlyph returns the spinner character for frame n.
func spinnerGlyph(n int) string {
	return string(spinnerFrames[n%len(spinnerFrames)])
}

// activityStyle maps an activity kind to its display style.
func activityStyle(k ports.ActivityKind) lipgloss.Style {
	switch k {
	case ports.ActivityThinking:
		return thinkingStyle
	case ports.ActivityTool:
		return toolStyle
	case ports.ActivityStatus:
		return statusActivityStyle
	default: // ports.ActivityText and anything unrecognised
		return textActivityStyle
	}
}

// agentStateColor maps an agent lifecycle state to a display colour.
func agentStateColor(s domain.AgentState) lipgloss.Color {
	switch s {
	case domain.AgentStateWorking:
		return colorGreen
	case domain.AgentStateIdle:
		return colorGray
	case domain.AgentStateStarting:
		return colorYellow
	case domain.AgentStateUnavailable:
		return colorYellow
	case domain.AgentStateError:
		return colorRed
	case domain.AgentStateStopped:
		return colorGray
	default:
		return colorGray
	}
}

// taskStateColor maps a task lifecycle state to a display colour.
func taskStateColor(s domain.TaskState) lipgloss.Color {
	switch s {
	case domain.TaskCompleted:
		return colorGreen
	case domain.TaskFailed, domain.TaskCancelled:
		return colorRed
	case domain.TaskPaused:
		return colorYellow
	case domain.TaskCreated:
		return colorGray
	default:
		return colorCyan
	}
}

// messageTypeColor maps a protocol message type to a display colour so the
// conversation pane reads at a glance.
func messageTypeColor(t domain.MessageType) lipgloss.Color {
	switch t {
	case domain.MsgFinalDecision, domain.MsgApprove:
		return colorGreen
	case domain.MsgReject, domain.MsgChallenge, domain.MsgError:
		return colorRed
	case domain.MsgRequestChange, domain.MsgReview:
		return colorYellow
	case domain.MsgDelegate, domain.MsgTaskAssignment, domain.MsgImplement:
		return colorMagenta
	default:
		return colorCyan
	}
}

// taskMarker returns the ASCII-safe glyph used to mark a task in the task
// list: a filled dot for anything in flight, a check for completed, an "x"
// for failed/cancelled.
func taskMarker(s domain.TaskState) string {
	switch s {
	case domain.TaskCompleted:
		return gl().done
	case domain.TaskFailed, domain.TaskCancelled:
		return gl().errored
	default:
		return gl().working
	}
}
