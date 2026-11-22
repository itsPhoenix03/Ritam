package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

func waiting(id string) ports.Approval {
	return ports.Approval{ID: id, Task: "TASK-001", Agent: "opus", Tool: "Bash", Summary: "rm -rf src",
		Reasons: []string{"Recursive delete inside the workspace"}, Severity: "suspicious",
		Deadline: time.Now().Add(5 * time.Minute)}
}

// TestACallWaitingForApprovalIsAskedAboutAndAnswered: a waiting call opens
// the prompt; y allows it, and the next one is asked about.
func TestACallWaitingForApprovalIsAskedAboutAndAnswered(t *testing.T) {
	svc := newFakeService()
	svc.pending = []ports.Approval{waiting("q1"), waiting("q2")}
	m := populatedModel(svc)
	m = m.applyApprovals(svc.PendingApprovals(m.ctx))
	if m.mode != modeApproval {
		t.Fatalf("mode %v", m.mode)
	}
	lines := m.approvalLines(80)
	text := ""
	for _, l := range lines {
		text += l.plain() + "\n"
	}
	if !strings.Contains(text, "opus asks to run Bash: rm -rf src") || !strings.Contains(text, "1 of 2") ||
		!strings.Contains(text, "Recursive delete") {
		t.Errorf("the prompt says what and why:\n%s", text)
	}
	m, cmd := key(m, "y")
	if cmd == nil {
		t.Fatal("y answers")
	}
	m = runReview(t, m, cmd)
	if allowed, ok := svc.answers["q1"]; !ok || !allowed {
		t.Errorf("answers: %v", svc.answers)
	}
	if m.mode != modeApproval || len(m.approvals) != 1 || m.approvals[0].ID != "q2" {
		t.Errorf("the next call is asked about: mode %v, %+v", m.mode, m.approvals)
	}
	m, cmd = key(m, "n")
	m = runReview(t, m, cmd)
	if allowed, ok := svc.answers["q2"]; !ok || allowed {
		t.Errorf("n refuses: %v", svc.answers)
	}
	if m.mode != modeNormal {
		t.Errorf("none left: mode %v", m.mode)
	}
}

// TestAWaitingCallCanBeLeftForLater: esc puts the prompt away, A brings it
// back, and the prompt never takes over while the human is typing.
func TestAWaitingCallCanBeLeftForLater(t *testing.T) {
	svc := newFakeService()
	svc.pending = []ports.Approval{waiting("q1")}
	m := populatedModel(svc)
	m = m.applyApprovals(svc.PendingApprovals(m.ctx))
	m, _ = key(m, "esc")
	if m.mode != modeNormal || svc.callCount("AnswerApproval") != 0 {
		t.Fatalf("esc leaves it unanswered: mode %v", m.mode)
	}
	m = m.applyApprovals(svc.PendingApprovals(m.ctx))
	if m.mode != modeNormal {
		t.Error("a call put off stays put off until A")
	}
	m, _ = key(m, "A")
	if m.mode != modeApproval {
		t.Errorf("A brings it back: mode %v", m.mode)
	}

	m.mode = modePrompt
	m.approvalsLater = false
	m = m.applyApprovals(svc.PendingApprovals(m.ctx))
	if m.mode != modePrompt || !strings.Contains(m.statusLine, "wait for your approval") {
		t.Errorf("typing is not interrupted: mode %v, status %q", m.mode, m.statusLine)
	}
}

func TestTheReviewModeIsShownAndSet(t *testing.T) {
	svc := newFakeService()
	m := populatedModel(svc)
	m.review, m.reviewLoaded = ports.ReviewInfo{Mode: "auto", Warning: "rules cannot foresee everything", Active: true,
		AskTimeout: 5 * time.Minute}, true
	text := ""
	for _, l := range m.reviewLines(100) {
		text += l.plain() + "\n"
	}
	if !strings.Contains(text, "REVIEW") || !strings.Contains(text, "rules cannot foresee everything") ||
		!strings.Contains(text, ":review auto | :review manual") {
		t.Errorf("settings:\n%s", text)
	}
	m, cmd := m.reviewCommand("manual")
	m = runReview(t, m, cmd)
	if svc.review.Mode != "manual" || m.review.Mode != "manual" {
		t.Errorf("mode: service %s, model %s", svc.review.Mode, m.review.Mode)
	}
	if m, _ := m.reviewCommand("yolo"); !strings.Contains(m.statusLine, "use auto or manual") {
		t.Errorf("status: %q", m.statusLine)
	}
}

func TestRefusedCallsShowAsFailures(t *testing.T) {
	if !isFailureEvent(domain.EvtToolBlocked) || !streamShowsEvent(domain.Event{Type: domain.EvtToolBlocked}) ||
		!convShowsEvent(domain.Event{Type: domain.EvtToolApproval}) {
		t.Error("a refused call is a failure in the stream; an approval shows in the conversation")
	}
	if !reviewEvent(domain.Event{Type: domain.EvtToolBlocked, Details: map[string]string{"approval": "q1"}}) ||
		reviewEvent(domain.Event{Type: domain.EvtToolCall}) {
		t.Error("only an approval's events reload the waiting calls")
	}
}

// runReview feeds a command's result back into the model, following only
// the review's own messages (a repaint ticker would never end).
func runReview(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	if cmd == nil {
		return m
	}
	switch msg := cmd().(type) {
	case tea.BatchMsg:
		for _, c := range msg {
			m = runReview(t, m, c)
		}
	case reviewLoadedMsg, reviewChangedMsg, approvalsLoadedMsg, approvalAnsweredMsg:
		next, more := m.Update(msg)
		m = runReview(t, next.(Model), more)
	}
	return m
}
