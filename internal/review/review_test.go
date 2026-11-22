package review

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// payloadOf is a pre-tool payload like the ones Claude Code was seen sending
// (captured from claude 2.1.330 on 2026-11-22).
func payloadOf(tool, id string, input map[string]any) string {
	in, _ := json.Marshal(input)
	return `{"session_id":"b46ca52b-c1f2-4d81-99df-3a26d03f1028","transcript_path":"C:\\x.jsonl",` +
		`"cwd":"D:\\work\\app","prompt_id":"p","permission_mode":"dontAsk","effort":{"level":"medium"},` +
		`"hook_event_name":"PreToolUse","tool_name":"` + tool + `","tool_input":` + string(in) +
		`,"tool_use_id":"` + id + `"}`
}

// recorder starts a server that keeps what it is told.
type recorder struct {
	*Server
	mu      sync.Mutex
	calls   []Call
	reports []Report
}

func startRecorder(t *testing.T) *recorder {
	t.Helper()
	s, err := Start(`C:\tools\ritam.exe`, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &recorder{Server: s}
	s.OnCall = func(c Call) { r.mu.Lock(); r.calls = append(r.calls, c); r.mu.Unlock() }
	s.OnTurnEnd = func(rep Report) { r.mu.Lock(); r.reports = append(r.reports, rep); r.mu.Unlock() }
	return r
}

func (r *recorder) callList() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// runHook runs the hook as the CLI would, with env as its environment.
func runHook(env map[string]string, stdin string, args ...string) (int, string, string) {
	if len(args) == 0 {
		args = []string{preTool}
	}
	var out, errOut bytes.Buffer
	code := RunHook(args, strings.NewReader(stdin), &out, &errOut, func(k string) string { return env[k] })
	return code, out.String(), errOut.String()
}

func TestACallInATurnIsRecordedAgainstItAndPassed(t *testing.T) {
	r := startRecorder(t)
	turn := r.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	code, out, errOut := runHook(turn.Env(), payloadOf("Bash", "toolu_1", map[string]any{"command": "go test ./...", "description": "Run tests"}))
	if code != 0 || out != "" || errOut != "" {
		t.Fatalf("a passed call exits 0 and says nothing: code=%d stdout=%q stderr=%q", code, out, errOut)
	}
	calls := r.callList()
	if len(calls) != 1 {
		t.Fatalf("want one call recorded, got %d", len(calls))
	}
	c := calls[0]
	if c.Task != "TASK-001" || c.Agent != "opus" || c.Kind != domain.AgentKindClaudeCode || c.Tool != "Bash" ||
		c.ToolUseID != "toolu_1" || c.Summary != "go test ./..." || c.Cwd != `D:\work\app` || c.Decision != Pass {
		t.Errorf("recorded call: %+v", c)
	}
}

func TestACallFromNoRunningTurnIsBlocked(t *testing.T) {
	r := startRecorder(t)
	env := map[string]string{URLEnv(): r.URL(), TokenEnv(): "made-up"}
	code, _, errOut := runHook(env, payloadOf("Bash", "toolu_1", map[string]any{"command": "ls"}))
	if code != blockExit || !strings.Contains(errOut, "no agent turn") {
		t.Errorf("want blocked with the reason: code=%d stderr=%q", code, errOut)
	}
	if len(r.callList()) != 0 {
		t.Error("a call from no turn is not recorded against any")
	}
}

func TestAnEndedTurnsTokenNoLongerWorks(t *testing.T) {
	r := startRecorder(t)
	turn := r.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	env := turn.Env()
	turn.End(ports.ToolCalls{})
	if code, _, _ := runHook(env, payloadOf("Bash", "toolu_1", map[string]any{"command": "ls"})); code != blockExit {
		t.Errorf("a token outlived its turn: code=%d", code)
	}
}

func TestTheHookBlocksWhenItCannotCheck(t *testing.T) {
	r := startRecorder(t)
	turn := r.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	good := payloadOf("Read", "toolu_1", map[string]any{"file_path": "main.go"})

	stopped, err := Start(`C:\tools\ritam.exe`, nil)
	if err != nil {
		t.Fatal(err)
	}
	gone := stopped.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode}).Env()
	_ = stopped.Close()

	for _, tc := range []struct {
		name  string
		env   map[string]string
		stdin string
		args  []string
	}{
		{"no address or token", map[string]string{}, good, nil},
		{"ritam is gone", gone, good, nil},
		{"the call is not JSON", turn.Env(), "rm -rf /", nil},
		{"the call names no tool", turn.Env(), `{"session_id":"s","tool_input":{}}`, nil},
		{"the call is too large", turn.Env(), strings.Repeat("x", MaxPayload+1), nil},
		{"an event the hook does not have", turn.Env(), good, []string{"posttool"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := runHook(tc.env, tc.stdin, tc.args...)
			if code != blockExit || strings.TrimSpace(errOut) == "" || out != "" {
				t.Errorf("want exit 2 with a reason on stderr: code=%d stdout=%q stderr=%q", code, out, errOut)
			}
		})
	}
	// The unreadable calls reached the server, and are on record as refused.
	for _, c := range r.callList() {
		if c.Decision != Deny || c.Note == "" {
			t.Errorf("a call that could not be checked is recorded as refused, with why: %+v", c)
		}
	}
}

