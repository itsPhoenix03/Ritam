package claude

import (
	"context"
	"strings"
	"testing"

	"projectorch/internal/config"
	"projectorch/internal/domain"
	"projectorch/internal/ports"
)

// No test in this file spawns a process or contacts a provider.

func testTask() *domain.Task {
	return &domain.Task{
		ID:             "TASK-001",
		Title:          "Fix the authentication cookie bug",
		Description:    "Logged-out users keep an authenticated session cookie.",
		AuthorityAgent: "opus",
		Participants:   []domain.AgentID{"opus", "codex"},
	}
}

func TestRenderPromptTellsTheAuthorityWhatItIs(t *testing.T) {
	m := domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "opus", Type: domain.MsgTaskAssignment, Content: "Start."}
	prompt := renderPrompt(testTask(), m, false)

	for _, want := range []string{"opus", "AUTHORITY", "TASK-001", "Fix the authentication cookie bug", headerMarker} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	// It must be told who it can address, or it cannot route anything.
	if !strings.Contains(prompt, "- codex") {
		t.Error("prompt must list the other participants by id")
	}
}

func TestRenderPromptTellsSupportingAgentsTheyCannotDecide(t *testing.T) {
	m := domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "Investigate."}
	prompt := renderPrompt(testTask(), m, false)

	if !strings.Contains(prompt, "SUPPORTING AGENT") {
		t.Error("supporting agents must be told their role")
	}
	if !strings.Contains(prompt, "FINAL_DECISION") || !strings.Contains(prompt, "may NOT") {
		t.Error("supporting agents must be told they cannot issue the final decision")
	}
	// Disagreement is a feature, and the agent has to know it is allowed.
	if !strings.Contains(prompt, "CHALLENGE") {
		t.Error("supporting agents must be told they may challenge")
	}
}

// TestRenderPromptOrientsAnAgentThatWasNeverBriefed covers the supporting
// agent's real situation: it receives no opening TASK_ASSIGNMENT, so the first
// thing it ever sees is a DELEGATE. That message alone has to tell it the
// task, its role and who it may reply to.
func TestRenderPromptOrientsAnAgentThatWasNeverBriefed(t *testing.T) {
	m := domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "Investigate the logout path."}
	prompt := renderPrompt(testTask(), m, false)

	for _, want := range []string{"TASK-001", "Fix the authentication cookie bug", "codex", "- opus", "Investigate the logout path."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("a delegated agent must be given %q", want)
		}
	}
}

func TestRenderPromptStatesTheReadOnlyConstraint(t *testing.T) {
	m := domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgImplement, Content: "Implement it."}

	ro := renderPrompt(testTask(), m, false)
	if !strings.Contains(ro, "READ-ONLY") {
		t.Error("a read-only agent must be told it cannot write")
	}
	rw := renderPrompt(testTask(), m, true)
	if strings.Contains(rw, "READ-ONLY") {
		t.Error("a write-enabled agent must not be told it is read-only")
	}
}

func TestRenderPromptCarriesRetrievedKnowledge(t *testing.T) {
	m := domain.Message{
		FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "Investigate.",
		Metadata: map[string]string{"knowledge": "- [INVESTIGATION] cookie path\n  Path=/ vs Path=/api"},
	}
	prompt := renderPrompt(testTask(), m, false)
	if !strings.Contains(prompt, "Path=/api") {
		t.Fatal("retrieved knowledge must reach the prompt: it is what replaces memory between one-shot runs")
	}
}

func TestParseOutputRoundTrip(t *testing.T) {
	raw := headerMarker + " type=ANSWER to=opus\n" +
		"The cookie is created with Path=/ but cleared with Path=/api.\n"

	msgs, err := parseOutput(raw)
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}
	if len(msgs) != 1 {
		t.Fatalf("got %d messages, want 1", len(msgs))
	}
	if msgs[0].Type != domain.MsgAnswer || msgs[0].ToAgent != "opus" {
		t.Fatalf("parsed %+v", msgs[0])
	}
	if !strings.Contains(msgs[0].Content, "Path=/api") {
		t.Errorf("content = %q", msgs[0].Content)
	}
}

