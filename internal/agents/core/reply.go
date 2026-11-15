package core

import (
	"fmt"
	"strings"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// ParseReply splits an agent's reply into protocol messages, one per block
// headed by marker ("@@ORCH" for Claude Code, "##CODEX" for Codex). who names
// the adapter in the error ("claude-code adapter").
//
// Both adapters used to carry their own copy of this parser, identical but
// for the marker; a fix made to one had to be remembered in the other. It
// lives here once, with the marker as a parameter.
//
// Agent output is untrusted: unknown message types are rejected rather than
// passed through, and text that is not part of a recognised block is ignored
// as conversational filler. A model that prefaces its answer with prose, or
// fences the header, must not be able to smuggle an unroutable message
// through.
//
// Header grammar: `<marker> type=<TYPE> to=<agent-id>`, keys matched without
// regard to case. Everything up to the next header line (or end of input) is
// that message's content.
func ParseReply(raw, marker, who string) ([]domain.Message, error) {
	lines := strings.Split(raw, "\n")
	var (
		msgs     []domain.Message
		cur      *domain.Message
		content  strings.Builder
		problems []string
		skipping bool
	)

	flush := func() {
		if cur == nil {
			return
		}
		cur.Content = strings.TrimSpace(content.String())
		msgs = append(msgs, *cur)
		cur = nil
		content.Reset()
	}

	for _, line := range lines {
		// Tolerate a fenced or bulleted header: models routinely decorate.
		trimmed := strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "`>-* "))
		if strings.HasPrefix(trimmed, marker) {
			flush()
			fields := ParseHeaderFields(trimmed, marker)

			// One bad block does not cost the whole reply: an unknown type is
			// delivered as INFORMATION (never as anything stronger), a block
			// with no recipient is dropped, and the agent is told about both.
			// Discarding everything once lost a whole broadcast from the
			// authority over a made-up "DECISION" type.
			raw := fields["type"]
			typ := domain.MessageType(strings.ToUpper(raw))
			if !domain.IsKnownMessageType(typ) {
				problems = append(problems, fmt.Sprintf("unknown message type %q was delivered as INFORMATION", raw))
				typ = domain.MsgInformation
			}
			to := domain.AgentID(fields["to"])
			if to == "" {
				problems = append(problems, fmt.Sprintf("a %s block had no recipient (to=) and was dropped", typ))
				skipping = true
				continue
			}
			skipping = false
			cur = &domain.Message{Type: typ, ToAgent: to}
			// A REVIEW may carry a verdict (approve, changes, reject). It is passed
			// on as written; the orchestrator validates it.
			if v := fields[domain.MetaVerdict]; v != "" {
				cur.Metadata = map[string]string{domain.MetaVerdict: v}
			}
			continue
		}
		if cur != nil && !skipping {
			content.WriteString(line)
			content.WriteString("\n")
		}
	}
	flush()
	if len(problems) > 0 {
		if len(msgs) == 0 {
			return nil, fmt.Errorf("%s: %s", who, strings.Join(problems, "; "))
		}
		if msgs[0].Metadata == nil {
			msgs[0].Metadata = map[string]string{}
		}
		msgs[0].Metadata[ports.ProtocolProblemsMetadataKey] = strings.Join(problems, "; ")
	}
	return msgs, nil
}

// ParseHeaderFields parses the `key=value` pairs following marker in a header
// line. Keys are matched case-insensitively, so a model writing `type=`
// instead of `TYPE=` is still understood; values may be quoted to allow
// spaces.
func ParseHeaderFields(header, marker string) map[string]string {
	fields := map[string]string{}
	rest := strings.TrimSpace(strings.TrimPrefix(header, marker))
	for _, tok := range strings.Fields(rest) {
		k, v, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		v = strings.TrimSuffix(v, ".")
		fields[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return fields
}
