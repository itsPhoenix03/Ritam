package review

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"ritam/internal/brand"
	"ritam/internal/domain"
	"ritam/internal/logging"
	"ritam/internal/ports"
	"ritam/internal/review/facts"
	"ritam/internal/review/rules"
)

// Server is the review server: the hook's other end, inside ritam. It
// listens on the loopback interface only, and answers only calls carrying
// the token of a turn in progress.
type Server struct {
	log *logging.Logger
	// program is ritam's own executable, the hook the CLIs run.
	program string
	srv     *http.Server
	// base is the address the hook calls; host is its host:port, which every
	// request must name (a page in a browser cannot, so it cannot reach the
	// server by renaming a domain to 127.0.0.1).
	base, host string

	mu    sync.Mutex
	turns map[string]*Turn // by token

	// OnCall is told about every call the hook reports, as it is answered.
	// It runs while the CLI waits, so it must be quick. Set it before the
	// first turn begins.
	OnCall func(Call)
	// OnTurnEnd is told how each turn's calls compared with what its CLI
	// reported. Set it before the first turn begins.
	OnTurnEnd func(Report)
	// Policy decides the calls; without one they are only recorded. Set it
	// before the first turn begins.
	Policy *Policy
}

// Start listens on a free port of 127.0.0.1 and serves the hook. program is
// the absolute path of ritam's executable, which the CLIs run as the hook.
func Start(program string, log *logging.Logger) (*Server, error) {
	if log == nil {
		log = logging.NewDiscard()
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("review server: %w", err)
	}
	s := &Server{
		log:     log,
		program: program,
		host:    ln.Addr().String(),
		turns:   map[string]*Turn{},
	}
	s.base = "http://" + s.host
	mux := http.NewServeMux()
	mux.HandleFunc(hookPath, s.handleHook)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		if err := s.srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("review server stopped", "error", err)
		}
	}()
	log.Info("review server listening", "address", s.host)
	return s, nil
}

// Close stops the server. A hook still asking is refused, which blocks its
// call: ritam is going away, and nothing can check it.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}

// URL is the address the hook calls.
func (s *Server) URL() string { return s.base }

// HookCommand implements ports.HookMonitor. With a policy the CLI waits as
// long as the human may take to answer, and a little more, so the hook
// always answers first.
func (s *Server) HookCommand(domain.AgentKind) ports.HookCommand {
	timeout := cliTimeoutSeconds
	if s.Policy != nil {
		timeout = int(s.hookWait().Seconds()) + cliMargin
	}
	return ports.HookCommand{Program: s.program, Args: []string{"hook", preTool}, TimeoutSeconds: timeout}
}

// hookWait is how long the hook waits for the server: the time the human
// has to answer, and a margin for the review itself.
func (s *Server) hookWait() time.Duration {
	if s.Policy == nil {
		return hookTimeout
	}
	return s.Policy.settings().AskTimeout + hookTimeout
}

// BeginTurn implements ports.HookMonitor.
func (s *Server) BeginTurn(scope ports.TurnScope) ports.HookTurn {
	t := &Turn{s: s, token: newToken(), task: scope.Task, agent: scope.Agent, kind: scope.Kind,
		seen: map[string]bool{}, history: &rules.History{}}
	if s.Policy != nil && s.Policy.Env != nil {
		t.env = s.Policy.Env(scope)
	}
	s.mu.Lock()
	s.turns[t.token] = t
	s.mu.Unlock()
	return t
}

// turnFor returns the turn holding token, or nil.
func (s *Server) turnFor(token string) *Turn {
	if token == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turns[token]
}

// handleHook answers one call from the hook.
func (s *Server) handleHook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeVerdict(w, http.StatusMethodNotAllowed, Deny, "the hook must POST")
		return
	}
	if r.Host != s.host {
		writeVerdict(w, http.StatusForbidden, Deny, "the request does not name the review server")
		return
	}
	if event := strings.TrimPrefix(r.URL.Path, hookPath); event != preTool {
		writeVerdict(w, http.StatusNotFound, Deny, fmt.Sprintf("no hook event %q", event))
		return
	}
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	turn := s.turnFor(token)
	if turn == nil {
		writeVerdict(w, http.StatusUnauthorized, Deny, "the call comes from no agent turn ritam is running")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, MaxPayload+1))
	call := Call{Task: turn.task, Agent: turn.agent, Kind: turn.kind, Decision: Pass}
	switch {
	case err != nil:
		call.Decision, call.Note = Deny, "the call could not be read"
	case len(body) > MaxPayload:
		call.Decision, call.Note = Deny, fmt.Sprintf("the call is larger than %d KiB, too large to check", MaxPayload>>10)
	default:
		p, err := decodePayload(body)
		if err != nil {
			call.Decision, call.Note = Deny, "the call is not a hook payload ritam can read: "+err.Error()
			break
		}
		call.SessionID, call.Cwd, call.Tool, call.ToolUseID, call.Subagent =
			p.SessionID, p.Cwd, p.ToolName, p.ToolUseID, p.AgentID
		call.Summary = summarize(p.ToolInput)
		if s.Policy != nil && s.Policy.Rules != nil {
			s.review(r.Context(), turn, p, body, &call)
		}
	}
	turn.record(call)
	if s.OnCall != nil {
		s.OnCall(call)
	}
	writeVerdict(w, http.StatusOK, call.Decision, call.Note)
	if call.Stopped && s.Policy.OnStop != nil {
		go s.Policy.OnStop(Stop{Task: call.Task, Agent: call.Agent, Reason: call.Note})
	}
}

