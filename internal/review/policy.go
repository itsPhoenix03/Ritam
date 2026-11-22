package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"ritam/internal/brand"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
	"ritam/internal/review/facts"
	"ritam/internal/review/rules"
)

// Policy is what the review decides with. A server without one only
// records calls, refusing those it cannot read.
//
// A call is parsed into facts (package facts) and checked against the rules
// (package rules), then against the turn's history. Its worst finding
// decides:
//
//	critical    refused, and the turn stopped (the task is paused)
//	harmful     refused
//	suspicious  the human is asked
//	info, none  passed in auto mode; in manual mode a command is asked about
//
// A call the human is asked about is refused when nobody answers in time,
// or nobody is there to answer. A passed call still meets the CLI's own
// permission rules and the access level.
type Policy struct {
	Rules *rules.Set
	// Settings are read on every call, so a change from the settings tab
	// applies at once.
	Settings func() Settings
	// Env describes the machine for a turn: the workspace, the home folder,
	// the protected files, the turn's build folder.
	Env func(ports.TurnScope) facts.Env
	// Asker asks the human; nil when nobody can be asked.
	Asker Asker
	// OnStop is told when a critical call stops a turn, after its hook has
	// been answered.
	OnStop func(Stop)
}

// Settings are the human's choices that bear on the review.
type Settings struct {
	// Manual: every command waits for the human.
	Manual bool
	// AskTimeout is how long a call waits for the human's answer.
	AskTimeout time.Duration
}

func (p *Policy) settings() Settings {
	s := Settings{AskTimeout: 5 * time.Minute}
	if p != nil && p.Settings != nil {
		s = p.Settings()
	}
	if s.AskTimeout <= 0 {
		s.AskTimeout = 5 * time.Minute
	}
	return s
}

// Stop is a turn stopped by a critical call.
type Stop struct {
	Task   domain.TaskID
	Agent  domain.AgentID
	Reason string
}

// Question is a call the human is asked to approve.
type Question struct {
	ID    string
	Task  domain.TaskID
	Agent domain.AgentID
	Kind  domain.AgentKind
	Tool  string
	// Summary says what the call does, secrets redacted.
	Summary string
	// Reasons are why it is asked: the findings, or manual mode.
	Reasons  []string
	Severity string
	// Manual: asked because the mode is manual, with no finding at all.
	Manual bool
	// Deadline is when it is refused if unanswered.
	Deadline time.Time
}

// Answer is the human's decision on a question.
type Answer struct {
	Allow bool
	// Note is what the human added; for a refusal it reaches the agent.
	Note string
}

// ErrNobody is what an Asker returns when nobody is there to answer.
var ErrNobody = errors.New("nobody is there to answer")

// Asker asks the human about a call and waits for the answer, or for ctx
// to end.
type Asker interface {
	Ask(ctx context.Context, q Question) (Answer, error)
}

// Ask is the policy's decision to put a call to the human; the hook never
// sees it, only the pass or deny that follows.
const Ask Decision = "ask"

// Finding is one rule that fired, as the record keeps it.
type Finding struct {
	Rule     string
	Severity string
	Category string
	Reason   string
	Evidence string
}

// decision is what the policy made of a call.
type decision struct {
	decision Decision
	stop     bool
	severity rules.Severity
	findings []rules.Finding
	manual   bool
}

// decide maps the worst finding and the mode to a decision.
func decide(findings []rules.Finding, s Settings, kind facts.ToolKind) decision {
	d := decision{findings: findings, decision: Pass}
	for _, f := range findings {
		if f.Rule.Severity > d.severity {
			d.severity = f.Rule.Severity
		}
	}
	switch {
	case d.severity == rules.Critical:
		d.decision, d.stop = Deny, true
	case d.severity == rules.Harmful:
		d.decision = Deny
	case d.severity == rules.Suspicious:
		d.decision = Ask
	case s.Manual && kind == facts.ToolShell:
		d.decision, d.manual = Ask, true
	}
	return d
}

