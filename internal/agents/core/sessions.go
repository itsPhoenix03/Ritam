package core

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/fsutil"
)

// Persistent agent sessions.
//
// Both agent CLIs keep a conversation on disk and can continue it: Claude
// Code with `--session-id` / `--resume`, Codex with `exec resume <id>`. Using
// that, each agent keeps its own context across turns -- and across a task
// ending -- instead of being re-briefed from scratch by every one-shot call.
//
// The registry below is the orchestrator's record of which session belongs to
// which agent on which task. It is the only source of truth for that mapping:
// the CLIs' own "most recent session" shortcuts (`--continue`, `--last`) are
// never used, because "most recent" may be the human's own interactive session
// or another agent's.

// SessionState records how a session came to be what it is now.
type SessionState string

const (
	// SessionActive is a session that has carried this agent's conversation
	// from the start.
	SessionActive SessionState = "active"
	// SessionRebuilt is a replacement for a session the CLI could no longer
	// find. The agent was re-briefed, but lost whatever it had not written
	// into a protocol message.
	SessionRebuilt SessionState = "rebuilt"
)

// SessionKey identifies the conversation one agent holds about one task.
//
// It includes the task's creation time, not only its id: task ids restart at
// TASK-001 on every launch, so an id-only key would hand a new task an old
// task's conversation.
type SessionKey string

// SessionKeyFor builds the key for agent on task t.
func SessionKeyFor(t *domain.Task, agent domain.AgentID) SessionKey {
	if t == nil {
		return SessionKey("no-task|" + string(agent))
	}
	return SessionKey(fmt.Sprintf("%s|%s|%s",
		t.ID, t.CreatedAt.UTC().Format(time.RFC3339Nano), agent))
}

// Session is one agent's conversation with its CLI about one task.
type Session struct {
	Key  SessionKey       `json:"key"`
	Kind domain.AgentKind `json:"kind"`
	// ID is what the CLI resumes: a Claude session UUID or a Codex thread id.
	ID string `json:"id"`
	// WorkingDir is the directory the session was created in. Claude files
	// sessions by directory and cannot find one from anywhere else, so every
	// later turn must run from here.
	WorkingDir string       `json:"workingDir"`
	Model      string       `json:"model,omitempty"`
	Turns      int          `json:"turns"`
	State      SessionState `json:"state"`
	Created    time.Time    `json:"created"`
	LastUsed   time.Time    `json:"lastUsed"`
	// Previous lists ids this entry replaced, oldest first, so a lost session
	// can still be found on disk by a human who wants to read it.
	Previous []string `json:"previous,omitempty"`

	// The running totals the CLI last reported for this session. Claude
	// reports its cost, and Codex its token counts, as totals for the whole
	// session (restored when it is resumed), not for the turn just taken;
	// a turn's share is how far they grew. Summing the totals instead would
	// charge every turn for all the turns before it.
	ReportedCostUSD      float64 `json:"reportedCostUsd,omitempty"`
	ReportedInputTokens  int64   `json:"reportedInputTokens,omitempty"`
	ReportedCachedTokens int64   `json:"reportedCachedTokens,omitempty"`
	ReportedOutputTokens int64   `json:"reportedOutputTokens,omitempty"`
}

// NewSession builds a fresh, active session entry.
func NewSession(key SessionKey, kind domain.AgentKind, id, workingDir, model string) Session {
	now := time.Now()
	return Session{
		Key: key, Kind: kind, ID: id, WorkingDir: workingDir, Model: model,
		State: SessionActive, Created: now, LastUsed: now,
	}
}

// Replace swaps in a new CLI session id, keeping the old one in Previous.
func (s *Session) Replace(id string, state SessionState) {
	if s.ID != "" && s.ID != id {
		s.Previous = append(s.Previous, s.ID)
	}
	s.ID = id
	s.State = state
	s.Turns = 0
	s.ReportedCostUSD = 0
	s.ReportedInputTokens, s.ReportedCachedTokens, s.ReportedOutputTokens = 0, 0, 0
}

// CostSince returns how much the session's reported running cost grew since
// the last report, and records total as the new baseline.
func (s *Session) CostSince(total float64) float64 { return since(total, &s.ReportedCostUSD) }

// TokensSince does the same for running token totals.
func (s *Session) TokensSince(input, cached, output int64) (int64, int64, int64) {
	return since(input, &s.ReportedInputTokens), since(cached, &s.ReportedCachedTokens),
		since(output, &s.ReportedOutputTokens)
}

// since returns how far a running total has grown past *last and records it.
// A total below the last one means the CLI started counting afresh (a new
// session behind the same entry), so all of it is new.
func since[T int64 | float64](total T, last *T) T {
	d := total - *last
	if d < 0 {
		d = total
	}
	*last = total
	return d
}

