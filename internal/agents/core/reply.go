package core

import (
	"fmt"
	"strings"

	"ritam/internal/domain"
	"ritam/internal/ports"
)

// ParseReply splits an agent's reply into protocol messages, one per block
// headed by marker ("@@ORCH" for Claude Code, "##CODEX" for Codex). who names
// the adapter in the error ("claude-code adapter"). A reply none of whose
// blocks can be delivered fails as domain.FailureMalformed.
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
// Header grammar: `<marker> type=<TYPE> to=<agent-id>` (see
// ParseHeaderFields). Everything up to the next header line (or end of input)
// is that message's content. A marker line is a header wherever it is --
// fenced, bulleted or quoted -- because models routinely decorate it, and
// refusing those would turn readable replies into paid retries.
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
			// A Malformed failure, carrying what the agent wrote so it can be
			// quoted back to it and to the human.
			f := domain.NewFailure(domain.FailureMalformed, fmt.Sprintf("%s: %s", who, strings.Join(problems, "; ")), nil)
			f.Raw = raw
			return nil, f
		}
		if msgs[0].Metadata == nil {
			msgs[0].Metadata = map[string]string{}
		}
		msgs[0].Metadata[ports.ProtocolProblemsMetadataKey] = strings.Join(problems, "; ")
	}
	return msgs, nil
}

// headerKeys are the only fields read from a header line.
var headerKeys = map[string]bool{"type": true, "to": true, "verdict": true, "item": true, "in_reply_to": true}

// ParseHeaderFields parses the fields following marker in a header line.
//
// Keys are matched case-insensitively (`type=` and `TYPE=` alike), and
// `key=value`, `key: value` and `key = value` are all read, since models
// write each. A value may be quoted to hold spaces. Only the known keys are
// kept, and reading stops at the first word that is not a field: models
// often run on into prose on the header line, and a `mode=fast` in that
// prose once replaced a real field.
func ParseHeaderFields(header, marker string) map[string]string {
	fields := map[string]string{}
	s := strings.TrimLeft(strings.TrimPrefix(header, marker), ":- \t")
	for {
		s = strings.TrimLeft(s, " \t")
		n := 0
		for n < len(s) && isKeyByte(s[n]) {
			n++
		}
		if n == 0 {
			return fields
		}
		key := strings.ToLower(strings.ReplaceAll(s[:n], "-", "_"))
		rest := strings.TrimLeft(s[n:], " \t")
		if rest == "" || (rest[0] != '=' && rest[0] != ':') {
			return fields // prose, not a field
		}
		rest = strings.TrimLeft(rest[1:], " \t")
		var value string
		if rest != "" && (rest[0] == '"' || rest[0] == '\'') {
			if end := strings.IndexByte(rest[1:], rest[0]); end >= 0 {
				value, s = rest[1:1+end], rest[2+end:]
			} else {
				value, s = rest[1:], ""
			}
		} else {
			end := strings.IndexAny(rest, " \t")
			if end < 0 {
				end = len(rest)
			}
			// A header written in bold or code (**...**, `...`) or ended with
			// punctuation leaves those characters on its last value -- but a
			// lone * is the broadcast address, not bold.
			value, s = strings.TrimRight(rest[:end], "`.,;:"), rest[end:]
			if value != string(domain.BroadcastAgentID) {
				value = strings.TrimRight(value, "`*.,;:")
			}
		}
		if _, seen := fields[key]; headerKeys[key] && !seen {
			fields[key] = strings.TrimSpace(value)
		}
	}
}

func isKeyByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
}

// NoReply is what an agent replies when nothing is asked of it and it has
// nothing new: the prompts offer it so that an agent need not invent a
// courtesy message ("Understood, thanks"), which costs its own turn and wakes
// the recipient for another.
const NoReply = "NO_REPLY"

// NothingToSay reports whether a reply carries no message: it is empty, or it
// is NO_REPLY (code formatting and surrounding space ignored). Both are taken
// the same way, as the agent choosing to say nothing.
func NothingToSay(reply string) bool {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(reply), "`"))
	return s == "" || s == NoReply
}

// NoHeaderFailure is the failure of a reply with text in it but no header
// at all: the agent answered in prose. It carries the text, so the agent and
// the human are shown what it wrote rather than only that it was unreadable.
func NoHeaderFailure(who string, agent domain.AgentID, marker, raw string) *domain.AgentFailure {
	article := "a"
	if strings.HasPrefix(marker, "@") {
		article = "an"
	}
	f := domain.NewFailure(domain.FailureMalformed,
		fmt.Sprintf("%s: agent %s replied without %s %s header", who, agent, article, marker), nil)
	f.Raw = raw
	return f
}
