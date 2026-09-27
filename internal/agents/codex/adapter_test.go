package codex

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

func TestRenderPromptTellsSupportingAgentsTheirRole(t *testing.T) {
	m := domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "Investigate."}
	prompt := renderPrompt(testTask(), m, false)

	for _, want := range []string{"codex", "SUPPORTING AGENT", "TASK-001", headerMarker, "- opus"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if !strings.Contains(prompt, "CHALLENGE") {
		t.Error("a supporting agent must be told it may challenge the authority")
	}
}

// TestRenderPromptOrientsAnAgentThatWasNeverBriefed covers the supporting
// agent's real situation: it receives no opening TASK_ASSIGNMENT, so the first
// thing it ever sees is a DELEGATE. That message alone has to tell it the
// task, its role and who it may reply to.
func TestRenderPromptOrientsAnAgentThatWasNeverBriefed(t *testing.T) {
	m := domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "Investigate the logout path."}
	prompt := renderPrompt(testTask(), m, false)

	for _, want := range []string{"TASK-001", "Fix the authentication cookie bug", "- opus", "Investigate the logout path."} {
		if !strings.Contains(prompt, want) {
			t.Errorf("a delegated agent must be given %q", want)
		}
	}
}

func TestRenderPromptMarksAuthority(t *testing.T) {
	m := domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "opus", Type: domain.MsgTaskAssignment, Content: "Start."}
	prompt := renderPrompt(testTask(), m, false)
	if !strings.Contains(prompt, "AUTHORITY") {
		t.Error("the authority agent must be told it holds authority")
	}
}

func TestRenderPromptStatesTheSandbox(t *testing.T) {
	m := domain.Message{FromAgent: "opus", ToAgent: "codex", Type: domain.MsgImplement, Content: "Implement it."}

	if !strings.Contains(renderPrompt(testTask(), m, false), "read-only sandbox") {
		t.Error("a read-only agent must be told it cannot write")
	}
	if !strings.Contains(renderPrompt(testTask(), m, true), "write access to the project folder") {
		t.Error("a write-enabled agent must be told its sandbox")
	}
}

func TestRenderPromptCarriesRetrievedKnowledge(t *testing.T) {
	m := domain.Message{
		FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate, Content: "Investigate.",
		Metadata: map[string]string{"knowledge": "- [INVESTIGATION] cookie path\n  Path=/ vs Path=/api"},
	}
	if !strings.Contains(renderPrompt(testTask(), m, false), "Path=/api") {
		t.Fatal("retrieved knowledge must reach the prompt")
	}
}

func TestParseOutputRoundTrip(t *testing.T) {
	raw := headerMarker + " TYPE=ANSWER TO=opus\n" +
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
}

func TestParseOutputCaseInsensitiveKeys(t *testing.T) {
	msgs, err := parseOutput(headerMarker + " type=answer to=opus\nlowercase works too\n")
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Type != domain.MsgAnswer {
		t.Fatalf("parsed %+v", msgs)
	}
}

func TestParseOutputHandlesProseAndFencing(t *testing.T) {
	raw := "Here is what I found.\n" +
		"`" + headerMarker + " TYPE=CHALLENGE TO=opus`\n" +
		"I do not believe that is the root cause.\n"

	msgs, err := parseOutput(raw)
	if err != nil {
		t.Fatalf("parseOutput: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Type != domain.MsgChallenge {
		t.Fatalf("parsed %+v", msgs)
	}
	if strings.Contains(msgs[0].Content, "Here is what I found") {
		t.Error("text before the header must be ignored")
	}
}

// TestABadBlockDoesNotCostTheWholeReply: agent output is untrusted, so an
// unknown type is never passed through as itself, and a block with no
// recipient cannot be routed. But the rest of the reply is still delivered:
// discarding all of it once lost a whole broadcast from the authority over a
// made-up "DECISION" type.
func TestABadBlockDoesNotCostTheWholeReply(t *testing.T) {
	raw := headerMarker + " TYPE=SEIZE_CONTROL TO=opus\nhello\n" +
		headerMarker + " TYPE=ANSWER\nlost\n" +
		headerMarker + " TYPE=ANSWER TO=human\nfine\n"
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
	if _, err := parseOutput(headerMarker + " TYPE=ANSWER\nhello\n"); err == nil {
		t.Fatal("a reply whose only block has no recipient must be rejected")
	}
}

func TestBuildArgsUsesTheReadOnlySandbox(t *testing.T) {
	a := New("codex", config.AgentConfig{}, nil)
	args := a.buildArgs("C:\\tmp\\last.txt")
	joined := strings.Join(args, " ")

	for _, want := range []string{"exec", "--sandbox read-only", "--skip-git-repo-check", "--color never", "--output-last-message"} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q\ngot: %s", want, joined)
		}
	}
	// The prompt is read from stdin, not passed as an argument: on Windows the
	// `codex` shim is a .cmd file, and cmd.exe truncates a multi-line argument
	// at its first newline.
	if args[len(args)-1] != "-" {
		t.Errorf("prompt must come from stdin via a trailing %q, got %q", "-", args[len(args)-1])
	}
	if strings.Contains(joined, "\n") {
		t.Error("no argument may contain a newline")
	}
}

func TestBuildArgsHonoursAllowWrites(t *testing.T) {
	a := New("codex", config.AgentConfig{AllowWrites: true}, nil)
	joined := strings.Join(a.buildArgs("out.txt"), " ")
	if !strings.Contains(joined, "--sandbox workspace-write") {
		t.Errorf("expected the workspace-write sandbox, got: %s", joined)
	}
}