func TestParseOutputHandlesMultipleBlocksAndProse(t *testing.T) {
	raw := "Sure, here is my response.\n" +
		"`" + headerMarker + " type=DELEGATE to=codex`\n" +
		"Investigate the logout path.\n" +
		headerMarker + " type=STATUS_UPDATE to=human\n" +
		"Investigation requested.\n"

	msgs, err := parseOutput(raw)
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want 2", len(msgs))
	}
	if msgs[0].Type != domain.MsgDelegate || msgs[1].Type != domain.MsgStatusUpdate {
		t.Fatalf("parsed %+v / %+v", msgs[0], msgs[1])
	}
	// Prose before the first header must not leak into a message body.
	if strings.Contains(msgs[0].Content, "Sure, here is my response") {
		t.Error("text before the first header must be ignored")
	}
}

// TestABadBlockDoesNotCostTheWholeReply: agent output is untrusted, so an
// unknown type is never passed through as itself, and a block with no
// recipient cannot be routed. But the rest of the reply is still delivered:
// discarding all of it once lost a whole broadcast from the authority over a
// made-up "DECISION" type.
func TestABadBlockDoesNotCostTheWholeReply(t *testing.T) {
	raw := headerMarker + " type=SEIZE_CONTROL to=opus\nhello\n" +
		headerMarker + " type=ANSWER\nlost\n" +
		headerMarker + " type=ANSWER to=human\nfine\n"
	msgs, err := parseOutput(raw)
	if err != nil {
		t.Fatalf("a reply with good blocks must not be rejected: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("got %d messages, want the unknown-type one and the good one: %+v", len(msgs), msgs)
	}
	if msgs[0].Type != domain.MsgInformation || msgs[0].Content != "hello" {
		t.Errorf("an unknown type must become INFORMATION, never anything stronger: %+v", msgs[0])
	}
	if msgs[1].Type != domain.MsgAnswer || msgs[1].Content != "fine" {
		t.Errorf("the good block must be delivered as written: %+v", msgs[1])
	}
	if strings.Contains(msgs[0].Content+msgs[1].Content, "lost") {
		t.Error("the body of a dropped block must not leak into another message")
	}
	problems := msgs[0].Metadata[ports.ProtocolProblemsMetadataKey]
	if !strings.Contains(problems, "SEIZE_CONTROL") || !strings.Contains(problems, "no recipient") {
		t.Errorf("the problems must be reported so the agent can be told: %q", problems)
	}
}

func TestAReplyWithNothingRoutableIsRejected(t *testing.T) {
	if _, err := parseOutput(headerMarker + " type=ANSWER\nhello\n"); err == nil {
		t.Fatal("a reply whose only block has no recipient must be rejected")
	}
}

func TestParseOutputWithNoHeaderYieldsNothing(t *testing.T) {
	msgs, err := parseOutput("I had a look around and everything seems fine.")
	if err != nil {
		t.Fatalf("plain prose should not be an error: %v", err)
	}
	if len(msgs) != 0 {
		t.Fatalf("got %d messages, want 0", len(msgs))
	}
}

func TestExtractResultReadsTheEnvelope(t *testing.T) {
	stdout := `{"type":"result","subtype":"success","is_error":false,` +
		`"session_id":"abc","result":"@@ORCH type=ANSWER to=opus\nhello"}`

	got, err := extractResult(stdout)
	if err != nil {
		t.Fatalf("extractResult: %v", err)
	}
	if !strings.Contains(got, "hello") {
		t.Errorf("result = %q", got)
	}
}

// TestAnEmptySuccessfulResultIsSilence: a turn that succeeded with no text is
// the agent saying nothing -- right when it was told no reply is needed. It
// used to be an error, which set the agent to ERROR and sent it a malformed-
// reply notice; an agent that owes a reply and stays silent is now chased by
// the orchestrator's reminders instead.
func TestAnEmptySuccessfulResultIsSilence(t *testing.T) {
	for name, stdout := range map[string]string{
		"envelope": `{"type":"result","subtype":"success","is_error":false,"result":""}`,
		"stream":   `{"type":"system"}` + "\n" + `{"type":"result","subtype":"success","is_error":false,"result":""}`,
	} {
		got, err := extractResult(stdout)
		if err != nil || got != "" {
			t.Errorf("%s: an empty successful result is silence, got %q, %v", name, got, err)
		}
	}
}

func TestExtractResultRejectsBadOutput(t *testing.T) {
	cases := map[string]string{
		"empty":     "   ",
		"not json":  "Please run `claude /login` to authenticate.",
		"cli error": `{"type":"result","subtype":"error","is_error":true,"result":"rate limited"}`,
	}
	for name, stdout := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := extractResult(stdout); err == nil {
				t.Fatal("expected an error: unparseable CLI output must never be treated as an agent reply")
			}
		})
	}
}

