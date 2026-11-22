package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The review's side of the TUI: the tool calls waiting for the human's
// approval, and the review mode in the settings tab.
//
// A call waiting for approval opens a prompt at the bottom when the human
// is not typing: y allows it, n refuses it, esc leaves it for later (A
// brings it back). It is refused anyway if nobody answers in time. The mode
// is set with :review auto|manual and shown at the top of the settings tab.

type reviewLoadedMsg struct {
	info ports.ReviewInfo
	err  error
}

type reviewChangedMsg struct {
	info ports.ReviewInfo
	err  error
}

type approvalsLoadedMsg struct{ list []ports.Approval }

type approvalAnsweredMsg struct {
	approval ports.Approval
	allow    bool
	err      error
}

func loadReviewCmd(ctx context.Context, svc ports.Service) tea.Cmd {
	return func() tea.Msg {
		info, err := svc.Review(ctx)
		return reviewLoadedMsg{info: info, err: err}
	}
}

func setReviewModeCmd(ctx context.Context, svc ports.Service, mode string) tea.Cmd {
	return func() tea.Msg {
		info, err := svc.SetReviewMode(ctx, mode)
		return reviewChangedMsg{info: info, err: err}
	}
}

func loadApprovalsCmd(ctx context.Context, svc ports.Service) tea.Cmd {
	return func() tea.Msg { return approvalsLoadedMsg{list: svc.PendingApprovals(ctx)} }
}

func answerApprovalCmd(ctx context.Context, svc ports.Service, a ports.Approval, allow bool) tea.Cmd {
	return func() tea.Msg {
		err := svc.AnswerApproval(ctx, a.ID, allow, "")
		return approvalAnsweredMsg{approval: a, allow: allow, err: err}
	}
}

// reviewEvent reports whether an event changes the waiting approvals.
func reviewEvent(ev domain.Event) bool {
	switch ev.Type {
	case domain.EvtToolApproval:
		return true
	case domain.EvtToolCall, domain.EvtToolBlocked:
		return ev.Details["approval"] != ""
	}
	return false
}

// applyApprovals takes the waiting approvals in, opening the prompt when
// one waits and the human is not typing, closing it when none is left.
func (m Model) applyApprovals(list []ports.Approval) Model {
	m.approvals = list
	switch {
	case len(list) == 0 && m.mode == modeApproval:
		m.mode = modeNormal
	case len(list) > 0 && m.mode == modeNormal && !m.approvalsLater:
		m.mode = modeApproval
		m.inputSelected = false
	case len(list) > 0 && m.mode != modeApproval:
		m.setStatus("%d tool call(s) wait for your approval: esc, then A", len(list))
	}
	if len(list) == 0 {
		m.approvalsLater = false
	}
	return m
}

// updateApproval handles a key while a call waits for approval.
func (m Model) updateApproval(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if len(m.approvals) == 0 {
		m.mode = modeNormal
		return m, nil
	}
	a := m.approvals[0]
	switch msg.String() {
	case "y", "Y":
		if m.burst {
			m.setStatus("ignored %s: it came with pasted text; press y to allow", msg.String())
			return m, nil
		}
		m.approvals = m.approvals[1:]
		if len(m.approvals) == 0 {
			m.mode = modeNormal
		}
		m.setStatus("allowing %s's %s...", a.Agent, a.Tool)
		return m, answerApprovalCmd(m.ctx, m.svc, a, true)
	case "n", "N":
		if m.burst {
			return m, nil
		}
		m.approvals = m.approvals[1:]
		if len(m.approvals) == 0 {
			m.mode = modeNormal
		}
		m.setStatus("refusing %s's %s...", a.Agent, a.Tool)
		return m, answerApprovalCmd(m.ctx, m.svc, a, false)
	case "esc":
		m.mode = modeNormal
		m.approvalsLater = true
		m.setStatus("%d tool call(s) wait for your approval; A brings them back (refused if unanswered in time)", len(m.approvals))
	}
	return m, nil
}

// openApprovals brings the approval prompt back.
func (m Model) openApprovals() (Model, tea.Cmd) {
	m.approvalsLater = false
	if len(m.approvals) == 0 {
		m.setStatus("no tool call waits for your approval")
		return m, loadApprovalsCmd(m.ctx, m.svc)
	}
	m.mode = modeApproval
	m.inputSelected = false
	return m, nil
}

