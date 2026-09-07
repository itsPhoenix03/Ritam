package domain

import (
	"testing"
	"time"
)

var clock = time.Date(2026, 10, 9, 13, 0, 0, 0, time.UTC)

func msgAt(id string, sec int, from, to AgentID, typ MessageType, needsReply bool) *Message {
	return &Message{ID: MessageID(id), FromAgent: from, ToAgent: to, Type: typ,
		RequiresResponse: needsReply, Timestamp: clock.Add(time.Duration(sec) * time.Second)}
}

func find(l []Request, id MessageID) Request {
	for _, r := range l {
		if r.Message.ID == id {
			return r
		}
	}
	return Request{}
}

// TestAConsultationJoinsTheHumansThread is the scenario the ledger exists
// for: the human asks scout, scout asks codex to check part of it, codex
// answers scout, scout reports to the human.
func TestAConsultationJoinsTheHumansThread(t *testing.T) {
	h := []*Message{
		msgAt("H1", 0, HumanAgentID, "scout", MsgInformation, true),
		msgAt("Q1", 5, "scout", "codex", MsgQuestion, false),
	}
	l := BuildLedger(h)
	if len(l) != 2 {
		t.Fatalf("want 2 requests, got %d", len(l))
	}
	q := find(l, "Q1")
	if q.Thread != "H1" || q.Parent != "H1" {
		t.Errorf("scout's question to codex must join the human's thread: thread=%s parent=%s", q.Thread, q.Parent)
	}

	h = append(h, msgAt("A1", 20, "codex", "scout", MsgAnswer, false))
	l = BuildLedger(h)
	if find(l, "Q1").Open() {
		t.Error("codex's answer must close scout's question")
	}
	if !find(l, "H1").Open() {
		t.Error("the human's question is still open: scout has not reported back yet")
	}

	h = append(h, msgAt("R1", 30, "scout", HumanAgentID, MsgReview, false))
	l = BuildLedger(h)
	if len(OpenRequests(l)) != 0 {
		t.Errorf("after scout reports to the human nothing is open, got %d", len(OpenRequests(l)))
	}
}

func TestOneReplyAnswersSeveralQuestions(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("Q1", 0, "opus", "codex", MsgQuestion, false),
		msgAt("Q2", 1, "opus", "codex", MsgDelegate, false),
		msgAt("A", 9, "codex", "opus", MsgAnswer, false),
	})
	if len(OpenRequests(l)) != 0 {
		t.Error("agents answer several questions in one reply; both must close")
	}
}

func TestAReplyToSomeoneElseDoesNotCloseTheRequest(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("H1", 0, HumanAgentID, "scout", MsgInformation, true),
		msgAt("X", 5, "scout", "opus", MsgInformation, false),
	})
	if !find(l, "H1").Open() {
		t.Error("scout telling opus something is not an answer to the human")
	}
}

func TestInReplyToClosesOnlyTheNamedRequest(t *testing.T) {
	a := msgAt("A", 9, "codex", "opus", MsgAnswer, false)
	a.InReplyTo = "Q1"
	l := BuildLedger([]*Message{
		msgAt("Q1", 0, "opus", "codex", MsgQuestion, false),
		msgAt("Q2", 1, "opus", "codex", MsgQuestion, false),
		a,
	})
	if find(l, "Q1").Open() || !find(l, "Q2").Open() {
		t.Error("an explicit InReplyTo must close exactly that request")
	}
}

func TestNotEveryMessageIsARequest(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("T", 0, HumanAgentID, "opus", MsgTaskAssignment, true), // answered by working
		msgAt("E", 1, HumanAgentID, "scout", MsgError, false),        // orchestrator notice
		msgAt("B", 2, "opus", BroadcastAgentID, MsgDelegate, false),  // nobody in particular
		msgAt("I", 3, "scout", "opus", MsgInformation, false),        // a statement
		msgAt("F", 4, "opus", BroadcastAgentID, MsgFinalDecision, false),
	})
	if len(l) != 0 {
		var ids []MessageID
		for _, r := range l {
			ids = append(ids, r.Message.ID)
		}
		t.Errorf("none of these ask a specific party for a reply, got requests %v", ids)
	}
}

func TestABroadcastAnswersTheAsker(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("Q", 0, "opus", "codex", MsgQuestion, false),
		msgAt("A", 3, "codex", BroadcastAgentID, MsgAnswer, false),
	})
	if find(l, "Q").Open() {
		t.Error("a broadcast reaches the asker too, so it answers the request")
	}
}

// The rows below are the ways a question the human asks A can unfold. The
// ledger must read every one of them correctly, because a follow-up completes
// only when nothing is left open.

// Row 3, seen in a real run: the human asked codex to ask opus; codex did;
// opus answered the human directly and only told codex afterwards.
func TestAHelperAnsweringTheHumanDirectlyClosesTheChain(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("H", 0, HumanAgentID, "codex", MsgInformation, true),
		msgAt("Q", 5, "codex", "opus", MsgQuestion, false),
		msgAt("A", 9, "opus", HumanAgentID, MsgAnswer, false),
	})
	if find(l, "H").Open() {
		t.Error("opus answered the human on codex's behalf; the human's question is answered")
	}
	if find(l, "Q").Open() {
		t.Error("codex's question to opus was answered upward, to the human; it must close too")
	}
}

// Row 4: the answer comes from further down the chain.
func TestAnAnswerFromDeeperInTheChainClosesEveryStep(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("H", 0, HumanAgentID, "scout", MsgInformation, true),
		msgAt("Q1", 2, "scout", "opus", MsgQuestion, false),
		msgAt("Q2", 4, "opus", "codex", MsgDelegate, false),
		msgAt("A", 9, "codex", HumanAgentID, MsgAnswer, false),
	})
	if n := len(OpenRequests(l)); n != 0 {
		t.Errorf("codex answered the human at the end of a three-step chain; %d request(s) left open", n)
	}
}

