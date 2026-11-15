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
