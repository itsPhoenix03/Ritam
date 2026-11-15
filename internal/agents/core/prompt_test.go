package core

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// The rendered prompts are product behaviour, so each situation is kept as a
// golden file under testdata/prompts: a change to the wording shows up as a
// readable diff. To accept a change, regenerate them with
//
//	go test ./internal/agents/core -run Golden -update
//
// (or RITAM_UPDATE_GOLDEN=1 go test ./internal/agents/core) and read the diff.
var update = flag.Bool("update", false, "rewrite the golden prompt files in testdata/prompts")

// The adapters' dialects, as their protocol.go files declare them.
var (
	testDialect  = Dialect{Marker: "@@ORCH", ReadOnly: "You are READ-ONLY: you can read, grep and glob files, but you cannot edit anything or run shell commands."}
	codexDialect = Dialect{Marker: "##CODEX", UpperKeys: true, ReadOnly: "You are READ-ONLY, in the read-only sandbox: you can inspect files, but you cannot edit anything or make network calls."}
)

func goldenTask() *domain.Task {
	return &domain.Task{
		ID:    "TASK-007",
		Title: "Fix the logout cookie bug",
		Description: "Logged-out users keep an authenticated session cookie.\n" +
			"Reproduce it, find the cause, fix it and add a regression test.\n" +
			"The fix must not change the login flow.\n" +
			"Keep the cookie name; other services read it.",
		AuthorityAgent: "opus-1",
		Participants:   []domain.AgentID{"opus-1", "sonnet-1", "codex-1"},
	}
}

// goldenBoard is a team board as the orchestrator attaches it: the roster,
// then who waits on whom.
const goldenBoard = "" +
	"id        model              role        files      commands    state    spent\n" +
	"opus-1    claude opus        authority   write      no          working  $0.42\n" +
	"sonnet-1  claude sonnet      supporting  read-only  no          idle     $0.08\n" +
	"codex-1   codex gpt-5-codex  supporting  write      build/test  idle     n/a\n" +
	"- opus-1: waiting on codex-1 (IMPLEMENT, 2m)\n" +
	"- codex-1: owes opus-1 a reply (IMPLEMENT, 2m)"

func goldenRules(self domain.AgentID) string {
	g := Grant{Level: domain.AccessBuildAndTest}
	if self == "codex-1" {
		g.Commands, g.BuildDir = []string{"go build", "go test"}, "build/codex-1"
	} else {
		g.Builders = []domain.AgentID{"codex-1"}
	}
	return WriteRules("/work/project", g)
}

func goldenPrompts() map[string]string {
	t := goldenTask()
	assignment := domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "opus-1", Type: domain.MsgTaskAssignment,
		Content:  "Task TASK-007: Fix the logout cookie bug\n\nThe other participants (sonnet-1, codex-1) have not been briefed.",
		Metadata: map[string]string{ports.TeamMetadataKey: goldenBoard}}
	delegate := domain.Message{FromAgent: "opus-1", ToAgent: "sonnet-1", Type: domain.MsgDelegate,
		Content:  "Find where the session cookie is cleared on logout. Report file and line; do not change files.",
		Metadata: map[string]string{ports.TeamMetadataKey: goldenBoard}}
	resumed := domain.Message{FromAgent: "opus-1", ToAgent: "codex-1", Type: domain.MsgImplement,
		Content:  "Clear the cookie with Path=/ in auth/logout.go and add a regression test.",
		Metadata: map[string]string{ports.TeamMetadataKey: goldenBoard, RecapKey: "(ignored on a resumed turn)"}}
	combined := domain.Message{FromAgent: domain.BroadcastAgentID, ToAgent: "opus-1", Type: domain.MsgInformation,
		Content: "=== 1 of 2 · ANSWER from sonnet-1 · 10:04:05 · MSG-12 ===\n" +
			"The cookie is set with Path=/ but cleared with Path=/api (auth/logout.go:41).\n\n" +
			"=== 2 of 2 · NOTICE from orchestrator · 10:04:09 · MSG-13 ===\n" +
			"Your TEST_RESULT was rejected by the orchestrator (ROUTING_VIOLATION): recipient \"codex\" is not a participant of this task.",
		Metadata: map[string]string{ports.BatchMetadataKey: "2", ports.TeamMetadataKey: goldenBoard}}
	rebuilt := domain.Message{FromAgent: "opus-1", ToAgent: "sonnet-1", Type: domain.MsgQuestion,
		Content: "Does the fix in auth/logout.go also cover the admin logout?",
		Metadata: map[string]string{ports.TeamMetadataKey: goldenBoard,
			RecapKey: "[opus-1 -> sonnet-1 DELEGATE] Find where the session cookie is cleared on logout.\n" +
				"[sonnet-1 -> opus-1 ANSWER] Cleared with Path=/api in auth/logout.go:41; it is set with Path=/."}}

	return map[string]string{
		"authority_opening":        RenderBrief(t, assignment, testDialect, goldenRules("opus-1")),
		"supporting_first_contact": RenderBrief(t, delegate, testDialect, ""),
		"resumed_turn":             RenderTurn(t, resumed, codexDialect, goldenRules("codex-1")),
		"combined_turn_notice":     RenderTurn(t, combined, testDialect, goldenRules("opus-1")),
		"rebuilt_session":          RenderBrief(t, rebuilt, testDialect, ""),
	}
}

