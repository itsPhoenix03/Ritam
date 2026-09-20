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

	"projectorch/internal/domain"
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
}

// OpenSessionRegistry loads the registry at path, creating it on first save.
// An empty path keeps the registry in memory only, which tests and mock runs
// use.
//
// A file that exists but cannot be parsed is an error rather than an empty
// registry: silently starting over would orphan every session it recorded, and
// each agent would quietly lose its context.
func OpenSessionRegistry(path string) (*SessionRegistry, error) {
	r := &SessionRegistry{path: path, entries: map[SessionKey]Session{}}
	if path == "" {
		return r, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}
	if err != nil {
		return nil, fmt.Errorf("sessions: reading %s: %w", path, err)
	}
	var list []Session
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("sessions: %s is not valid (fix or remove it): %w", path, err)
	}
	for _, s := range list {
		r.entries[s.Key] = s
	}
	return r, nil
}

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
	return r.flushLocked()
}

// flushLocked writes the registry atomically: a temporary file in the same
// directory, renamed over the old one, so a crash mid-write can never leave a
// truncated registry behind.
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

	dir := filepath.Dir(r.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".sessions-*.tmp")
	if err != nil {
		return fmt.Errorf("sessions: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("sessions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("sessions: %w", err)
	}
	if err := os.Rename(tmpName, r.path); err != nil {
		os.Remove(tmpName)
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