// SessionRegistry is the persistent map from SessionKey to Session.
//
// It is safe for concurrent use: every adapter shares one registry.
type SessionRegistry struct {
	path    string
	mu      sync.Mutex
	entries map[SessionKey]Session
	// guard learns of every write, so the file is not taken for an
	// agent's edit (see FileGuard).
	guard *FileGuard
	// recovered is the warning about an unreadable file set aside on open.
	recovered string
}

// UseGuard routes the registry's writes through g.
func (r *SessionRegistry) UseGuard(g *FileGuard) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.guard = g
}

// OpenSessionRegistry loads the registry at path, creating it on first save.
// An empty path keeps the registry in memory only, which tests and mock runs
// use.
//
// A file that exists but cannot be parsed (an empty one is what a power cut
// mid-write can leave) is kept aside and the registry starts empty, with
// Recovered saying so. Refusing to start would leave the human nothing to do
// but delete it by hand; keeping it lets a human still find the sessions it
// recorded. Only a file that can be neither read nor moved is an error.
func OpenSessionRegistry(path string) (*SessionRegistry, error) {
	r := &SessionRegistry{path: path, entries: map[SessionKey]Session{}}
	if path == "" {
		return r, nil
	}
	list, notice, err := loadSessions(path)
	if err != nil {
		return nil, err
	}
	r.recovered = notice
	for _, s := range list {
		r.entries[s.Key] = s
	}
	return r, nil
}

// CheckSessionsFile keeps an unreadable sessions file at path aside, as
// OpenSessionRegistry would, and returns the warning for the human ("" when
// the file is fine or absent). Start-up calls it so the warning is shown
// and logged with the other start-up notices.
func CheckSessionsFile(path string) (string, error) {
	if path == "" {
		return "", nil
	}
	_, notice, err := loadSessions(path)
	return notice, err
}

// loadSessions reads the sessions file at path. One that does not parse is
// renamed to <path>.bad-<time> and reported in notice, with no sessions.
func loadSessions(path string) (list []Session, notice string, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("sessions: reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &list); err != nil {
		keptAs, mvErr := fsutil.SetAside(path)
		if mvErr != nil {
			return nil, "", fmt.Errorf("sessions: %s is not valid (%v) and could not be kept aside (fix or remove it): %w",
				path, err, mvErr)
		}
		return nil, fmt.Sprintf("warning: the agents' session record %s could not be read (%v); it was kept as %s "+
			"and agents start new conversations", path, err, keptAs), nil
	}
	return list, "", nil
}

// Recovered is the warning about an unreadable sessions file found when the
// registry was opened, "" when there was none.
func (r *SessionRegistry) Recovered() string { return r.recovered }

// Path reports where the registry is stored, "" when in memory only.
func (r *SessionRegistry) Path() string { return r.path }

// Get returns the session recorded for key.
func (r *SessionRegistry) Get(key SessionKey) (Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.entries[key]
	return s, ok
}

// Save records s and writes the registry to disk.
func (r *SessionRegistry) Save(s Session) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.entries[s.Key] = s
	return r.guard.Write(r.path, r.flushLocked)
}

// flushLocked writes the registry atomically: a temporary file in the same
// directory, flushed to disk and renamed over the old one (and the folder
// flushed where the platform allows), so neither a crash nor a power cut
// mid-write can leave a truncated or empty registry behind.
func (r *SessionRegistry) flushLocked() error {
	if r.path == "" {
		return nil
	}
	list := make([]Session, 0, len(r.entries))
	for _, s := range r.entries {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Key < list[j].Key })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := fsutil.WriteAtomic(r.path, data); err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	return nil
}

// NewSessionUUID returns a random RFC 4122 version-4 UUID, the form Claude
// Code's --session-id requires.
func NewSessionUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(fmt.Sprintf("sessions: no randomness available: %v", err))
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ResolveWorkingDir returns dir as an absolute path, defaulting to the
// orchestrator's own working directory. Sessions record it so later turns can
// run from exactly the same place.
func ResolveWorkingDir(dir string) string {
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			return wd
		}
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

// ResumableIn reports whether the session may be resumed by an agent
// confined to the workspace at root. Both CLIs resume a session in the
// folder it was made in, so a session recorded anywhere else -- a project
// that moved, a changed workspace.root, a sessions file edited by hand or by
// an agent -- would run the agent outside the workspace. Such a session is
// not resumed; the agent starts a new one. A session that recorded no
// folder runs in the agent's own, which is inside.
func (s Session) ResumableIn(root string) bool {
	return s.WorkingDir == "" || root == "" || config.Within(root, s.WorkingDir)
}

// WorkspaceRoot is the workspace an agent configured with cfg is confined
// to: the one it was given, else its own working folder.
func WorkspaceRoot(cfg config.AgentConfig) string {
	if cfg.WorkspaceRoot != "" {
		return cfg.WorkspaceRoot
	}
	return ResolveWorkingDir(cfg.WorkingDir)
}