// review decides a call: its facts, the rules, the turn's history, and the
// human when asked. It fills in the record and returns the reason shown to
// the agent on a refusal.
func (s *Server) review(ctx context.Context, t *Turn, p payload, call *Call) string {
	pol := s.Policy
	in := facts.Input{Provider: string(t.kind), Tool: p.ToolName, ToolInput: p.ToolInput, Cwd: p.Cwd}
	fc := facts.Build(in, t.env)
	t.addWritten(fc)
	findings := pol.Rules.Check(fc, t.env)
	set := pol.settings()
	d := decide(findings, set, fc.Kind)
	// The turn's history may make it worse: a refusal tried another way, a
	// script written then run.
	cur := rules.StepOf(fc, findings, string(d.decision))
	if seq := pol.Rules.CheckSequence(cur, t.history, t.env); len(seq) > 0 {
		d = decide(append(findings, seq...), set, fc.Kind)
	}

	call.Severity = d.severity.String()
	for _, f := range d.findings {
		call.Findings = append(call.Findings, Finding{Rule: f.Rule.ID, Severity: f.Rule.Severity.String(),
			Category: f.Rule.Category, Reason: f.Rule.Reason, Evidence: logging.Redact(f.Evidence)})
	}
	reason := ""
	switch d.decision {
	case Deny:
		reason = refusal(d.findings)
		if d.stop {
			call.Stopped = true
			reason += fmt.Sprintf(" %s has stopped this turn and paused the task for the human.", brand.Name)
		}
	case Ask:
		call.Asked = true
		reason = s.ask(ctx, t, call, d, set)
		if reason == "" {
			d.decision = Pass
		} else {
			d.decision = Deny
		}
	}
	call.Decision = d.decision
	call.Note = reason
	t.history.Add(rules.StepOf(fc, d.findings, string(d.decision)))
	return reason
}

// refusal is what the agent is told when a rule refuses its call: the worst
// finding's reason and what matched.
func refusal(findings []rules.Finding) string {
	if len(findings) == 0 {
		return "refused by " + brand.Name + "'s review"
	}
	f := findings[0]
	msg := fmt.Sprintf("refused: %s [%s", f.Rule.Reason, f.Rule.ID)
	if ev := logging.Redact(f.Evidence); ev != "" {
		msg += ": " + ev
	}
	msg += "]"
	if len(findings) > 1 {
		var others []string
		for _, o := range findings[1:] {
			if o.Rule.Severity >= rules.Suspicious {
				others = append(others, o.Rule.ID)
			}
		}
		if len(others) > 0 {
			msg += " (also " + strings.Join(others, ", ") + ")"
		}
	}
	return msg
}

// ask puts the call to the human and waits. It returns "" when the human
// approved, otherwise why the call is refused.
func (s *Server) ask(parent context.Context, t *Turn, call *Call, d decision, set Settings) string {
	q := Question{ID: newToken()[:16], Task: t.task, Agent: t.agent, Kind: t.kind, Tool: call.Tool,
		Summary: call.Summary, Severity: d.severity.String(), Manual: d.manual,
		Deadline: time.Now().Add(set.AskTimeout)}
	for _, f := range d.findings {
		if f.Rule.Severity >= rules.Suspicious {
			q.Reasons = append(q.Reasons, f.Rule.Reason)
		}
	}
	if d.manual {
		q.Reasons = append(q.Reasons, "manual mode: every command waits for your approval")
	}
	if s.Policy.Asker == nil {
		call.Answer = "nobody"
		return "this call needs the human's approval, and nobody is there to give it; " + why(q)
	}
	call.Question = q.ID
	// The question goes when the hook does: a CLI killed mid-question (the
	// task paused, ritam closing) leaves nothing to answer.
	ctx, cancel := context.WithTimeout(parent, set.AskTimeout)
	defer cancel()
	ans, err := s.Policy.Asker.Ask(ctx, q)
	switch {
	case errors.Is(err, ErrNobody):
		call.Answer = "nobody"
		return "this call needs the human's approval, and nobody is there to give it; " + why(q)
	case errors.Is(err, context.Canceled) || parent.Err() != nil:
		call.Answer = "withdrawn"
		return "the call was withdrawn: its turn ended while the human was asked"
	case err != nil:
		call.Answer = "timed out"
		return fmt.Sprintf("this call needs the human's approval, and nobody answered within %s; %s", set.AskTimeout, why(q))
	case ans.Allow:
		call.Answer = "approved"
		return ""
	}
	call.Answer = "refused"
	msg := "the human refused this call"
	if ans.Note != "" {
		msg += ": " + ans.Note
	}
	return msg
}

func why(q Question) string {
	if len(q.Reasons) == 0 {
		return "ask the human, or do it another way"
	}
	return "it was asked about because: " + strings.Join(q.Reasons, "; ")
}