func TestTheServerRefusesRequestsThatDoNotNameIt(t *testing.T) {
	r := startRecorder(t)
	turn := r.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	req, _ := http.NewRequest(http.MethodPost, r.URL()+hookPath+preTool,
		strings.NewReader(payloadOf("Bash", "toolu_1", map[string]any{"command": "ls"})))
	req.Host = "evil.example:80"
	req.Header.Set("Authorization", "Bearer "+turn.Env()[TokenEnv()])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden || len(r.callList()) != 0 {
		t.Errorf("a request for another host must be refused: HTTP %d", resp.StatusCode)
	}
}

func TestEveryCallTheCLIReportedMustHaveReachedTheHook(t *testing.T) {
	r := startRecorder(t)
	turn := r.BeginTurn(ports.TurnScope{Task: "TASK-001", Agent: "opus", Kind: domain.AgentKindClaudeCode})
	runHook(turn.Env(), payloadOf("Read", "toolu_1", map[string]any{"file_path": "a.go"}))
	turn.End(ports.ToolCalls{Calls: []ports.ToolCall{{ID: "toolu_1", Tool: "Read"}, {ID: "toolu_2", Tool: "Bash"}}})
	turn.End(ports.ToolCalls{}) // a second End changes nothing

	if len(r.reports) != 1 {
		t.Fatalf("want one report, got %d", len(r.reports))
	}
	rep := r.reports[0]
	if rep.Seen != 1 || rep.Reported != 2 || len(rep.Missed) != 1 || rep.Missed[0].ID != "toolu_2" {
		t.Errorf("report: %+v", rep)
	}
	if p := rep.Problem(); !strings.Contains(p, "1 of the 2") || !strings.Contains(p, "Bash") {
		t.Errorf("problem: %q", p)
	}

	for _, tc := range []struct {
		name string
		rep  Report
		want string
	}{
		{"all seen", Report{Seen: 2, Reported: 2, ByID: true}, ""},
		{"nothing to compare", Report{}, ""},
		{"none seen, by id", Report{Seen: 0, Reported: 2, ByID: true, Missed: make([]ports.ToolCall, 2)}, "none of the 2"},
		{"none seen, by count", Report{Seen: 0, Reported: 3}, "none of the 3"},
		{"some seen, by count", Report{Seen: 1, Reported: 3}, ""},
	} {
		if got := tc.rep.Problem(); (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: problem %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestTheHookIsRitamItselfRunWithoutAShell(t *testing.T) {
	r := startRecorder(t)
	cmd := r.HookCommand(domain.AgentKindClaudeCode)
	if cmd.Program != `C:\tools\ritam.exe` || strings.Join(cmd.Args, " ") != "hook pretool" {
		t.Errorf("hook command: %+v", cmd)
	}
	if cmd.TimeoutSeconds <= int(hookTimeout.Seconds()) {
		t.Errorf("the CLI must wait longer (%ds) than the hook waits for ritam (%s)", cmd.TimeoutSeconds, hookTimeout)
	}
}

func TestASummarySaysWhatTheCallDoesWithoutSecrets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input string
		want  string
	}{
		{"shell command", `{"command":"go  test\n ./...","description":"Run the tests"}`, "go test ./..."},
		{"file tool", `{"file_path":"D:\\app\\main.go","content":"package main"}`, `D:\app\main.go`},
		{"search", `{"pattern":"TODO","path":"internal"}`, "TODO"},
		{"web", `{"url":"https://go.dev/doc","prompt":"summarise"}`, "https://go.dev/doc"},
		{"argv list", `{"command":["bash","-lc","make test"]}`, "bash -lc make test"},
		{"codex exec", `{"cmd":"cargo build"}`, "cargo build"},
		{"patch", `{"command":"*** Begin Patch\n*** Update File: a.go\n@@\n-x\n+y\n*** Add File: b.go\n+z\n*** End Patch"}`,
			"patch: update a.go, add b.go"},
		{"bearer token", `{"command":"curl -H 'Authorization: Bearer abcdefghijklmnopqrstuvwxyz' https://x"}`, "[REDACTED]"},
		{"api key", `{"command":"export ANTHROPIC_API_KEY=sk-ant-0123456789abcdefghij"}`, "[REDACTED]"},
		{"unknown tool", `{"todos":[{"content":"x"}]}`, `{"todos":[{"content":"x"}]}`},
	} {
		got := summarize(json.RawMessage(tc.input))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: summary %q, want it to contain %q", tc.name, got, tc.want)
		}
		if strings.Contains(got, "abcdefghijklmnopqrstuvwxyz") || strings.Contains(got, "0123456789abcdefghij") {
			t.Errorf("%s: a secret survived: %q", tc.name, got)
		}
	}
	long := summarize(json.RawMessage(`{"command":"` + strings.Repeat("a", 500) + `"}`))
	if n := len([]rune(long)); n != summaryLimit || !strings.HasSuffix(long, "...") {
		t.Errorf("a long command is cut to %d characters, got %d", summaryLimit, n)
	}
}
