package domain

import (
	"strings"
	"testing"
)

// noteKinds are the orchestrator's notes as they are written now (from
// OrchestratorID) and as history written before that recorded them (from the
// human, recognised by their metadata).
func noteKinds(id string, sec int, to AgentID) map[string]*Message {
	out := map[string]*Message{}
	for _, kind := range []string{"checkpoint", "reminder", "resumed", "authority", "unavailable",
		"unreadable", "decision-pending", "closed"} {
		m := msgAt(id, sec, HumanAgentID, to, MsgInformation, false)
		m.Metadata = map[string]string{MetaOrchestrator: kind}
		out["old "+kind] = m
	}
	for _, reason := range []string{"malformed-reply", "partial-reply"} {
		m := msgAt(id, sec, HumanAgentID, to, MsgError, false)
		m.Metadata = map[string]string{MetaReason: reason}
		out["old "+reason] = m
	}
	v := msgAt(id, sec, HumanAgentID, to, MsgError, false)
	v.Metadata = map[string]string{MetaViolation: "ROUTING_VIOLATION"}
	out["old violation notice"] = v
	for name, m := range out {
		cp := m.Clone()
		cp.FromAgent = OrchestratorID
		out["new"+strings.TrimPrefix(name, "old")] = cp
	}
	return out
}

// TestAQuestionToTheHumanStaysOpenAcrossEveryOrchestratorNote: opus asks the
// human something, and before the human replies the orchestrator writes to
// opus -- a checkpoint, a reminder, a resume note, a notice about a bad
// reply. None of them is the human answering, and none asks for anything.
func TestAQuestionToTheHumanStaysOpenAcrossEveryOrchestratorNote(t *testing.T) {
	for name, note := range noteKinds("N", 5, "opus") {
		l := BuildLedger([]*Message{msgAt("Q", 0, "opus", HumanAgentID, MsgQuestion, false), note})
		if !find(l, "Q").Open() {
			t.Errorf("%s: the orchestrator's note answered opus's question to the human", name)
		}
		if len(l) != 1 {
			t.Errorf("%s: an orchestrator note must not open a request, got %d requests", name, len(l))
		}
	}
}

// TestTheHumansReplyStillAnswersAfterANote: the same question, a checkpoint,
// then the human's real reply -- which answers it, as before. The human's
// REJECT of a decision carries metadata of its own; it is still the human.
func TestTheHumansReplyStillAnswersAfterANote(t *testing.T) {
	cp := msgAt("C", 3, OrchestratorID, "opus", MsgInformation, false)
	cp.Metadata = map[string]string{MetaOrchestrator: "checkpoint"}
	reply := msgAt("R", 9, HumanAgentID, "opus", MsgInformation, true)
	l := BuildLedger([]*Message{msgAt("Q", 0, "opus", HumanAgentID, MsgQuestion, false), cp, reply})
	if q := find(l, "Q"); q.Open() || q.Answer.ID != "R" {
		t.Errorf("the human's reply answers opus's question: open=%v", q.Open())
	}
	if !find(l, "R").Open() {
		t.Error("and the human's message is itself a request to opus, as before")
	}

	rej := msgAt("J", 9, HumanAgentID, "opus", MsgReject, true)
	rej.Metadata = map[string]string{"decisionId": "DEC-1"}
	if l := BuildLedger([]*Message{msgAt("Q", 0, "opus", HumanAgentID, MsgQuestion, false), rej}); find(l, "Q").Open() {
		t.Error("the human's rejection is the human answering")
	}
}

// TestFromOrchestratorKnowsOldAndNewNotes: the one rule every reader of the
// history uses to tell the orchestrator's notes from the human.
func TestFromOrchestratorKnowsOldAndNewNotes(t *testing.T) {
	for name, m := range noteKinds("N", 0, "opus") {
		if !FromOrchestrator(m) {
			t.Errorf("%s is the orchestrator's", name)
		}
	}
	for name, m := range map[string]*Message{
		"the human's instruction": {FromAgent: HumanAgentID, ToAgent: "opus"},
		"the human's follow-up":   {FromAgent: HumanAgentID, ToAgent: "opus", Metadata: map[string]string{"followUp": "true"}},
		"the human's rejection":   {FromAgent: HumanAgentID, ToAgent: "opus", Metadata: map[string]string{"decisionId": "DEC-1"}},
		"an agent's message":      {FromAgent: "opus", ToAgent: HumanAgentID, Metadata: map[string]string{MetaOrchestrator: "x"}},
		"no message":              nil,
	} {
		if FromOrchestrator(m) {
			t.Errorf("%s is not the orchestrator's", name)
		}
	}
}

// TestADelegateStaysOpenAfterAClarifyingQuestion: opus delegates to codex,
// codex asks opus which way to do it. The question asks about the work; it
// does not deliver it. It is a request of its own, from codex to opus, made
// in service of the delegation -- also when it names the delegation through
// InReplyTo.
func TestADelegateStaysOpenAfterAClarifyingQuestion(t *testing.T) {
	for _, linked := range []bool{false, true} {
		q := msgAt("Q", 5, "codex", "opus", MsgQuestion, false)
		if linked {
			q.InReplyTo = "D"
		}
		h := []*Message{msgAt("D", 0, "opus", "codex", MsgDelegate, false), q}
		l := BuildLedger(h)
		if !find(l, "D").Open() {
			t.Fatalf("linked=%v: codex's clarifying question closed the delegation", linked)
		}
		if r := find(l, "Q"); !r.Open() || r.Parent != "D" || r.Thread != "D" {
			t.Errorf("linked=%v: the question is a request in service of the delegation: open=%v parent=%s thread=%s",
				linked, r.Open(), r.Parent, r.Thread)
		}

		h = append(h, msgAt("A", 9, "opus", "codex", MsgAnswer, false))
		l = BuildLedger(h)
		if find(l, "Q").Open() || !find(l, "D").Open() {
			t.Errorf("linked=%v: opus's answer closes codex's question and leaves the work open", linked)
		}
		h = append(h, msgAt("R", 20, "codex", "opus", MsgImplementationResult, false))
		if n := len(OpenRequests(BuildLedger(h))); n != 0 {
			t.Errorf("linked=%v: codex's result finishes the delegation; %d left open", linked, n)
		}
	}
}

