package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRedactRemovesCredentialShapedText guards a hard requirement: API keys,
// tokens, cookies and secrets must never reach the log, including when they
// arrive inside untrusted agent output.
func TestRedactRemovesCredentialShapedText(t *testing.T) {
	cases := []string{
		"sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
		"Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		"api_key=super-secret-value",
		"password: hunter2hunter2",
		"Set-Cookie: session=abc123def456; Path=/",
	}
	for _, in := range cases {
		got := Redact(in)
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("Redact(%q) = %q, expected redaction", in, got)
		}
	}
}

func TestRedactKeepsOrdinaryText(t *testing.T) {
	in := "The cookie is created with Path=/ but logout clears it using Path=/api."
	if got := Redact(in); got != in {
		t.Errorf("Redact rewrote ordinary text: %q", got)
	}
}

// TestFileLoggerRedactsAttributes checks the slog handler, not just the helper:
// a secret passed as a structured attribute must not land on disk either.
func TestFileLoggerRedactsAttributes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orch.log")
	log, err := New(Options{Level: "debug", Format: "text", File: path})
	if err != nil {
		t.Fatal(err)
	}
	log.Info("agent output", "api_key", "sk-live-should-not-appear", "taskId", "TASK-001")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if strings.Contains(body, "sk-live-should-not-appear") {
		t.Fatal("a secret reached the log file")
	}
	if !strings.Contains(body, "TASK-001") {
		t.Error("correlation fields must survive redaction")
	}
}

// TestCorrelationLoggers checks that the tracing fields are attached.
func TestCorrelationLoggers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orch.log")
	log, err := New(Options{Level: "debug", Format: "json", File: path})
	if err != nil {
		t.Fatal(err)
	}
	log.ForTask("TASK-007").ForAgent("opus").Info("working")
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	body := string(data)
	for _, want := range []string{"TASK-007", "opus", "taskId", "agentId"} {
		if !strings.Contains(body, want) {
			t.Errorf("log line %q missing %q", body, want)
		}
	}
}

func TestDiscardLoggerIsSafe(t *testing.T) {
	l := NewDiscard()
	l.Info("no output expected")
	if err := l.Close(); err != nil {
		t.Errorf("Close on a discard logger: %v", err)
	}
}
