package claude

import (
	"strings"
	"testing"

	"ritam/internal/agents/core"
	"ritam/internal/domain"
	"ritam/internal/ports"
)

func TestAReviewVerdictTravelsFromTheReplyHeader(t *testing.T) {
	msgs, err := parseOutput("@@ORCH type=REVIEW to=human verdict=approve\nChecked with codex; the fix holds.")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parse: %v %v", msgs, err)
	}
	if got := msgs[0].Metadata[domain.MetaVerdict]; got != "approve" {
		t.Errorf("verdict = %q, want approve", got)
	}
}

// TestSupportingPromptsOfferOnlyPermittedTypes: a supporting agent that is
// shown APPROVE in its menu will, sooner or later, send it.
func TestSupportingPromptsOfferOnlyPermittedTypes(t *testing.T) {
	tk := testTask() // authority opus, participants opus + codex
	m := msg("check this")
	m.ToAgent = "codex"

	for name, p := range map[string]string{
		"full brief": renderPrompt(tk, m, false),
		"turn brief": core.RenderTurn(tk, m, false, headerMarker),
	} {
		typeLine := ""
		for _, l := range strings.Split(p, "\n") {
			if strings.Contains(l, "PROPOSAL") && strings.Contains(l, "REVIEW") {
				typeLine = l
			}
		}
		if typeLine == "" {
			t.Fatalf("%s: no type list found", name)
		}
		for _, banned := range []string{"APPROVE", "FINAL_DECISION", "DELEGATE", "REQUEST_CHANGE", "IMPLEMENT,"} {
			if strings.Contains(typeLine, banned) {
				t.Errorf("%s offers a supporting agent %s: %s", name, banned, typeLine)
			}
		}
		if !strings.Contains(p, "verdict=approve") || !strings.Contains(p, "QUESTION") {
			t.Errorf("%s must explain verdicts and consulting another agent", name)
		}
	}
}

func TestTheAuthorityIsStillOfferedEverything(t *testing.T) {
	p := renderPrompt(testTask(), msg("start"), false) // to opus, the authority
	if !strings.Contains(p, "APPROVE") || !strings.Contains(p, "FINAL_DECISION") {
		t.Error("the authority must be offered its decision types")
	}
	if !strings.Contains(p, "verdict=approve") {
		t.Error("the authority should know supporting agents report verdicts")
	}
}

// TestAHelperIsToldTheQuestionIsTheHumans: an agent can only answer the human
// directly -- the preferred path -- if it knows the request it received is
// part of the human's question.
func TestAHelperIsToldTheQuestionIsTheHumans(t *testing.T) {
	tk := testTask()
	m := domain.Message{FromAgent: "codex", ToAgent: "opus", Type: domain.MsgQuestion,
		Content:  "How well did scout and I do?",
		Metadata: map[string]string{ports.ThreadMetadataKey: `the human asked codex: "how good were the reviews?"`}}

	for name, p := range map[string]string{
		"full brief": renderPrompt(tk, m, false),
		"turn brief": core.RenderTurn(tk, m, false, headerMarker),
	} {
		if !strings.Contains(p, "PART OF A QUESTION FROM THE HUMAN") || !strings.Contains(p, "the human asked codex") {
			t.Errorf("%s does not tell the agent the request is the human's", name)
		}
		if !strings.Contains(p, "answer the human directly (to=human)") || !strings.Contains(p, "tell codex") {
			t.Errorf("%s should prefer answering the human directly and telling the asker", name)
		}
	}

	plain := m
	plain.Metadata = nil
	if strings.Contains(core.RenderTurn(tk, plain, false, headerMarker), "PART OF A QUESTION FROM THE HUMAN") {
		t.Error("a request that is not the human's must not carry the note")
	}
}

func TestEveryRoleIsToldToAnswerTheHumanDirectly(t *testing.T) {
	sup := msg("x")
	sup.ToAgent = "codex"
	for name, p := range map[string]string{
		"authority":  renderPrompt(testTask(), msg("x"), false),
		"supporting": renderPrompt(testTask(), sup, false),
	} {
		if !strings.Contains(p, "answer the human directly") {
			t.Errorf("the %s brief should prefer answering the human directly", name)
		}
	}
}
