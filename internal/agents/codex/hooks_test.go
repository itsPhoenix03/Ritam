package codex

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"ritam/internal/config"
	"ritam/internal/domain"
	"ritam/internal/ports"
	"ritam/internal/review"
)

// hookModeEnv switches the test binary into ritam's hook: the review server
// in these tests names the test binary as the hook program.
const hookModeEnv = "RITAM_TEST_HOOK_MODE"

// runHookMode is the test binary as `ritam hook ...`: os.Args[1] is "hook".
func runHookMode() int {
	return review.RunHook(os.Args[2:], os.Stdin, os.Stdout, os.Stderr, os.Getenv)
}

// hookCommandOf finds the hook command line in a CLI's -c overrides.
var hookCommandOf = regexp.MustCompile(`^hooks\.PreToolUse=\[\{matcher="\*",hooks=\[\{type="command",command='([^']*)',timeout=(\d+)\}\]\}\]$`)

// splitCommandLine splits the command lines hookCommandLine writes: a
// program, quoted or not, then plain words.
func splitCommandLine(line string) []string {
	if q := line[0]; q == '"' || q == '\'' {
		end := strings.IndexByte(line[1:], q) + 1
		return append([]string{line[1:end]}, strings.Fields(line[end+1:])...)
	}
	return strings.Fields(line)
}

// runFakeHookedCLI behaves like Codex running one shell command and one web
// search: it runs the hook from its -c override before the command, as
// Codex's documentation describes, and streams both items.
func runFakeHookedCLI() {
	_ = readAll(os.Stdin)
	var command string
	for i, a := range os.Args {
		if a == "-c" && i+1 < len(os.Args) {
			if m := hookCommandOf.FindStringSubmatch(os.Args[i+1]); m != nil {
				command = m[1]
			}
		}
	}
	if command == "" {
		fakeLog("no hook in the config overrides")
		os.Exit(1)
	}
	fakeLog(fmt.Sprintf("bypass=%v", slices.Contains(os.Args, "--dangerously-bypass-hook-trust")))
	out := func(s string) { _, _ = os.Stdout.WriteString(s + "\n") }
	out(`{"type":"thread.started","thread_id":"thr-1"}`)
	out(`{"type":"turn.started"}`)

	argv := splitCommandLine(command)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), hookModeEnv+"=1")
	cmd.Stdin = strings.NewReader(`{"session_id":"thr-1","cwd":"/work","hook_event_name":"PreToolUse",` +
		`"model":"gpt-6-sol","turn_id":"t-1","permission_mode":"default","tool_name":"Bash",` +
		`"tool_use_id":"call_1","tool_input":{"command":"cargo test"}}`)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			fakeLog("hook did not start: " + err.Error())
			os.Exit(1)
		}
		code = ee.ExitCode()
	}
	fakeLog(fmt.Sprintf("hook exit=%d %s", code, strings.TrimSpace(stderr.String())))

	out(`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"cargo test"}}`)
	out(`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"cargo test"}}`)
	out(`{"type":"item.started","item":{"id":"item_2","type":"web_search"}}`)
	out(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":5}}`)
	writeReply()
}

// stubHooks is a hook monitor that only names its hook.
type stubHooks struct{ program string }

func (s stubHooks) HookCommand(domain.AgentKind) ports.HookCommand {
	return ports.HookCommand{Program: s.program, Args: []string{"hook", "pretool"}, TimeoutSeconds: 30}
}

func (stubHooks) BeginTurn(domain.TaskID, domain.AgentID, domain.AgentKind) ports.HookTurn { return nil }

// overrideOf returns the -c value that installs the hook, or "".
func overrideOf(args []string) string {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "hooks.") {
			return args[i+1]
		}
	}
	return ""
}

// TestCodexIsToldToRunRitamsHookBeforeEveryToolCall: on a new thread and a
// resumed one, the hook goes in as a config override, with trust bypassed,
// before the prompt and thread arguments.
func TestCodexIsToldToRunRitamsHookBeforeEveryToolCall(t *testing.T) {
	ws := t.TempDir()
	a := New("x", config.AgentConfig{AllowWrites: true, WorkingDir: ws, WorkspaceRoot: ws}, nil)
	a.UseHooks(stubHooks{program: `C:\tools\ritam.exe`})
	for name, args := range map[string][]string{
		"exec":   a.buildArgs("reply.txt"),
		"resume": a.buildResumeArgs("thr-1", "reply.txt"),
	} {
		m := hookCommandOf.FindStringSubmatch(overrideOf(args))
		if m == nil {
			t.Fatalf("%s: no hook override in %v", name, args)
		}
		if !strings.HasSuffix(m[1], "ritam.exe hook pretool") || m[2] != "30" {
			t.Errorf("%s: hook command %q, timeout %s", name, m[1], m[2])
		}
		if !slices.Contains(args, "--dangerously-bypass-hook-trust") {
			t.Errorf("%s: a headless turn cannot trust the hook in Codex's UI, so trust is bypassed: %v", name, args)
		}
		if args[len(args)-1] != "-" {
			t.Errorf("%s: the prompt marker must stay last: %v", name, args)
		}
	}

	plain := New("x", config.AgentConfig{AllowWrites: true}, nil)
	if args := plain.buildArgs("reply.txt"); overrideOf(args) != "" || slices.Contains(args, "--dangerously-bypass-hook-trust") {
		t.Errorf("without hooks, nothing about hooks: %v", args)
	}
}

