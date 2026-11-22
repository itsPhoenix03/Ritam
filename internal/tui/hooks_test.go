package tui

import (
	"testing"

	"ritam/internal/domain"
)

// TestToolCallsStayInTheEventsTabAndAMissedHookIsShownEverywhere: the
// activity stream already shows each tool as it starts, so the record of a
// tool call is left to the events tab; a turn whose calls missed ritam's
// hook is something the human must see, in red, in the conversation too.
func TestToolCallsStayInTheEventsTabAndAMissedHookIsShownEverywhere(t *testing.T) {
	call := domain.Event{Type: domain.EvtToolCall, Summary: "opus Bash: go test ./..."}
	if streamShowsEvent(call) || convShowsEvent(call) {
		t.Error("a tool call belongs to the events tab only")
	}
	missed := domain.Event{Type: domain.EvtHookMissed, Summary: "opus: 1 of the 2 tool calls ..."}
	if !streamShowsEvent(missed) || !convShowsEvent(missed) || !isFailureEvent(missed.Type) {
		t.Error("a missed hook is shown in the stream and the conversation, as a failure")
	}
}