func TestBuildArgsEnforcesReadOnly(t *testing.T) {
	a := New("opus", config.AgentConfig{}, nil)
	args := strings.Join(a.buildArgs(), " ")

	for _, want := range []string{"-p", "--output-format stream-json", "--allowedTools Read,Grep,Glob", "--disallowedTools"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q\ngot: %s", want, args)
		}
	}
	for _, denied := range writeTools {
		if !strings.Contains(args, denied) {
			t.Errorf("write tool %q must be explicitly denied\ngot: %s", denied, args)
		}
	}
}

func TestBuildArgsHonoursAllowWrites(t *testing.T) {
	a := New("opus", config.AgentConfig{AllowWrites: true, Model: "opus",
		AllowedCommands: []string{"go test", "rm -rf build"}}, nil)
	argv := a.buildArgs()
	args := strings.Join(argv, " ")

	allowed, denied := "", ""
	for i, arg := range argv {
		switch arg {
		case "--allowedTools":
			allowed = argv[i+1]
		case "--disallowedTools":
			denied = argv[i+1]
		}
	}
	for _, want := range []string{"Edit", "Write", "Bash(go test:*)"} {
		if !strings.Contains(allowed, want) {
			t.Errorf("a write-enabled agent may use %s: allowed=%q", want, allowed)
		}
	}
	if strings.Contains(allowed, "rm -rf") {
		t.Errorf("a destructive command must never be allowed, even when configured: %q", allowed)
	}
	if strings.Contains(allowed, "Bash,") || strings.HasSuffix(allowed, "Bash") {
		t.Errorf("only named commands may run, not any shell command: %q", allowed)
	}
	for _, want := range []string{"Bash(rm:*)", "Bash(git push:*)", "Bash(curl:*)", "PowerShell"} {
		if !strings.Contains(denied, want) {
			t.Errorf("%s must be denied in write mode: %q", want, denied)
		}
	}
	if strings.Contains(denied, "Edit") {
		t.Errorf("editing must not be denied to a write-enabled agent: %q", denied)
	}
	if !strings.Contains(args, "--permission-mode acceptEdits") {
		t.Errorf("expected acceptEdits, got: %s", args)
	}
	if !strings.Contains(args, "--model opus") {
		t.Errorf("model override lost: %s", args)
	}
}

func TestSendBeforeStartIsRejected(t *testing.T) {
	a := New("opus", config.AgentConfig{}, nil)
	m := domain.Message{TaskID: "TASK-001", FromAgent: "human", ToAgent: "opus", Type: domain.MsgTaskAssignment}

	// Never started, so the adapter is stopped and must refuse rather than
	// silently spawning a process.
	if err := a.SendMessage(context.Background(), testTask(), m); err == nil {
		t.Fatal("expected a send to a stopped adapter to fail")
	}
	if a.Status() != domain.AgentStateStopped {
		t.Errorf("status = %s, want STOPPED", a.Status())
	}
}

func TestResolveReportsAMissingCLIClearly(t *testing.T) {
	a := New("opus", config.AgentConfig{Executable: "claude-does-not-exist-xyz"}, nil)

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to fail when the CLI is not installed")
	}
	if !strings.Contains(err.Error(), "executable") && !strings.Contains(err.Error(), "PATH") {
		t.Errorf("error should point at the missing executable, got: %v", err)
	}
	if a.Status() != domain.AgentStateUnavailable {
		t.Errorf("status = %s, want AGENT_UNAVAILABLE", a.Status())
	}
}

// TestBuildArgsNeverCarriesThePrompt: the prompt goes on stdin. As an argument
// it broke on NUL bytes and hit Windows' 32,767-character command-line cap.
func TestBuildArgsNeverCarriesThePrompt(t *testing.T) {
	a := New("opus", config.AgentConfig{}, nil)
	args := a.buildArgs()
	if len(args) == 0 || args[0] != "-p" {
		t.Fatalf("expected print mode first, got %v", args)
	}
	if args[1] != "--output-format" {
		t.Errorf("-p must not be followed by a positional prompt, got %q", args[1])
	}
}
