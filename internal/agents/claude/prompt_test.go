package claude

import (
	"strings"
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
	"ritam/internal/orchestrator"
)

// writeRulesForTest stands in for a write-enabled agent's rules.
var writeRulesForTest = core.WriteRules("/work/project", core.Grant{Level: domain.AccessBuildAndTest})

// TestTheExampleInEveryPromptIsAcceptedIfCopiedWordForWord: the example used
// to be fixed (type=DELEGATE to=codex) and was shown to supporting agents,
// which may not delegate, in tasks with no agent called codex. Copied, it was
// a refused message and a paid retry. Each prompt's example is now parsed and
// put to the orchestrator's own checks, for the authority and a supporting
// agent, in the opening brief and the per-turn one.
func TestTheExampleInEveryPromptIsAcceptedIfCopiedWordForWord(t *testing.T) {
	tk := &domain.Task{ID: "TASK-009", Title: "t", AuthorityAgent: "opus-1",
		Participants: []domain.AgentID{"opus-1", "sonnet-1", "codex-1"}}
	for _, self := range []domain.AgentID{"opus-1", "sonnet-1"} {
		m := domain.Message{FromAgent: domain.HumanAgentID, ToAgent: self, Type: domain.MsgInformation, Content: "go"}
		for name, p := range map[string]string{"full brief": renderPrompt(tk, m, ""), "turn brief": renderTurn(tk, m, "")} {
			msgs, err := parseOutput(exampleOf(t, p))
			if err != nil || len(msgs) != 1 {
				t.Fatalf("%s for %s: example does not parse: %v %+v", name, self, err, msgs)
			}
			msgs[0].FromAgent = self
			if err := orchestrator.NewAuthorityManager().Authorize(tk, &msgs[0]); err != nil {
				t.Errorf("%s for %s: the example would be refused: %v", name, self, err)
			}
		}
	}
}

// exampleOf returns the example at the end of a prompt, as an agent copying
// it would write it.
func exampleOf(t *testing.T, p string) string {
	t.Helper()
	_, ex, ok := strings.Cut(p, "Example for this task:\n")
	if !ok {
		t.Fatalf("no example in:\n%s", p)
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(ex, "\n"), "\n") {
		lines = append(lines, strings.TrimPrefix(l, "  "))
	}
	return strings.Join(lines, "\n")
}

// TestEveryBriefOffersSilence: without being told it may stay silent, an
// agent sends "Understood, thanks", which costs its turn and the
// recipient's.
func TestEveryBriefOffersSilence(t *testing.T) {
	for name, p := range map[string]string{"full brief": renderPrompt(testTask(), msg("x"), ""),
		"turn brief": renderTurn(testTask(), msg("x"), writeRulesForTest)} {
		if !strings.Contains(p, "reply with exactly NO_REPLY") || !strings.Contains(p, "Do not send thanks") {
			t.Errorf("%s does not offer NO_REPLY:\n%s", name, p)
		}
	}
}
