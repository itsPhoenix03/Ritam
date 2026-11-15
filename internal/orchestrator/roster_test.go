package orchestrator_test

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

// updateGolden regenerates testdata/prompts: go test ./internal/orchestrator
// -run Roster -update (or RITAM_UPDATE_GOLDEN=1), then read the diff.
var updateGolden = flag.Bool("update", false, "rewrite the golden prompt files in testdata/prompts")

// rosterGateway is a testGateway that knows each agent's provider, model and
// rights, as the real agent manager does.
type rosterGateway struct {
	*testGateway
	agents map[domain.AgentID]domain.Agent
	rights map[domain.AgentID]ports.AgentRights
}

func (g *rosterGateway) Statuses() []domain.Agent {
	out := g.testGateway.Statuses()
	for i, a := range out {
		info := g.agents[a.ID]
		out[i].Kind, out[i].Model = info.Kind, info.Model
	}
	return out
}

func (g *rosterGateway) AgentRights(_ *domain.Task, id domain.AgentID) (ports.AgentRights, bool) {
	r, ok := g.rights[id]
	return r, ok
}

type rosterRig struct {
	h                   *harness
	gw                  *rosterGateway
	task                *domain.Task
	opus, sonnet, codex *promptAgent
}

// newRosterRig is a mixed team: a write-enabled Claude authority, a
// read-only Claude agent and a Codex agent that builds.
func newRosterRig(t *testing.T) *rosterRig {
	t.Helper()
	r := &rosterRig{opus: newPromptAgent("opus-1"), sonnet: newPromptAgent("sonnet-1"), codex: newPromptAgent("codex-1")}
	r.gw = &rosterGateway{testGateway: newGateway(),
		agents: map[domain.AgentID]domain.Agent{
			"opus-1":   {Kind: domain.AgentKindClaudeCode, Model: "opus"},
			"sonnet-1": {Kind: domain.AgentKindClaudeCode, Model: "sonnet"},
			"codex-1":  {Kind: domain.AgentKindCodex, Model: "gpt-5-codex"},
		},
		rights: map[domain.AgentID]ports.AgentRights{
			"opus-1":   {Writes: true},
			"sonnet-1": {},
			"codex-1":  {Writes: true, Builds: true},
		}}
	r.gw.add(r.opus, domain.RoleAuthority)
	r.gw.add(r.sonnet, domain.RoleReviewer)
	r.gw.add(r.codex, domain.RoleImplementer)
	r.h = newHarness(t, defaultLimits(), time.Minute, r.gw.testGateway)
	r.h.orch.Agents = r.gw
	r.h.orch.Authority.Rights = r.gw
	r.task = createTask(t, r.h, "opus-1", "opus-1", "sonnet-1", "codex-1")
	for _, a := range []*promptAgent{r.opus, r.sonnet, r.codex} {
		_ = a.Start(context.Background())
	}
	r.h.orch.OpenInbox(r.task.ID)
	return r
}

// TestTheRosterGivesTheAuthoritysOpeningBriefEachAgentsRights renders the
// authority's whole opening prompt for a mixed team and keeps it as a golden
// file: who runs which model, who may write, who may build, and the advice
// on choosing among them.
func TestTheRosterGivesTheAuthoritysOpeningBriefEachAgentsRights(t *testing.T) {
	r := newRosterRig(t)
	if err := r.h.orch.Assign(context.Background(), r.task); err != nil {
		t.Fatal(err)
	}
	got := r.opus.deliveries()
	if len(got) != 1 {
		t.Fatalf("the authority should get its brief, got %d deliveries", len(got))
	}
	board := got[0].Metadata[ports.TeamMetadataKey]
	for _, want := range []string{
		"opus-1 (you)  claude opus        authority             write      no",
		"sonnet-1      claude sonnet      supporting, reviewer  read-only  no",
		"codex-1       codex gpt-5-codex  supporting            write      build/test",
	} {
		if !strings.Contains(board, want) {
			t.Errorf("the roster is missing %q:\n%s", want, board)
		}
	}
	if !strings.Contains(got[0].Content, "Only agents marked build/test can run commands") ||
		!strings.Contains(got[0].Content, "cheapest capable agent") {
		t.Errorf("the authority is not told how to use the roster:\n%s", got[0].Content)
	}

	// The prompt as Claude would get it, write-enabled, with codex-1 the
	// task's builder.
	acc := core.NewAccess(domain.AccessBuildAndTest, []string{"go test"})
	acc.SetSandboxed("codex-1", true)
	rules := core.WriteRules("/work/project", acc.GrantFor(r.task, "opus-1", nil))
	dialect := core.Dialect{Marker: "@@ORCH",
		ReadOnly: "You are READ-ONLY: you can read, grep and glob files, but you cannot edit anything or run shell commands."}
	prompt := core.RenderBrief(r.task, got[0], dialect, rules)
	prompt = strings.ReplaceAll(prompt, string(r.task.ID), "TASK-ID")
	prompt = regexp.MustCompile(`\b\d+[smh]\)`).ReplaceAllString(prompt, "0s)")
	checkGolden(t, "authority_opening_mixed_team", prompt)
}