// TestHookTrustIsNotBypassedWhenTheWorkspaceHasHooksOfItsOwn: bypassing
// trust would run those unreviewed, outside the sandbox.
func TestHookTrustIsNotBypassedWhenTheWorkspaceHasHooksOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name, file, content string
		bypass              bool
	}{
		{"hooks.json", "hooks.json", `{"hooks":{}}`, false},
		{"a hooks table in config.toml", "config.toml", "model = \"x\"\n[[hooks.PreToolUse]]\nmatcher = \"*\"\n", false},
		{"a hooks key in config.toml", "config.toml", "hooks.PreToolUse = []\n", false},
		{"config.toml without hooks", "config.toml", "model = \"x\"\n# hooks are set elsewhere\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			_ = os.MkdirAll(filepath.Join(ws, ".codex"), 0o700)
			_ = os.WriteFile(filepath.Join(ws, ".codex", tc.file), []byte(tc.content), 0o600)
			a := New("x", config.AgentConfig{AllowWrites: true, WorkingDir: ws, WorkspaceRoot: ws}, nil)
			a.UseHooks(stubHooks{program: "/opt/ritam"})
			args := a.buildArgs("reply.txt")
			if got := slices.Contains(args, "--dangerously-bypass-hook-trust"); got != tc.bypass {
				t.Errorf("bypass = %v, want %v: %v", got, tc.bypass, args)
			}
			if overrideOf(args) == "" {
				t.Error("ritam's hook is still configured, so the turn's check can tell it did not run")
			}
		})
	}
}

func TestTheHookCommandLineSurvivesTheShellAndTOML(t *testing.T) {
	cmd := func(p string) ports.HookCommand { return ports.HookCommand{Program: p, Args: []string{"hook", "pretool"}} }
	for _, tc := range []struct{ program, goos, want string }{
		{`C:\tools\ritam.exe`, "windows", `C:\tools\ritam.exe hook pretool`},
		{`C:\Program Files\ritam\ritam.exe`, "windows", `"C:\Program Files\ritam\ritam.exe" hook pretool`},
		{"/opt/ritam", "linux", "'/opt/ritam' hook pretool"},
		{"/home/o'neil/ritam", "darwin", `'/home/o'\''neil/ritam' hook pretool`},
	} {
		if got := hookCommandLine(cmd(tc.program), tc.goos); got != tc.want {
			t.Errorf("%s on %s: %q, want %q", tc.program, tc.goos, got, tc.want)
		}
	}
	if got := tomlString(`"C:\a b\ritam.exe" hook`); got != `'"C:\a b\ritam.exe" hook'` {
		t.Errorf("a Windows path keeps its backslashes in a literal string: %s", got)
	}
	if got := tomlString(`'/o'\''n/ritam' hook`); got != `"'/o'\\''n/ritam' hook"` {
		t.Errorf("a quote needs a basic string: %s", got)
	}
}

func TestToolItemsAreCountedOnceAndWebSearchesNot(t *testing.T) {
	var c toolItems
	for _, l := range []string{
		`{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"ls"}}`,
		`{"type":"item.completed","item":{"id":"item_2","type":"file_change"}}`,
		`{"type":"item.started","item":{"id":"item_3","type":"mcp_tool_call"}}`,
		`{"type":"item.started","item":{"id":"item_4","type":"web_search"}}`,
		`{"type":"item.completed","item":{"id":"item_5","type":"agent_message","text":"done"}}`,
		`not json "item.started"`,
	} {
		c.observe(l)
	}
	if n := c.count(); n != 3 {
		t.Errorf("want 3 tool calls, got %d", n)
	}
}

// TestACodexToolCallIsRecordedAgainstItsTurn: the fake Codex runs the hook
// from its config override, the hook reaches a real review server with the
// turn's token, and the turn's report compares the number of calls.
func TestACodexToolCallIsRecordedAgainstItsTurn(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	srv, err := review.Start(exe, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	var (
		mu      sync.Mutex
		calls   []review.Call
		reports []review.Report
	)
	srv.OnCall = func(c review.Call) { mu.Lock(); calls = append(calls, c); mu.Unlock() }
	srv.OnTurnEnd = func(r review.Report) { mu.Lock(); reports = append(reports, r); mu.Unlock() }

	a, cliLog, _ := fakeAdapter(t, "hooked")
	a.UseHooks(srv)
	if err := a.SendTask(context.Background(), testTask(), msg("Run the tests.")); err != nil {
		t.Fatal(err)
	}
	if o := nextOutbound(t, a); o.Err != nil {
		t.Fatalf("turn failed: %v", o.Err)
	}
	waitFor(t, "the turn's hook report", func() bool { mu.Lock(); defer mu.Unlock(); return len(reports) == 1 })

	log := readFile(cliLog)
	if !strings.Contains(log, "hook exit=0") || !strings.Contains(log, "bypass=true") {
		t.Errorf("the hook must run and pass: %s", log)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 || calls[0].Kind != domain.AgentKindCodex || calls[0].Tool != "Bash" ||
		calls[0].Summary != "cargo test" || calls[0].Task != testTask().ID {
		t.Errorf("recorded calls: %+v", calls)
	}
	if rep := reports[0]; rep.Seen != 1 || rep.Reported != 1 || rep.ByID || rep.Problem() != "" {
		t.Errorf("report: %+v (%s)", rep, rep.Problem())
	}
	if len(a.runner.Env) != 0 {
		t.Errorf("the turn's token must stay in that turn's process: %v", a.runner.Env)
	}
}
