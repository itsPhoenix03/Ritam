// Package logging provides the structured logger used across the
// orchestrator, plus redaction helpers so that credentials never reach disk.
package logging

import (
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"

	"projectorch/internal/domain"
)

// Logger wraps slog with the orchestrator's correlation fields.
type Logger struct {
	*slog.Logger
	closer io.Closer
}

// Options configures logger construction.
type Options struct {
	Level  string
	Format string
	// File, when set, receives the log output. The TUI owns the terminal, so
	// writing to a file is the normal mode of operation.
	File string
}

// New builds a Logger. When a file is configured and cannot be opened, logging
// falls back to stderr rather than failing startup.
func New(o Options) (*Logger, error) {
	var (
		w      io.Writer = os.Stderr
		closer io.Closer
	)
	if o.File != "" {
		f, err := os.OpenFile(o.File, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			w, closer = f, f
		}
	}

	opts := &slog.HandlerOptions{Level: parseLevel(o.Level), ReplaceAttr: redactAttr}
	var h slog.Handler
	if strings.EqualFold(o.Format, "json") {
		h = slog.NewJSONHandler(w, opts)
	} else {
		h = slog.NewTextHandler(w, opts)
	}
	return &Logger{Logger: slog.New(h), closer: closer}, nil
}

// NewDiscard returns a logger that writes nowhere. Useful in tests.
func NewDiscard() *Logger {
	return &Logger{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// Close releases the log file, if any.
func (l *Logger) Close() error {
	if l.closer != nil {
		return l.closer.Close()
	}
	return nil
}

// ForTask returns a logger tagged with a task id.
func (l *Logger) ForTask(id domain.TaskID) *Logger {
	return &Logger{Logger: l.With("taskId", string(id)), closer: nil}
}

// ForAgent returns a logger tagged with an agent id.
func (l *Logger) ForAgent(id domain.AgentID) *Logger {
	return &Logger{Logger: l.With("agentId", string(id)), closer: nil}
}

// ForMessage returns a logger tagged with task, agent and message ids so that
// every important operation is traceable.
func (l *Logger) ForMessage(m *domain.Message) *Logger {
	return &Logger{Logger: l.With(
		"taskId", string(m.TaskID),
		"agentId", string(m.FromAgent),
		"messageId", string(m.ID),
		"messageType", string(m.Type),
	)}
}

func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// secretKeys are attribute names whose values are always dropped.
var secretKeys = map[string]bool{
	"apikey": true, "api_key": true, "token": true, "accesstoken": true,
	"access_token": true, "refresh_token": true, "authorization": true,
	"cookie": true, "set-cookie": true, "password": true, "secret": true,
	"anthropic_api_key": true, "openai_api_key": true, "session": true,
}

// secretPatterns match credential-shaped substrings inside free text, such as
// agent output echoed into a log line.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)sk-[A-Za-z0-9_\-]{16,}`),
	regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._\-]{16,}`),
	regexp.MustCompile(`(?i)(api[_-]?key|token|password|secret)\s*[:=]\s*\S+`),
	regexp.MustCompile(`(?i)(set-)?cookie\s*:\s*[^\s;]+`),
}

// Redact removes credential-shaped content from a string. Agent output is
// untrusted and may contain anything, so it is scrubbed before it is logged.
func Redact(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, "[REDACTED]")
	}
	return s
}

func redactAttr(_ []string, a slog.Attr) slog.Attr {
	if secretKeys[strings.ToLower(a.Key)] {
		return slog.String(a.Key, "[REDACTED]")
	}
	if a.Value.Kind() == slog.KindString {
		return slog.String(a.Key, Redact(a.Value.String()))
	}
	return a
}