// TestImplementForAReadOnlyAgentIsRefusedNamingWhoCanWrite: asked to
// implement, a read-only agent spent a paid turn saying it could not. The bus
// now refuses the request and tells the sender who can write.
func TestImplementForAReadOnlyAgentIsRefusedNamingWhoCanWrite(t *testing.T) {
	r := newRosterRig(t)
	ctx := context.Background()
	if _, err := r.h.orch.HandleOutboundForTest(ctx, r.task.ID, ports.Outbound{AgentID: "opus-1",
		Message: domain.Message{ToAgent: "sonnet-1", Type: domain.MsgImplement, Content: "Fix auth/logout.go."}}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.sonnet.deliveries()); n != 0 {
		t.Fatalf("a read-only agent must not be handed IMPLEMENT, got %d deliveries", n)
	}
	got := r.opus.deliveries()
	if len(got) != 1 || !domain.FromOrchestrator(&got[0]) {
		t.Fatalf("the authority should get a notice, got %+v", got)
	}
	if !strings.Contains(got[0].Content, "sonnet-1 is read-only") || !strings.Contains(got[0].Content, "codex-1") {
		t.Errorf("the notice must say the agent is read-only and name who can write:\n%s", got[0].Content)
	}

	// To an agent that can write, it goes through.
	r.opus.finish()
	if _, err := r.h.orch.HandleOutboundForTest(ctx, r.task.ID, ports.Outbound{AgentID: "opus-1",
		Message: domain.Message{ToAgent: "codex-1", Type: domain.MsgImplement, Content: "Fix auth/logout.go."}}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.codex.deliveries()); n != 1 {
		t.Errorf("IMPLEMENT to a writer must be delivered, got %d deliveries", n)
	}
}

// TestTheRosterShowsADroppedAgentAndWhatEachHasSpent: the roster is live.
// After an agent is dropped the next delivery says so, and the spending
// column shows each agent's cost, or n/a when its CLI reports none.
func TestTheRosterShowsADroppedAgentAndWhatEachHasSpent(t *testing.T) {
	r := newRosterRig(t)
	ctx := context.Background()
	if _, err := r.h.orch.Tasks.AddUsage(ctx, r.task.ID, "opus-1", ports.Usage{CostUSD: 0.42, InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.orch.Tasks.AddUsage(ctx, r.task.ID, "codex-1", ports.Usage{InputTokens: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.h.orch.Tasks.DropAgent(ctx, r.task.ID, "sonnet-1", "its turns kept failing"); err != nil {
		t.Fatal(err)
	}
	tk, _ := r.h.orch.Tasks.Get(ctx, r.task.ID)
	if err := r.h.orch.Bus.Route(ctx, tk, &domain.Message{FromAgent: domain.HumanAgentID, ToAgent: "codex-1",
		Type: domain.MsgInformation, Content: "Status?"}); err != nil {
		t.Fatal(err)
	}
	got := r.codex.deliveries()
	if len(got) != 1 {
		t.Fatalf("codex-1 should get the human's message, got %d", len(got))
	}
	board := got[0].Metadata[ports.TeamMetadataKey]
	for _, want := range []string{"state", "dropped", "spent", "$0.42", "n/a"} {
		if !strings.Contains(board, want) {
			t.Errorf("the roster is missing %q:\n%s", want, board)
		}
	}
	for _, l := range strings.Split(board, "\n") {
		if strings.HasPrefix(l, "sonnet-1") && !strings.Contains(l, "dropped") {
			t.Errorf("sonnet-1's line must show it was dropped: %q", l)
		}
	}
}

func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", "prompts", name+".golden")
	if *updateGolden || os.Getenv("RITAM_UPDATE_GOLDEN") != "" {
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
		t.Fatalf("%v (run go test ./internal/orchestrator -run Roster -update to create it)", err)
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