// TestAQuestionBackDoesNotCloseTheChainAboveIt: the human asks codex, codex
// asks opus, and opus asks the human to clarify. That is a question about the
// human's question, not the answer to it.
func TestAQuestionBackDoesNotCloseTheChainAboveIt(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("H", 0, HumanAgentID, "codex", MsgInformation, true),
		msgAt("Q", 5, "codex", "opus", MsgQuestion, false),
		msgAt("B", 9, "opus", HumanAgentID, MsgQuestion, false),
	})
	for _, id := range []MessageID{"H", "Q", "B"} {
		if !find(l, id).Open() {
			t.Errorf("%s was closed by a question back", id)
		}
	}
}

// TestAStatusUpdateDoesNotFinishDelegatedWork: "working on it" from codex is
// not the work opus asked for, whichever kind of work request it was. It still
// answers a question.
func TestAStatusUpdateDoesNotFinishDelegatedWork(t *testing.T) {
	for _, typ := range []MessageType{MsgDelegate, MsgImplement, MsgRequestChange} {
		l := BuildLedger([]*Message{
			msgAt("W", 0, "opus", "codex", typ, false),
			msgAt("S", 5, "codex", "opus", MsgStatusUpdate, false),
		})
		if !find(l, "W").Open() {
			t.Errorf("a status update closed opus's %s", typ)
		}
	}
	l := BuildLedger([]*Message{
		msgAt("Q", 0, "opus", "codex", MsgQuestion, false),
		msgAt("S", 5, "codex", "opus", MsgStatusUpdate, false),
	})
	if find(l, "Q").Open() {
		t.Error("a status update answers a question, as before")
	}
}

// TestAStatusUpdateToTheHumanStillClosesTheHumansInstruction: agents report
// "done: ..." to the human as a status update. Left open, the human's
// instruction would draw a reminder the human did not need.
func TestAStatusUpdateToTheHumanStillClosesTheHumansInstruction(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("H", 0, HumanAgentID, "codex", MsgInformation, true),
		msgAt("S", 5, "codex", HumanAgentID, MsgStatusUpdate, false),
	})
	if find(l, "H").Open() {
		t.Error("codex's status update to the human answers the human's instruction")
	}
}

// TestALedgerFromOldHistoryReadsLikeOneFromNew: a task begun before the
// orchestrator had its own id has its notes stored as the human's. Rebuilt,
// it must give the same requests, answers and abandonments as the same task
// recorded today.
func TestALedgerFromOldHistoryReadsLikeOneFromNew(t *testing.T) {
	build := func(noteFrom AgentID) []*Message {
		note := func(id string, sec int, to AgentID, typ MessageType, meta map[string]string) *Message {
			m := msgAt(id, sec, noteFrom, to, typ, false)
			m.Metadata = meta
			return m
		}
		return []*Message{
			msgAt("T", 0, HumanAgentID, "opus", MsgTaskAssignment, true),
			msgAt("QH", 1, "opus", HumanAgentID, MsgQuestion, false),
			msgAt("D", 2, "opus", "codex", MsgDelegate, false),
			note("CP", 3, "opus", MsgInformation, map[string]string{MetaOrchestrator: "checkpoint"}),
			note("MR", 4, "codex", MsgError, map[string]string{MetaReason: "malformed-reply"}),
			msgAt("QS", 5, "opus", "scout", MsgQuestion, false),
			note("RM", 6, "scout", MsgInformation, map[string]string{MetaOrchestrator: "reminder"}),
			note("CL", 7, "opus", MsgInformation, map[string]string{MetaOrchestrator: "closed-notify", MetaCloses: "QS"}),
			note("VN", 8, "codex", MsgError, map[string]string{MetaViolation: "ROUTING_VIOLATION"}),
			msgAt("H", 9, HumanAgentID, "opus", MsgInformation, true),
			msgAt("R", 10, "codex", "opus", MsgImplementationResult, false),
		}
	}
	summary := func(l []Request) map[MessageID]string {
		out := map[MessageID]string{}
		for _, r := range l {
			s := "open"
			if r.Answer != nil {
				s = "answered by " + string(r.Answer.ID)
			}
			if r.Abandoned {
				s += " (abandoned)"
			}
			out[r.Message.ID] = s + " thread " + string(r.Thread)
		}
		return out
	}
	old, now := summary(BuildLedger(build(HumanAgentID))), summary(BuildLedger(build(OrchestratorID)))
	if len(old) != len(now) {
		t.Fatalf("old history gives %d requests, new %d:\nold %v\nnew %v", len(old), len(now), old, now)
	}
	for id, s := range now {
		if old[id] != s {
			t.Errorf("%s: old history reads %q, new %q", id, old[id], s)
		}
	}
	// And what both say is right: only the human answered opus's question,
	// scout's question was abandoned, codex delivered the work.
	want := map[MessageID]string{
		"QH": "answered by H thread QH",
		"D":  "answered by R thread D",
		"QS": "answered by CL (abandoned) thread QS",
		"H":  "open thread H",
	}
	for id, s := range want {
		if now[id] != s {
			t.Errorf("%s = %q, want %q", id, now[id], s)
		}
	}
}