// TestConfiguredArgsCannotEscapeTheSandbox is a regression test for a real
// finding: a Codex agent auditing buildArgs through the orchestrator noticed
// that configured Args were appended after --sandbox, so a later flag could
// override it and a nominally read-only agent would run unsandboxed.
func TestConfiguredArgsCannotEscapeTheSandbox(t *testing.T) {
	escapes := []string{
		"--dangerously-bypass-approvals-and-sandbox",
		"--dangerously-bypass-hook-trust",
		"--approve-for-me",
		"--sandbox=danger-full-access",
		"-s",
		"--cd",
		"--output-last-message",
	}
	// The orchestrator's own flags legitimately appear in the output, so match
	// argument-by-argument rather than on the joined string.
	orchestratorOwned := map[string]bool{
		"exec": true, "--json": true,
		"--sandbox": true, "read-only": true, "workspace-write": true,
		"--skip-git-repo-check": true, "--color": true, "never": true,
		"--output-last-message": true, "out.txt": true, "-": true,
	}

	for _, bad := range escapes {
		t.Run(bad, func(t *testing.T) {
			a := New("codex", config.AgentConfig{Args: []string{bad, "danger-full-access"}}, nil)
			args := a.buildArgs("out.txt")

			for i, got := range args {
				if orchestratorOwned[got] {
					continue
				}
				t.Fatalf("argument %d = %q survived from config; the flag and its value must both be dropped\ngot: %v",
					i, got, args)
			}
			// The orphaned value must not survive either: Codex reads the
			// first positional argument as the prompt.
			for _, got := range args {
				if got == "danger-full-access" {
					t.Fatalf("the dropped flag's value leaked through as a positional: %v", args)
				}
			}

			var sandboxIdx = -1
			for i, v := range args {
				if v == "--sandbox" {
					sandboxIdx = i
				}
			}
			if sandboxIdx < 0 || args[sandboxIdx+1] != "read-only" {
				t.Fatalf("the read-only sandbox must survive\ngot: %v", args)
			}
		})
	}
}

func TestHarmlessConfiguredArgsAreKept(t *testing.T) {
	a := New("codex", config.AgentConfig{Args: []string{"--enable", "some-feature"}}, nil)
	joined := strings.Join(a.buildArgs("out.txt"), " ")
	if !strings.Contains(joined, "--enable some-feature") {
		t.Errorf("harmless args should be passed through, got: %s", joined)
	}
}

// TestOrchestratorFlagsWinOverConfiguredArgs checks ordering: even an
// unfiltered duplicate must not take precedence, because a later flag wins.
func TestOrchestratorFlagsWinOverConfiguredArgs(t *testing.T) {
	a := New("codex", config.AgentConfig{Args: []string{"--color", "always"}}, nil)
	args := a.buildArgs("out.txt")

	lastColor := -1
	for i, v := range args {
		if v == "--color" {
			lastColor = i
		}
	}
	if lastColor < 0 || args[lastColor+1] != "never" {
		t.Errorf("the orchestrator's --color must come last, got: %v", args)
	}
}

func TestSendBeforeStartIsRejected(t *testing.T) {
	a := New("codex", config.AgentConfig{}, nil)
	m := domain.Message{TaskID: "TASK-001", FromAgent: "opus", ToAgent: "codex", Type: domain.MsgDelegate}

	if err := a.SendMessage(context.Background(), testTask(), m); err == nil {
		t.Fatal("expected a send to a stopped adapter to fail")
	}
	if a.Status() != domain.AgentStateStopped {
		t.Errorf("status = %s, want STOPPED", a.Status())
	}
}

func TestResolveReportsAMissingCLIClearly(t *testing.T) {
	a := New("codex", config.AgentConfig{Executable: "codex-does-not-exist-xyz"}, nil)

	err := a.Start(context.Background())
	if err == nil {
		t.Fatal("expected Start to fail when the CLI is not installed")
	}
	if a.Status() != domain.AgentStateUnavailable {
		t.Errorf("status = %s, want AGENT_UNAVAILABLE", a.Status())
	}
}

func TestLastMessageFileIsCreatedAndRemoved(t *testing.T) {
	path, cleanup, err := newLastMessageFile("", "codex")
	if err != nil {
		t.Fatalf("newLastMessageFile: %v", err)
	}
	if path == "" {
		t.Fatal("expected a path")
	}
	cleanup()
}

// TestAWriteEnabledCodexWritesOnlyInTheWorkspace: the workspace-write sandbox
// would also let commands write to temp folders; that is turned off, on new
// threads and on resumed ones.
func TestAWriteEnabledCodexWritesOnlyInTheWorkspace(t *testing.T) {
	a := New("codex", config.AgentConfig{AllowWrites: true, WorkingDir: "proj"}, nil)
	for name, args := range map[string]string{
		"exec":   strings.Join(a.buildArgs("out.txt"), " "),
		"resume": strings.Join(a.buildResumeArgs("thread-1", "out.txt"), " "),
	} {
		for _, want := range []string{"exclude_tmpdir_env_var=true", "exclude_slash_tmp=true", "network_access=false"} {
			if !strings.Contains(args, want) {
				t.Errorf("%s: missing %s\n%s", name, want, args)
			}
		}
		if !strings.HasSuffix(args, "-") || !strings.Contains(args, "out.txt") {
			t.Errorf("%s: the reply file and stdin prompt must still be passed: %s", name, args)
		}
	}
	ro := strings.Join(New("codex", config.AgentConfig{}, nil).buildArgs("out.txt"), " ")
	if strings.Contains(ro, "sandbox_workspace_write") {
		t.Error("a read-only agent needs no write confinement")
	}
}