// TestGoldenPromptsMatchTheirFiles: the five situations an agent's prompt is
// rendered in -- the authority's opening brief, a supporting agent's first
// contact, a resumed turn, a combined turn carrying a notice, and a session
// rebuilt after it was lost -- each match their stored text.
func TestGoldenPromptsMatchTheirFiles(t *testing.T) {
	for name, got := range goldenPrompts() {
		t.Run(name, func(t *testing.T) { checkGolden(t, name, got) })
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "prompts", name+".golden")
	if *update || os.Getenv("RITAM_UPDATE_GOLDEN") != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test ./internal/agents/core -run Golden -update to create it)", err)
	}
	want := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got == want {
		return
	}
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("%s differs from %s at line %d:\n got: %q\nwant: %q\n(if the change is meant, rerun with -update and review the diff)",
				name, path, i+1, g, w)
		}
	}
}

// TestEveryPromptEndsWithTheReplyFormatAndATaskExample: the reply format is
// the last thing read, after the write rules, and its example names a real
// participant.
func TestEveryPromptEndsWithTheReplyFormatAndATaskExample(t *testing.T) {
	for name, p := range goldenPrompts() {
		format := strings.LastIndex(p, "REPLY FORMAT (mandatory):")
		if format < 0 {
			t.Fatalf("%s: no reply format", name)
		}
		if rules := strings.LastIndex(p, "WRITE RULES"); rules > format {
			t.Errorf("%s: the write rules come after the reply format", name)
		}
		if i := strings.LastIndex(p, "Example for this task:"); i < format {
			t.Errorf("%s: the example is not at the end of the reply format", name)
		}
		if strings.Contains(p[format:], "to=codex\n") || strings.Contains(p[format:], "TO=opus\n") {
			t.Errorf("%s: a hard-coded example id is back", name)
		}
	}
}

// TestAResumedTurnCarriesTheTaskAndTheAuthoritysRules: after the CLI compacts
// a session, the standing brief may be all the authority has left of the task
// and of its own rules.
func TestAResumedTurnCarriesTheTaskAndTheAuthoritysRules(t *testing.T) {
	task := goldenTask()
	m := domain.Message{FromAgent: "sonnet-1", ToAgent: "opus-1", Type: domain.MsgAnswer, Content: "done"}
	p := RenderTurn(task, m, testDialect, "")
	for _, want := range []string{
		"Logged-out users keep an authenticated session cookie.",
		"make sure the requests you sent have been answered",
		"[...]", // the description is capped
	} {
		if !strings.Contains(p, want) {
			t.Errorf("resumed turn is missing %q:\n%s", want, p)
		}
	}
	if strings.Contains(p, "Keep the cookie name") {
		t.Error("a resumed turn repeats only the first lines of the description")
	}
}

// TestTheTypeTableCoversExactlyWhatAgentsAreOffered: every type an agent may
// be offered has a meaning, and nothing an agent should never send is listed.
func TestTheTypeTableCoversExactlyWhatAgentsAreOffered(t *testing.T) {
	seen := map[domain.MessageType]int{}
	for _, g := range typeGroups {
		for _, tm := range g.types {
			seen[tm.typ]++
		}
	}
	for _, typ := range domain.AllowedTypes(true) {
		if seen[typ] != 1 {
			t.Errorf("%s appears %d times in the type table", typ, seen[typ])
		}
	}
	if len(seen) != len(domain.AllowedTypes(true)) {
		t.Errorf("the type table lists %d types; agents are offered %d", len(seen), len(domain.AllowedTypes(true)))
	}

	var sup, auth strings.Builder
	WriteTypeGuide(&sup, false)
	WriteTypeGuide(&auth, true)
	for _, banned := range []string{"ERROR", "TASK_ASSIGNMENT", "DELEGATE", "IMPLEMENT (", "APPROVE", "FINAL_DECISION", "deciding:"} {
		if strings.Contains(sup.String(), banned) {
			t.Errorf("a supporting agent is offered %s:\n%s", banned, sup.String())
		}
	}
	if strings.Contains(auth.String(), "ERROR") || !strings.Contains(auth.String(), "FINAL_DECISION (the outcome") {
		t.Errorf("the authority's types:\n%s", auth.String())
	}
}