// Row 5: two branches; an answer down one leaves the other waiting.
func TestOtherBranchesStayOpenUntilAnswered(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("H", 0, HumanAgentID, "opus", MsgInformation, true),
		msgAt("QB", 2, "opus", "scout", MsgQuestion, false),
		msgAt("QC", 3, "opus", "codex", MsgQuestion, false),
		msgAt("A", 9, "scout", HumanAgentID, MsgAnswer, false),
	})
	if find(l, "H").Open() || find(l, "QB").Open() {
		t.Error("scout's answer to the human closes the human's question and scout's branch")
	}
	if !find(l, "QC").Open() {
		t.Error("codex has not answered; its branch must stay open so the task does not finish under it")
	}
}

// An agent-level version of row 3: A asks B, B asks C, C answers A directly.
func TestSkippingAMiddleAgentWorksBetweenAgentsToo(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("Q1", 0, "opus", "scout", MsgDelegate, false),
		msgAt("Q2", 3, "scout", "codex", MsgQuestion, false),
		msgAt("A", 9, "codex", "opus", MsgAnswer, false),
	})
	if len(OpenRequests(l)) != 0 {
		t.Error("codex answered opus, who started the chain; both steps are answered")
	}
}

// An answer to the human from someone outside the thread answers nothing.
func TestAnUnrelatedAgentDoesNotCloseTheHumansQuestion(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("H", 0, HumanAgentID, "codex", MsgInformation, true),
		msgAt("S", 5, "scout", HumanAgentID, MsgStatusUpdate, false),
	})
	if !find(l, "H").Open() {
		t.Error("scout was never asked; its status update is not an answer to the human's question to codex")
	}
}

// Row 6: an agent asks the human, and the human's reply answers it.
func TestTheHumansReplyAnswersAnAgentsQuestion(t *testing.T) {
	l := BuildLedger([]*Message{
		msgAt("Q", 0, "opus", HumanAgentID, MsgQuestion, false),
		msgAt("R", 9, HumanAgentID, "opus", MsgInformation, true),
	})
	if find(l, "Q").Open() {
		t.Error("the human answered opus's question")
	}
	if !find(l, "R").Open() {
		t.Error("and the human's message is itself a new question to opus")
	}
}

// TestRespondingToSomeoneElseIsNotAnAnswer: passing the human's question on is
// "in reply to" it, but it is not an answer to the human.
func TestRespondingToSomeoneElseIsNotAnAnswer(t *testing.T) {
	q := msgAt("Q", 5, "codex", "opus", MsgQuestion, false)
	q.InReplyTo = "H"
	l := BuildLedger([]*Message{msgAt("H", 0, HumanAgentID, "codex", MsgInformation, true), q})
	if !find(l, "H").Open() {
		t.Error("codex asking opus, in reply to the human, has not answered the human")
	}
	if find(l, "Q").Thread != "H" {
		t.Error("and codex's question belongs to the human's thread")
	}
}

// TestInReplyToAnOrdinaryMessageStillAnswers: replying "in reply to" an answer
// or a reminder -- not a request -- must still count as answering the asker.
func TestInReplyToAnOrdinaryMessageStillAnswers(t *testing.T) {
	note := msgAt("N", 3, HumanAgentID, "codex", MsgInformation, false) // a reminder, not a request
	reply := msgAt("A", 6, "codex", HumanAgentID, MsgAnswer, false)
	reply.InReplyTo = "N"
	l := BuildLedger([]*Message{msgAt("H", 0, HumanAgentID, "codex", MsgInformation, true), note, reply})
	if find(l, "H").Open() {
		t.Error("codex answered the human; pointing InReplyTo at the reminder must not hide that")
	}
}

// A request the orchestrator closed as unanswered, after a reminder, is closed
// by a note in the history -- so the TUI and the run loop, which both read the
// ledger from the messages, agree it is no longer outstanding.
func TestAClosingNoteClosesARequestAsAbandoned(t *testing.T) {
	note := msgAt("N", 60, HumanAgentID, "opus", MsgInformation, false)
	note.Metadata = map[string]string{MetaCloses: "QC"}
	l := BuildLedger([]*Message{
		msgAt("QC", 0, "opus", "codex", MsgQuestion, false),
		msgAt("QH", 1, "opus", HumanAgentID, MsgQuestion, false),
		note,
	})
	qc := find(l, "QC")
	if qc.Open() || !qc.Abandoned {
		t.Errorf("the note must close codex's request as abandoned: open=%v abandoned=%v", qc.Open(), qc.Abandoned)
	}
	if !find(l, "QH").Open() {
		t.Error("the note is addressed to opus but is not the human answering opus's own question")
	}
}

// A message to everyone answers everyone who asked its sender, even when it
// is linked (InReplyTo) to one particular request.
func TestABroadcastAnswersEveryAskerEvenWhenLinked(t *testing.T) {
	d := msgAt("D", 9, "opus", BroadcastAgentID, MsgFinalDecision, false)
	d.InReplyTo = "R"
	l := BuildLedger([]*Message{
		msgAt("H", 0, HumanAgentID, "opus", MsgInformation, true),
		msgAt("R", 5, "scout", "opus", MsgImplementationResult, true),
		d,
	})
	if len(OpenRequests(l)) != 0 {
		t.Error("opus's decision went to everyone; both the human's question and scout's result are answered")
	}
}
