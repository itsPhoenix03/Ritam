package rules

import (
	"slices"
	"strings"
	"sync"

	"ritam/internal/review/facts"
)

// Step is what a turn's history keeps of one reviewed call.
type Step struct {
	Kind       facts.ToolKind
	Programs   []string
	Paths      []facts.Path
	Rules      []string
	Categories []string
	// Decision is pass, ask or deny.
	Decision string
	// Written are the texts the call wrote, by path, for checking a
	// script that is run later in the turn.
	Written map[string]string
}

// StepOf summarizes a call, its findings and the decision on it.
func StepOf(call *facts.Call, findings []Finding, decision string) Step {
	s := Step{Kind: call.Kind, Paths: call.Paths, Decision: decision}
	for _, c := range call.Commands {
		if c.Program != "" && !slices.Contains(s.Programs, c.Program) {
			s.Programs = append(s.Programs, c.Program)
		}
	}
	for _, f := range findings {
		s.Rules = append(s.Rules, f.Rule.ID)
		if !slices.Contains(s.Categories, f.Rule.Category) {
			s.Categories = append(s.Categories, f.Rule.Category)
		}
	}
	for _, c := range call.Contents {
		if c.Scope == facts.ScopeEdit && c.Path != "" {
			if s.Written == nil {
				s.Written = map[string]string{}
			}
			s.Written[c.Path] = c.Text
		}
	}
	return s
}

// History is one turn's reviewed calls, oldest first. It is safe for use
// by several goroutines: a CLI may make calls in parallel.
type History struct {
	mu    sync.Mutex
	steps []Step
}

// Add records a reviewed call.
func (h *History) Add(s Step) {
	h.mu.Lock()
	h.steps = append(h.steps, s)
	h.mu.Unlock()
}

// Steps returns a copy of the history.
func (h *History) Steps() []Step {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.steps)
}

// Written returns what the turn wrote to p, the latest write first found.
func (h *History) Written(p string, fold bool) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := len(h.steps) - 1; i >= 0; i-- {
		for path, text := range h.steps[i].Written {
			if path == p || fold && strings.EqualFold(path, p) {
				return text, true
			}
		}
	}
	return "", false
}

// CheckSequence tests the sequence rules: cur is the call being decided,
// its findings and tentative decision already in it, against the turn's
// earlier calls.
func (s *Set) CheckSequence(cur Step, h *History, env facts.Env) []Finding {
	past := h.Steps()
	fold := env.OS == "windows" || env.OS == "macos"
	var out []Finding
	for _, r := range s.byTarget[OnSequence] {
		if !r.appliesTo(env.OS, "") {
			continue
		}
		window := past
		if n := r.match.within; n > 0 && len(window) > n {
			window = window[len(window)-n:]
		}
		if ev, ok := sequenceFires(r.match, cur, window, env, fold); ok {
			out = append(out, Finding{Rule: r, Evidence: ev})
		}
	}
	sortFindings(out)
	return out
}

func sequenceFires(k *cond, cur Step, window []Step, env facts.Env, fold bool) (string, bool) {
	if k.count != nil {
		n := 0
		if stepMatches(k.count.of, cur, env) {
			n++
		}
		for _, p := range window {
			if stepMatches(k.count.of, p, env) {
				n++
			}
		}
		return "", n >= k.count.min
	}
	if len(k.steps) != 2 {
		return "", false
	}
	first, last := k.steps[0], k.steps[1]
	if !stepMatches(last, cur, env) {
		return "", false
	}
	for i := len(window) - 1; i >= 0; i-- {
		prev := window[i]
		if !stepMatches(first, prev, env) {
			continue
		}
		if last.sameCategory && !slices.ContainsFunc(cur.Categories, func(c string) bool { return slices.Contains(prev.Categories, c) }) {
			continue
		}
		if last.samePath {
			if p, ok := sharedPath(first, last, prev, cur, env, fold); ok {
				return p, true
			}
			continue
		}
		return "", true
	}
	return "", false
}

// sharedPath finds a path the earlier call touched as its step says and
// the current call touches as its own does.
func sharedPath(first, last step, prev, cur Step, env facts.Env, fold bool) (string, bool) {
	for _, pc := range cur.Paths {
		if !pathFits(last, pc, env) {
			continue
		}
		for _, pp := range prev.Paths {
			if !pathFits(first, pp, env) {
				continue
			}
			if pc.Path == pp.Path || fold && strings.EqualFold(pc.Path, pp.Path) {
				return pc.Path, true
			}
		}
	}
	return "", false
}

func pathFits(st step, p facts.Path, env facts.Env) bool {
	if len(st.access) > 0 && !slices.Contains(st.access, string(p.Access)) {
		return false
	}
	return len(st.glob) == 0 || slices.ContainsFunc(st.glob, func(g string) bool { return env.MatchGlob(g, p.Path) })
}

// stepMatches tests one step's keys against a call.
func stepMatches(st step, x Step, env facts.Env) bool {
	if len(st.category) > 0 && !slices.ContainsFunc(x.Categories, func(c string) bool { return slices.Contains(st.category, c) }) {
		return false
	}
	if len(st.rule) > 0 && !slices.ContainsFunc(x.Rules, func(r string) bool { return slices.Contains(st.rule, r) }) {
		return false
	}
	if len(st.tool) > 0 && !slices.Contains(st.tool, string(x.Kind)) {
		return false
	}
	if len(st.program) > 0 && !slices.ContainsFunc(x.Programs, func(p string) bool { return slices.Contains(st.program, p) }) {
		return false
	}
	if len(st.decision) > 0 && !slices.Contains(st.decision, x.Decision) {
		return false
	}
	if (len(st.access) > 0 || len(st.glob) > 0) && !slices.ContainsFunc(x.Paths, func(p facts.Path) bool { return pathFits(st, p, env) }) {
		return false
	}
	return true
}
