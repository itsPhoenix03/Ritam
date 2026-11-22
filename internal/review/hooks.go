package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"ritam/internal/domain"
	"ritam/internal/review/facts"
	"ritam/internal/review/rules"
)

// Hook is a program of the human's own that the review runs on tool calls
// (review.hooks in the settings). It gets the call on stdin and can refuse
// it or put it to the human; it can never approve a call ritam's rules
// refused, nor skip a question ritam would ask.
//
// It answers with its exit code: 2 refuses the call, its stderr the reason;
// 0 has no objection, unless its stdout says otherwise in JSON:
// {"decision": "deny"|"ask", "reason": "..."}, or Claude Code's own form
// {"hookSpecificOutput": {"permissionDecision": "deny"|"ask", ...}}. An
// "allow" is ignored, as is a rewritten tool input. Any other exit, a crash
// or a timeout is a failure: the call is refused unless the hook says
// onFailure: ignore.
type Hook struct {
	Name string
	Run  []string
	// Wants says whether the hook is for a call: its tool's name and kind,
	// and the CLI making it. Nil: every call.
	Wants func(tool string, kind facts.ToolKind, cli domain.AgentKind) bool
	// Native: the hook gets the CLI's own payload rather than ritam's.
	Native  bool
	Timeout time.Duration
	// Block: a failing hook refuses the call.
	Block bool
	// Dir is where it runs (the workspace).
	Dir string
	// Check, when set, runs before the hook: an error refuses the call, and
	// the hook does not run (a project's hook changed since the human
	// approved it).
	Check func() error
}

// hookPayload is what a hook with ritam's input reads on stdin: the call
// and what ritam found in it.
type hookPayload struct {
	Event     string          `json:"event"`
	Task      string          `json:"task"`
	Agent     string          `json:"agent"`
	CLI       string          `json:"cli"`
	Tool      string          `json:"tool"`
	Kind      string          `json:"kind"`
	ToolInput json.RawMessage `json:"tool_input"`
	Cwd       string          `json:"cwd"`
	Summary   string          `json:"summary"`
	Severity  string          `json:"severity"`
	Rules     []string        `json:"rules"`
	Commands  []hookCommand   `json:"commands"`
	Paths     []hookPathFact  `json:"paths"`
}

type hookCommand struct {
	Program string   `json:"program"`
	Args    []string `json:"args"`
	Raw     string   `json:"raw"`
}

type hookPathFact struct {
	Path   string `json:"path"`
	Access string `json:"access"`
	Zone   string `json:"zone"`
}

// hookResult is one hook's answer.
type hookResult struct {
	name   string
	deny   bool
	ask    bool
	reason string
}

// maxHookOutput is the most of a hook's output read.
const maxHookOutput = 64 << 10

// runHooks runs the hooks that want the call, all at once, and returns
// their objections.
func runHooks(ctx context.Context, hooks []Hook, t *Turn, call *Call, fc *facts.Call, raw []byte, d decision) []hookResult {
	var want []Hook
	for _, h := range hooks {
		if h.Wants == nil || h.Wants(call.Tool, fc.Kind, t.kind) {
			want = append(want, h)
		}
	}
	if len(want) == 0 {
		return nil
	}
	ours := hookPayload{Event: preTool, Task: string(t.task), Agent: string(t.agent), CLI: string(t.kind),
		Tool: call.Tool, Kind: string(fc.Kind), Cwd: call.Cwd, Summary: call.Summary, Severity: d.severity.String()}
	var p payload
	if json.Unmarshal(raw, &p) == nil {
		ours.ToolInput = p.ToolInput
	}
	for _, f := range d.findings {
		ours.Rules = append(ours.Rules, f.Rule.ID)
	}
	for _, c := range fc.Commands {
		hc := hookCommand{Program: c.Program, Raw: c.Raw}
		for _, a := range c.Args {
			hc.Args = append(hc.Args, a.Value)
		}
		ours.Commands = append(ours.Commands, hc)
	}
	for _, pa := range fc.Paths {
		ours.Paths = append(ours.Paths, hookPathFact{Path: pa.Path, Access: string(pa.Access), Zone: string(pa.Zone)})
	}
	oursJSON, _ := json.Marshal(ours)

	results := make([]hookResult, len(want))
	var wg sync.WaitGroup
	for i, h := range want {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h.Check != nil {
				if err := h.Check(); err != nil {
					results[i] = hookResult{name: h.Name, deny: true, reason: err.Error()}
					return
				}
			}
			in := oursJSON
			if h.Native {
				in = raw
			}
			results[i] = runUserHook(ctx, h, in)
		}()
	}
	wg.Wait()
	var out []hookResult
	for _, r := range results {
		if r.deny || r.ask {
			out = append(out, r)
		}
	}
	return out
}

