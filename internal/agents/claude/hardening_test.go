package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/core"
	"ritam/internal/config"
)

// TestConfiguredArgsCannotLoosenThePermissions: a later flag wins, so an
// args entry in ritam.yaml -- which an agent might have written -- must not
// be able to skip, widen or replace what ritam decides.
func TestConfiguredArgsCannotLoosenThePermissions(t *testing.T) {
	for _, bad := range [][]string{
		{"--dangerously-skip-permissions"},
		{"--permission-mode", "bypassPermissions"},
		{"--permission-mode=bypassPermissions"},
		{"--allowedTools", "Bash"},
		{"--allowed-tools=Bash"},
		{"--disallowedTools", "Read"},
		{"--add-dir", `C:\`},
		{"--settings", "evil.json"},
		{"--setting-sources", "user,project,local"},
		{"--mcp-config", "servers.json"},
		{"--session-id", "00000000-0000-4000-8000-000000000000"},
		{"--resume", "someone-elses"},
		{"--continue"},
		{"-c"},
		{"-p"},
		{"-cp"},
		{"-r", "x"},
		{"--AllowedTools", "Bash"},
		{"--tools", "Bash"},
	} {
		a := New("opus", config.AgentConfig{AllowWrites: true, Args: bad}, nil)
		args := a.buildArgs()
		for _, arg := range bad {
			if arg == "-p" {
				continue // ritam's own print flag
			}
			n := 0
			for _, got := range args {
				if got == arg {
					n++
				}
			}
			if n > 0 && !ritamsOwn(arg) {
				t.Errorf("configured %v reached the CLI: %v", bad, args)
				break
			}
		}
		if strings.Contains(strings.Join(args, " "), "bypassPermissions") {
			t.Errorf("configured %v set the permission mode: %v", bad, args)
		}
		if n := count(args, "--allowedTools"); n != 1 {
			t.Errorf("configured %v: want ritam's one --allowedTools, got %d in %v", bad, n, args)
		}
	}
}

// ritamsOwn reports whether ritam passes arg itself.
func ritamsOwn(arg string) bool {
	return slices.Contains([]string{"--permission-mode", "--allowedTools", "--disallowedTools", "--setting-sources",
		"--strict-mcp-config", "--settings", "Read", "Bash", ""}, arg)
}

func count(args []string, s string) int {
	n := 0
	for _, a := range args {
		if a == s {
			n++
		}
	}
	return n
}

// TestHarmlessArgsComeFirstAndSurvive: what is not refused is passed on,
// before ritam's own flags, so ritam's win any conflict.
func TestHarmlessArgsComeFirstAndSurvive(t *testing.T) {
	a := New("opus", config.AgentConfig{Args: []string{"--effort", "high", "stray", "--append-system-prompt=Be brief"}}, nil)
	args := a.buildArgs()
	if args[0] != "--effort" || args[1] != "high" || args[2] != "--append-system-prompt=Be brief" {
		t.Errorf("harmless args lead, stray positionals dropped: %v", args)
	}
	if slices.Contains(args, "stray") {
		t.Errorf("a positional would be read as the prompt: %v", args)
	}
	if i := slices.Index(args, "--permission-mode"); i < slices.Index(args, "--effort") {
		t.Errorf("ritam's flags must come after the configured ones: %v", args)
	}
}

// TestNoUserOrProjectSettingsAreLoaded: allow rules, hooks and MCP servers in
// ~/.claude or the project's .claude would widen what the grant allows.
func TestNoUserOrProjectSettingsAreLoaded(t *testing.T) {
	args := New("opus", config.AgentConfig{AllowWrites: true}, nil).buildArgs()
	i := slices.Index(args, "--setting-sources")
	if i < 0 || args[i+1] != "" {
		t.Errorf("want --setting-sources with no source: %v", args)
	}
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Errorf("want --strict-mcp-config: %v", args)
	}
}

// TestRitamsOwnFilesCannotBeEdited: the file tools may not touch ritam's
// settings, database, sessions or team.
func TestRitamsOwnFilesCannotBeEdited(t *testing.T) {
	root := filepath.Join(t.TempDir(), "my project")
	cfg := config.AgentConfig{AllowWrites: true,
		Protected: []string{filepath.Join(root, ".ritam"), filepath.Join(root, "ritam.yaml")}}
	_, denied := toolFlags(New("opus", cfg, nil).buildArgs())
	for _, p := range cfg.Protected {
		r := rulePath(p)
		if !strings.Contains(denied, "Edit("+r+")") || !strings.Contains(denied, "Edit("+r+"/**)") {
			t.Errorf("%s must be denied to the file tools: %s", p, denied)
		}
	}
}

func TestRulePathIsAbsoluteInPOSIXForm(t *testing.T) {
	if runtime.GOOS == "windows" {
		if got := rulePath(`D:\Web Projects\app\.ritam`); got != "//d/Web Projects/app/.ritam" {
			t.Errorf("got %q", got)
		}
		return
	}
	if got := rulePath("/home/me/app/.ritam"); got != "//home/me/app/.ritam" {
		t.Errorf("got %q", got)
	}
}

// TestSettingsFile: the sandbox where the platform has one, and reads held
// to the working folder when confined, written to the scratch folder.
func TestSettingsFile(t *testing.T) {
	scratch := t.TempDir()
	a := New("opus", config.AgentConfig{AllowWrites: true, ConfineReads: true, ScratchDir: scratch}, nil)
	args := a.buildArgs()
	i := slices.Index(args, "--settings")
	if i < 0 {
		t.Fatalf("want --settings: %v", args)
	}
	data, err := os.ReadFile(args[i+1])
	if err != nil {
		t.Fatalf("--settings must name a file in the scratch folder: %v", err)
	}
	var s struct {
		Sandbox *struct {
			Enabled                  bool `json:"enabled"`
			AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
		} `json:"sandbox"`
		Permissions struct {
			BlockReads bool `json:"blockReadsOutsideWorkingDirectories"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(data, &s); err != nil {
		t.Fatal(err)
	}
	if !s.Permissions.BlockReads {
		t.Errorf("confined reads must block reads outside the working folder: %s", data)
	}
	if (s.Sandbox != nil) != a.RunsInSandbox() {
		t.Errorf("the sandbox is turned on exactly where it is declared: %s", data)
	}
	if s.Sandbox != nil && (!s.Sandbox.Enabled || s.Sandbox.AllowUnsandboxedCommands) {
		t.Errorf("sandboxed commands must not be retried outside it: %s", data)
	}

	if runtime.GOOS == "windows" {
		a = New("opus", config.AgentConfig{AllowWrites: true}, nil)
		if slices.Contains(a.buildArgs(), "--settings") {
			t.Error("with nothing to set, no --settings")
		}
	}
}

// TestASessionOutsideTheWorkspaceIsNotResumed: a moved project, a changed
// workspace root or an edited sessions file must not send the agent
// somewhere else.
func TestASessionOutsideTheWorkspaceIsNotResumed(t *testing.T) {
	reg, _ := core.OpenSessionRegistry("")
	ws := t.TempDir()
	a, log := sessionAdapter(t, reg, ws)
	a.cfg.WorkspaceRoot = ws
	tk := sessionTask(time.Now())
	turn(t, a, tk, msg("one"))

	key := core.SessionKeyFor(tk, "opus")
	sess, _ := reg.Get(key)
	old := sess.ID
	sess.WorkingDir = t.TempDir() // elsewhere
	if err := reg.Save(sess); err != nil {
		t.Fatal(err)
	}
	turn(t, a, tk, msg("two"))

	cs := calls(t, log)
	last := cs[len(cs)-1]
	if strings.Contains(last.args, "--resume") || !sameDir(last.cwd, ws) {
		t.Errorf("the session outside the workspace must not be resumed: %+v", last)
	}
	now, _ := reg.Get(key)
	if now.ID == old || !slices.Contains(now.Previous, old) || !sameDir(now.WorkingDir, ws) {
		t.Errorf("a new session in the workspace replaces it: %+v", now)
	}
}

// TestOnlyAllowedArgsReachClaude: flags that replace ritam's instructions,
// write files or spend differently are dropped, with their values.
func TestOnlyAllowedArgsReachClaude(t *testing.T) {
	a := New("opus", config.AgentConfig{AllowWrites: true, Args: []string{
		"--system-prompt", "x", "--system-prompt-file", `C:\x.txt`, "--debug-file", `C:\x.log`,
		"--max-budget-usd", "1000", "--effort", "high",
	}}, nil)
	got := strings.Join(a.buildArgs(), " ")
	for _, bad := range []string{"--system-prompt", `C:\x.txt`, "--debug-file", "1000"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q reached Claude: %s", bad, got)
		}
	}
	if !strings.Contains(got, "--effort high") {
		t.Errorf("--effort should be kept: %s", got)
	}
}
