package codex

import (
	"strings"
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
	"ritam/internal/orchestrator"
)

// writeRulesForTest stands in for a write-enabled agent's rules.
var writeRulesForTest = core.WriteRules("/work/project", core.Grant{Level: domain.AccessBuildAndTest})

// TestTheExampleInEveryPromptIsAcceptedIfCopiedWordForWord: Codex agents
// were shown `TO=opus` in tasks whose authority was opus-1. Each prompt's
// example is now parsed and put to the orchestrator's own checks, for the
// authority and a supporting agent, in the opening brief and the per-turn
// one.
func TestTheExampleInEveryPromptIsAcceptedIfCopiedWordForWord(t *testing.T) {
	tk := &domain.Task{ID: "TASK-009", Title: "t", AuthorityAgent: "codex-1",
		Participants: []domain.AgentID{"codex-1", "opus-1"}}
	for _, self := range []domain.AgentID{"codex-1", "opus-1"} {
		m := domain.Message{FromAgent: domain.HumanAgentID, ToAgent: self, Type: domain.MsgInformation, Content: "go"}
		for name, p := range map[string]string{"full brief": renderPrompt(tk, m, ""), "turn brief": renderTurn(tk, m, "")} {
			_, ex, ok := strings.Cut(p, "Example for this task:\n")
			if !ok {
				t.Fatalf("%s for %s: no example", name, self)
			}
			var lines []string
			for _, l := range strings.Split(strings.TrimRight(ex, "\n"), "\n") {
				lines = append(lines, strings.TrimPrefix(l, "  "))
			}
			if !strings.HasPrefix(lines[0], "##CODEX TYPE=") {
				t.Errorf("%s for %s: the example is not in Codex's own header form: %q", name, self, lines[0])
			}
			msgs, err := parseOutput(strings.Join(lines, "\n"))
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
