package codex

import (
	"testing"

	"ritam/internal/domain"
)

func TestAReviewVerdictTravelsFromTheReplyHeader(t *testing.T) {
	msgs, err := parseOutput("##CODEX TYPE=REVIEW TO=human verdict=changes\nThe expiry check is missing.")
	if err != nil || len(msgs) != 1 {
		t.Fatalf("parse: %v %v", msgs, err)
	}
	if got := msgs[0].Metadata[domain.MetaVerdict]; got != "changes" {
		t.Errorf("verdict = %q, want changes", got)
	}
}