func writeVerdict(w http.ResponseWriter, status int, d Decision, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Verdict{Decision: d, Reason: reason})
}

// newToken is a turn's credential: 32 random bytes, hex.
func newToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail on supported platforms; a fixed token
		// would let any process pass as any turn, so refuse to go on.
		panic("review: no randomness for a turn token: " + err.Error())
	}
	return hex.EncodeToString(b)
}

// Turn is one CLI invocation's hook calls. It implements ports.HookTurn.
type Turn struct {
	s     *Server
	token string
	task  domain.TaskID
	agent domain.AgentID
	kind  domain.AgentKind

	// env describes the machine for the turn's review; history is what the
	// turn did so far.
	env     facts.Env
	history *rules.History

	mu    sync.Mutex
	seen  map[string]bool // tool_use ids the hook reported
	calls int
	ended bool
}

// Env implements ports.HookTurn.
func (t *Turn) Env() map[string]string {
	return map[string]string{URLEnv(): t.s.base, TokenEnv(): t.token,
		WaitEnv(): strconv.Itoa(int(t.s.hookWait().Seconds()))}
}

// addWritten treats a script the turn wrote, now run, as code to check:
// write a script, then run it, is how a command gets past a check of the
// command line alone.
func (t *Turn) addWritten(c *facts.Call) {
	fold := t.env.OS == "windows" || t.env.OS == "macos"
	for _, p := range c.Paths {
		if p.Access != facts.Exec {
			continue
		}
		if text, ok := t.history.Written(p.Path, fold); ok {
			c.Contents = append(c.Contents, facts.Content{Scope: facts.ScopeInline, Path: p.Path, Text: text})
		}
	}
}

func (t *Turn) record(c Call) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.calls++
	if c.ToolUseID != "" {
		t.seen[c.ToolUseID] = true
	}
}

// End implements ports.HookTurn. It is safe to call more than once; only the
// first call counts.
func (t *Turn) End(reported ports.ToolCalls) {
	t.s.mu.Lock()
	delete(t.s.turns, t.token)
	t.s.mu.Unlock()

	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return
	}
	t.ended = true
	rep := Report{Task: t.task, Agent: t.agent, Kind: t.kind, Seen: t.calls, Reported: reported.Total(),
		ByID: len(reported.Calls) > 0}
	for _, c := range reported.Calls {
		if !t.seen[c.ID] {
			rep.Missed = append(rep.Missed, c)
		}
	}
	t.mu.Unlock()

	if t.s.OnTurnEnd != nil {
		t.s.OnTurnEnd(rep)
	}
}

// Report compares what a turn's hook saw with what its CLI reported running.
type Report struct {
	Task  domain.TaskID
	Agent domain.AgentID
	Kind  domain.AgentKind
	// Seen is how many calls the hook reported.
	Seen int
	// Reported is how many tool calls the CLI's output listed.
	Reported int
	// ByID says the CLI's calls were matched to the hook's one by one; if
	// not, only their numbers could be compared.
	ByID bool
	// Missed are the reported calls the hook never saw (ByID only).
	Missed []ports.ToolCall
}

// Problem says what is wrong with the turn's hook, or "" when every call the
// CLI reported went through it.
func (r Report) Problem() string {
	switch {
	case len(r.Missed) == r.Reported && r.Reported > 0 || !r.ByID && r.Reported > 0 && r.Seen == 0:
		return fmt.Sprintf("none of the %d tool calls the CLI reported went through %s's hook: it is not running for this agent",
			r.Reported, brand.Name)
	case len(r.Missed) > 0:
		var tools []string
		for _, c := range r.Missed {
			if !slices.Contains(tools, c.Tool) {
				tools = append(tools, c.Tool)
			}
		}
		return fmt.Sprintf("%d of the %d tool calls the CLI reported did not go through %s's hook (%s)",
			len(r.Missed), r.Reported, brand.Name, strings.Join(tools, ", "))
	}
	return ""
}