// runUserHook runs one hook and reads its answer.
func runUserHook(ctx context.Context, h Hook, input []byte) hookResult {
	res := hookResult{name: h.Name}
	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, h.Run[0], h.Run[1:]...)
	cmd.Dir = h.Dir
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr limitedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	fail := func(why string) hookResult {
		if h.Block {
			res.deny, res.reason = true, "it failed ("+why+"), and a failing hook refuses the call"
		}
		return res
	}
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return fail(fmt.Sprintf("no answer within %s", timeout))
	case errors.As(err, &exit) && exit.ExitCode() == blockExit:
		res.deny, res.reason = true, firstLine(stderr.String(), "refused")
		return res
	case err != nil:
		return fail(firstLine(stderr.String(), err.Error()))
	}
	decision, reason := hookDecision(stdout.Bytes())
	switch decision {
	case "deny", "block":
		res.deny, res.reason = true, firstLine(reason, "refused")
	case "ask":
		res.ask, res.reason = true, firstLine(reason, "it asks for your approval")
	}
	return res
}

// hookDecision reads a decision from a hook's stdout, in ritam's form or
// Claude Code's; "" when it says nothing (or only allows).
func hookDecision(out []byte) (string, string) {
	var v struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
		Continue *bool  `json:"continue"`
		Stop     string `json:"stopReason"`
		Specific struct {
			Decision string `json:"permissionDecision"`
			Reason   string `json:"permissionDecisionReason"`
		} `json:"hookSpecificOutput"`
	}
	if len(bytes.TrimSpace(out)) == 0 || json.Unmarshal(out, &v) != nil {
		return "", ""
	}
	switch d := strings.ToLower(v.Specific.Decision); d {
	case "deny", "ask":
		return d, v.Specific.Reason
	}
	switch d := strings.ToLower(v.Decision); d {
	case "deny", "block", "ask":
		return d, v.Reason
	}
	if v.Continue != nil && !*v.Continue {
		return "deny", v.Stop
	}
	return "", ""
}

func firstLine(s, fallback string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" {
		return fallback
	}
	return clip(s, 300)
}

// limitedBuffer keeps the first maxHookOutput bytes written to it.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := maxHookOutput - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// applyHooks folds the hooks' objections into the decision: a refusal
// refuses the call, a question puts a passed call to the human.
func applyHooks(d *decision, results []hookResult, call *Call) (refusal string, asks []string) {
	for _, r := range results {
		sev := "harmful"
		if !r.deny {
			sev = "suspicious"
		}
		call.Findings = append(call.Findings, Finding{Rule: "hook." + r.name, Severity: sev, Category: "custom",
			Reason: r.reason})
		if r.deny && refusal == "" {
			refusal = fmt.Sprintf("refused by your hook %s: %s", r.name, r.reason)
		}
		if r.ask {
			asks = append(asks, fmt.Sprintf("your hook %s: %s", r.name, r.reason))
		}
	}
	switch {
	case refusal != "":
		d.decision = Deny
		if d.severity < rules.Harmful {
			d.severity = rules.Harmful
		}
	case len(asks) > 0 && d.decision == Pass:
		d.decision = Ask
		if d.severity < rules.Suspicious {
			d.severity = rules.Suspicious
		}
	}
	return refusal, asks
}