// approvalLines is the bottom strip while a call waits.
func (m Model) approvalLines(inner int) []line {
	if len(m.approvals) == 0 {
		return nil
	}
	a := m.approvals[0]
	g := gl()
	what := a.Tool
	if a.Summary != "" {
		what += ": " + a.Summary
	}
	head := fmt.Sprintf("%s asks to run %s", a.Agent, what)
	if len(m.approvals) > 1 {
		head += fmt.Sprintf("  (1 of %d)", len(m.approvals))
	}
	var out []line
	for i, l := range wrapWords(head, maxInt(8, inner-2)) {
		p := txt("  ")
		if i == 0 {
			p = st(g.caret+" ", warnStyle)
		}
		out = append(out, line{p, st(l, warnStyle)})
	}
	why := strings.Join(a.Reasons, "; ")
	if left := time.Until(a.Deadline).Round(time.Second); left > 0 {
		why += fmt.Sprintf(" "+dot()+" refused in %s if unanswered", left)
	}
	if a.Task != "" {
		why = string(a.Task) + " " + dot() + " " + why
	}
	for _, l := range wrapWords(why, maxInt(8, inner-2)) {
		out = append(out, line{txt("  "), st(l, dimStyle)})
	}
	return out
}

// reviewLines is the REVIEW section at the top of the settings tab.
func (m Model) reviewLines(inner int) []line {
	var out []line
	para := func(text string, style lipgloss.Style) {
		for _, w := range wrapWords(text, maxInt(10, inner-2)) {
			out = append(out, line{txt("  "), st(w, style)})
		}
	}
	r := m.review
	where := ""
	if r.File != "" {
		where = " " + dot() + " saved to " + r.File
	}
	out = append(out, line{st("REVIEW", sectionHeaderStyle), st("  who approves the agents' tool calls"+where, dimStyle)})
	if !m.reviewLoaded {
		return append(out, line{st("  loading...", dimStyle)})
	}
	for _, mode := range []struct{ name, text string }{
		{"auto", "ritam's rules decide: harmful calls are refused, suspicious ones are asked about"},
		{"manual", "you approve every command as well"},
	} {
		row := line{txt("    "), st(fitWidth(mode.name, 9), emphStyle), st(mode.text, dimStyle)}
		if mode.name == r.Mode {
			row = row.join(st("  "+gl().done+" current", lipgloss.NewStyle().Foreground(colorGreen)))
		}
		out = append(out, row)
	}
	if r.Mode == "auto" && r.Warning != "" {
		para(r.Warning, warnStyle)
	}
	if !r.Active {
		para("The scripted agents call no tools, so the review is idle until real agents run.", dimStyle)
	}
	if r.HeldBack != "" {
		para("held back: "+r.HeldBack, warnStyle)
	}
	var yours []string
	for _, l := range []struct {
		verb string
		list []string
	}{{"deny", r.Deny}, {"ask", r.Ask}, {"protect", r.Protect}} {
		for _, p := range l.list {
			yours = append(yours, l.verb+" "+p)
		}
	}
	if len(yours) > 0 {
		para("your rules: "+strings.Join(yours, "  "+dot()+"  "), bodyStyle)
	}
	if len(r.Hooks) > 0 {
		para("your hooks: "+strings.Join(r.Hooks, ", "), bodyStyle)
	}
	if len(r.HeldHooks) > 0 {
		para("this project's hooks, not run until you approve them on this machine (:hooks approve <name>); they "+
			"run outside any sandbox, as you, so read the script first: "+strings.Join(r.HeldHooks, ", "), warnStyle)
	}
	out = append(out, line{st(fmt.Sprintf("  :review auto | :review manual "+dot()+" a call waits %s for your answer", r.AskTimeout), dimStyle)})
	return append(out, blank())
}

// reviewCommand runs :review [auto|manual].
func (m Model) reviewCommand(rest string) (Model, tea.Cmd) {
	mode := strings.ToLower(strings.TrimSpace(rest))
	if mode == "" {
		m.center, m.activePane, m.zoom = tabSettings, paneConversation, false
		if m.reviewLoaded {
			m.setStatus("review mode %s (:review auto|manual changes it)", m.review.Mode)
		}
		return m, loadReviewCmd(m.ctx, m.svc)
	}
	if mode != "auto" && mode != "manual" {
		m.setStatus("review mode %q: use auto or manual", mode)
		return m, nil
	}
	m.setStatus("switching the review to %s...", mode)
	return m, setReviewModeCmd(m.ctx, m.svc, mode)
}

// hooksCommand runs :hooks [approve <name>].
func (m Model) hooksCommand(rest string) (Model, tea.Cmd) {
	verb, name, _ := strings.Cut(strings.TrimSpace(rest), " ")
	name = strings.TrimSpace(name)
	switch {
	case verb == "" || verb == "list":
		m.center, m.activePane, m.zoom = tabSettings, paneConversation, false
		return m, loadReviewCmd(m.ctx, m.svc)
	case verb == "approve" && name != "":
		m.setStatus("approving the hook %s...", name)
		return m, approveHookCmd(m.ctx, m.svc, name)
	}
	m.setStatus("usage: :hooks approve <name> (the settings tab lists this project's held hooks)")
	return m, nil
}

func approveHookCmd(ctx context.Context, svc ports.Service, name string) tea.Cmd {
	return func() tea.Msg {
		info, err := svc.ApproveHook(ctx, name)
		return reviewChangedMsg{info: info, err: err}
	}
}
