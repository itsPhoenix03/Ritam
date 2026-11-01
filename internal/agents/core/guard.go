package core

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"ritam/internal/domain"
)

// FileGuard keeps ritam's own files the way ritam last wrote them.
//
// Write-enabled agents may write anywhere in the workspace, and ritam's
// settings, sessions and team live there too. Claude Code's file tools are
// denied those files outright, but its shell commands and Codex's sandboxed
// commands are not: the Codex sandbox has no per-path deny. Edited, those
// files would decide what agents run as next time -- the args and executable
// in ritam.yaml, the folder a session resumes in, who may write.
//
// So ritam remembers every guarded file as it last read or wrote it, and
// after each agent turn compares. A file that changed without ritam writing
// it is put back, the agent's version kept aside for the human to read, and
// the human told. ritam's own writes go through Write, which updates what
// is remembered under the same lock the check takes, so a write of ritam's
// is never mistaken for an agent's.
//
// The database is not guarded this way: ritam writes it continuously. Only
// Claude's deny rules protect it.
type FileGuard struct {
	mu    sync.Mutex
	known map[string]*[]byte // nil: the file did not exist
	// keepDir is where an agent's version of a guarded file is kept.
	keepDir string
	// OnRestore is told of each file put back, by the turn of agent, with
	// where the agent's version was kept ("" when it was a deletion).
	OnRestore func(path string, agent domain.AgentID, kept string)
}

// NewFileGuard guards paths as they are now. keepDir is where an agent's
// version of a file is kept when it is put back; "" keeps nothing.
func NewFileGuard(keepDir string, paths ...string) *FileGuard {
	g := &FileGuard{known: map[string]*[]byte{}, keepDir: keepDir}
	for _, p := range paths {
		if p != "" {
			g.known[filepath.Clean(p)] = read(p)
		}
	}
	return g
}

// Write runs ritam's own write of path, then remembers the result.
// A nil guard, or a path it does not guard, just writes.
func (g *FileGuard) Write(path string, write func() error) error {
	if g == nil {
		return write()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	err := write()
	if _, ok := g.known[filepath.Clean(path)]; ok {
		g.known[filepath.Clean(path)] = read(path)
	}
	return err
}

// Check puts back every guarded file that changed since ritam last read or
// wrote it, after a turn of agent, and returns the paths put back.
func (g *FileGuard) Check(agent domain.AgentID) []string {
	if g == nil {
		return nil
	}
	type restore struct{ path, kept string }
	var done []restore
	g.mu.Lock()
	for path, want := range g.known {
		got := read(path)
		if same(got, want) {
			continue
		}
		kept := g.keep(path, got)
		var err error
		if want == nil {
			err = os.Remove(path)
		} else {
			err = writeFileAtomic(path, *want)
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			// Not put back: remember what is there now, so the human is
			// told once rather than after every turn.
			g.known[path] = got
		}
		done = append(done, restore{path, kept})
	}
	g.mu.Unlock()

	// Told outside the lock: whoever is told may write a guarded file.
	paths := make([]string, len(done))
	for i, r := range done {
		paths[i] = r.path
		if g.OnRestore != nil {
			g.OnRestore(r.path, agent, r.kept)
		}
	}
	return paths
}

// keep saves an agent's version of path in keepDir, returning where.
func (g *FileGuard) keep(path string, data *[]byte) string {
	if g.keepDir == "" || data == nil {
		return ""
	}
	if err := os.MkdirAll(g.keepDir, 0o700); err != nil {
		return ""
	}
	name := time.Now().Format("20060102-150405.000") + "-" + strings.TrimPrefix(filepath.Base(path), ".")
	kept := filepath.Join(g.keepDir, name)
	if os.WriteFile(kept, *data, 0o600) != nil {
		return ""
	}
	return kept
}

func read(path string) *[]byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return &data
}

func same(a, b *[]byte) bool {
	if a == nil || b == nil {
		return a == b
	}
	return bytes.Equal(*a, *b)
}

// writeFileAtomic writes data to path through a temporary file and a
// rename, so a reader never sees half a file.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("restoring %s: %w", path, err)
	}
	return nil
}
