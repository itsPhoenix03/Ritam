package core

import (
	"strings"
	"testing"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// TestOneParserServesBothMarkers: the two adapters each had a copy of the
// header parser, differing only in the marker. The shared one must read
// either marker the same way, and report problems in the caller's name.
func TestOneParserServesBothMarkers(t *testing.T) {
	for _, marker := range []string{"@@ORCH", "##CODEX"} {
		raw := "preamble that is ignored\n" +
			marker + " TYPE=ANSWER TO=opus\nthe answer\n" +
			"`" + marker + " type=made_up to=human`\nmore\n" +
			marker + " type=REVIEW\nlost\n"
		msgs, err := ParseReply(raw, marker, "test adapter")
		if err != nil {
			t.Fatalf("%s: %v", marker, err)
		}
		if len(msgs) != 2 || msgs[0].Type != domain.MsgAnswer || msgs[0].ToAgent != "opus" ||
			msgs[0].Content != "the answer" || msgs[1].Type != domain.MsgInformation {
			t.Fatalf("%s: unexpected messages %+v", marker, msgs)
		}
		problems := msgs[0].Metadata[ports.ProtocolProblemsMetadataKey]
		if !strings.Contains(problems, `unknown message type "made_up"`) ||
			!strings.Contains(problems, "a REVIEW block had no recipient (to=) and was dropped") {
			t.Errorf("%s: problems not reported: %q", marker, problems)
		}
		_, err = ParseReply(marker+" type=ANSWER\nhello", marker, "test adapter")
		if err == nil || !strings.HasPrefix(err.Error(), "test adapter: ") {
			t.Errorf("%s: a reply whose only block has no recipient must fail in the adapter's name: %v", marker, err)
		}
		// It is Malformed, and carries what the agent wrote, so the re-prompt
		// and the pause quote the agent rather than the adapter.
		if f := domain.FailureOf(err); f.Kind != domain.FailureMalformed || !strings.Contains(f.Raw, "hello") {
			t.Errorf("%s: want a Malformed failure carrying the reply, got %+v", marker, f)
		}
	}
}

// TestHeaderFieldsAreReadTheWaysModelsWriteThem covers the header forms
// models produce: separators other than "=", quoted values, decoration, and
// prose run on after the fields -- which must never override a field.
func TestHeaderFieldsAreReadTheWaysModelsWriteThem(t *testing.T) {
	cases := []struct {
		name, reply string
		typ         domain.MessageType
		to          domain.AgentID
		verdict     string
	}{
		{"equals", "@@ORCH type=ANSWER to=opus-1\nbody", domain.MsgAnswer, "opus-1", ""},
		{"colon", "@@ORCH type: ANSWER to: opus-1\nbody", domain.MsgAnswer, "opus-1", ""},
		{"spaced equals", "@@ORCH type = ANSWER to = opus-1\nbody", domain.MsgAnswer, "opus-1", ""},
		{"capital keys", "##CODEX TYPE=ANSWER TO=opus-1\nbody", domain.MsgAnswer, "opus-1", ""},
		{"quoted value", `@@ORCH type=REVIEW to="opus-1" verdict="changes requested"` + "\nbody", domain.MsgReview, "opus-1", "changes requested"},
		{"prose after the fields", "@@ORCH type=ANSWER to=opus-1 here is what I found, set to=codex-1 later\nbody", domain.MsgAnswer, "opus-1", ""},
		{"prose with a field-like word", "@@ORCH type=ANSWER to=opus-1 note mode=fast verdict=reject\nbody", domain.MsgAnswer, "opus-1", ""},
		{"unknown key", "@@ORCH type=ANSWER priority=high to=opus-1\nbody", domain.MsgAnswer, "opus-1", ""},
		{"fenced header", "```\n@@ORCH type=ANSWER to=opus-1\n```\nbody", domain.MsgAnswer, "opus-1", ""},
		{"inline code header", "`@@ORCH type=ANSWER to=opus-1`\nbody", domain.MsgAnswer, "opus-1", ""},
		{"bold bulleted header", "- **@@ORCH type=ANSWER to=opus-1**\nbody", domain.MsgAnswer, "opus-1", ""},
		{"trailing full stop", "@@ORCH type=ANSWER to=opus-1.\nbody", domain.MsgAnswer, "opus-1", ""},
		{"colon after the marker", "@@ORCH: type=ANSWER to=opus-1\nbody", domain.MsgAnswer, "opus-1", ""},
		{"broadcast", "@@ORCH type=INFORMATION to=*\nbody", domain.MsgInformation, "*", ""},
		{"broadcast in code", "`@@ORCH type=INFORMATION to=*`.\nbody", domain.MsgInformation, "*", ""},
	}
	for _, c := range cases {
		marker := "@@ORCH"
		if strings.Contains(c.reply, "##CODEX") {
			marker = "##CODEX"
		}
		msgs, err := ParseReply(c.reply, marker, "test adapter")
		if err != nil || len(msgs) != 1 {
			t.Errorf("%s: %v %+v", c.name, err, msgs)
			continue
		}
		m := msgs[0]
		if m.Type != c.typ || m.ToAgent != c.to || m.Metadata[domain.MetaVerdict] != c.verdict {
			t.Errorf("%s: got type=%s to=%q verdict=%q", c.name, m.Type, m.ToAgent, m.Metadata[domain.MetaVerdict])
		}
		if p := m.Metadata[ports.ProtocolProblemsMetadataKey]; p != "" {
			t.Errorf("%s: problems reported: %s", c.name, p)
		}
	}
}

// TestNoReplyIsNothingToSay: an agent with nothing to add replies NO_REPLY,
// which is taken the way an empty reply is, never as a malformed one.
func TestNoReplyIsNothingToSay(t *testing.T) {
	for _, reply := range []string{"", "  \n", "NO_REPLY", " NO_REPLY\n", "`NO_REPLY`", "```\nNO_REPLY\n```"} {
		if !NothingToSay(reply) {
			t.Errorf("%q should be nothing to say", reply)
		}
	}
	for _, reply := range []string{"NO_REPLY, thanks!", "@@ORCH type=ANSWER to=opus\nNO_REPLY", "no reply needed"} {
		if NothingToSay(reply) {
			t.Errorf("%q carries something and must be read", reply)
		}
	}
}
