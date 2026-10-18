package mockagent

import (
	"errors"

	"ritam/internal/domain"
)

// counterpart returns the first task participant that is not self, or ""
// when none exists. It lets a single Behavior func address "the other agent"
// without hard-coding ids like "opus"/"codex".
func counterpart(self domain.AgentID, t *domain.Task) domain.AgentID {
	if t == nil {
		return ""
	}
	for _, p := range t.Participants {
		if p != self {
			return p
		}
	}
	return ""
}

func reply(typ domain.MessageType, to domain.AgentID, content string, requiresResponse bool) domain.Message {
	return domain.Message{
		ToAgent:          to,
		Type:             typ,
		Content:          content,
		RequiresResponse: requiresResponse,
	}
}

// AuthorityBehavior models OPUS: it delegates investigation, then
// implementation, requests one round of changes, then issues a
// FINAL_DECISION. It also demonstrates the challenge -> re-investigate loop.
func AuthorityBehavior() Behavior {
	implResultCount := 0
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		other := counterpart(self, t)
		switch in.Type {
		case domain.MsgTaskAssignment:
			if other == "" {
				// Nobody to delegate to. Staying silent is better than
				// emitting a message with no recipient, which the bus would
				// correctly reject as a ROUTING_VIOLATION while the run loop
				// waited out its idle timeout for nothing.
				return nil
			}
			return []domain.Message{reply(domain.MsgDelegate, other,
				"Investigate cookie creation and logout behavior. Do not modify code yet.", true)}

		case domain.MsgAnswer:
			return []domain.Message{reply(domain.MsgImplement, in.FromAgent,
				"Implement centralized cookie options. Add regression tests.", true)}

		case domain.MsgImplementationResult:
			implResultCount++
			if implResultCount == 1 {
				return []domain.Message{reply(domain.MsgRequestChange, in.FromAgent,
					"Add a domain-scoped cookie regression test.", true)}
			}
			return []domain.Message{{
				ToAgent: domain.BroadcastAgentID,
				Type:    domain.MsgFinalDecision,
				Content: "Implementation accepted.",
				Metadata: map[string]string{
					"decision":  "Centralize authentication cookie options.",
					"reasoning": "Cookie creation uses Path=/ while logout cleared Path=/api.",
					"evidence":  "Codex investigation; 26 passing tests",
				},
			}}

		case domain.MsgChallenge:
			return []domain.Message{reply(domain.MsgDelegate, in.FromAgent,
				"Provide evidence for the alternative root cause.", true)}

		default:
			return nil
		}
	}
}

// ImplementerBehavior models CODEX: it answers investigation requests,
// implements on request, and reports results after each change request.
func ImplementerBehavior() Behavior {
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		switch in.Type {
		case domain.MsgTaskAssignment:
			return nil // waits to be delegated to

		case domain.MsgDelegate:
			return []domain.Message{reply(domain.MsgAnswer, in.FromAgent,
				"The cookie is created with Path=/, but logout clears it using Path=/api. "+
					"The browser therefore retains the original cookie.", false)}

		case domain.MsgImplement:
			return []domain.Message{reply(domain.MsgImplementationResult, in.FromAgent,
				"Implementation complete. 25 tests pass.", true)}

		case domain.MsgRequestChange:
			return []domain.Message{reply(domain.MsgImplementationResult, in.FromAgent,
				"Added domain-scoped cookie regression test. 26 tests pass.", true)}

		default:
			return nil
		}
	}
}

// ChallengingImplementerBehavior behaves like ImplementerBehavior except that
// it disputes a DELEGATE with a CHALLENGE instead of investigating, exercising
// the disagreement flow.
func ChallengingImplementerBehavior() Behavior {
	base := ImplementerBehavior()
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		if in.Type == domain.MsgDelegate {
			return []domain.Message{reply(domain.MsgChallenge, in.FromAgent,
				"I believe that is not the root cause. Evidence: the cookie is cleared "+
					"correctly in the integration test.", true)}
		}
		return base(self, t, in)
	}
}

// RogueBehavior models a supporting agent that violates authority rules: it
// issues a FINAL_DECISION it has no right to make, and delegates to an agent
// outside the task ("stranger"). It exists to test that the orchestrator
// rejects both authority violations.
func RogueBehavior() Behavior {
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		// It oversteps on either the opening brief or a delegation. A
		// supporting agent only ever receives the latter, since the authority
		// alone is briefed — so triggering on DELEGATE is what exercises the
		// realistic case of a delegated agent exceeding its role.
		if in.Type != domain.MsgTaskAssignment && in.Type != domain.MsgDelegate {
			return nil
		}
		return []domain.Message{
			{
				ToAgent: domain.BroadcastAgentID,
				Type:    domain.MsgFinalDecision,
				Content: "Decision made unilaterally.",
			},
			{
				ToAgent: domain.AgentID("stranger"),
				Type:    domain.MsgDelegate,
				Content: "Please help with this task.",
			},
		}
	}
}

// ErrRuntimeCrashed is the error NewCrashing's adapter reports on Outbound.
var ErrRuntimeCrashed = errors.New("runtime exited unexpectedly")

// CrashingBehavior emits an ERROR protocol message ("agent crashed") in
// response to anything. Pair it with NewCrashing (rather than New) to also
// simulate the underlying runtime failing, i.e. an Outbound with Err set.
func CrashingBehavior() Behavior {
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		return []domain.Message{reply(domain.MsgError, in.FromAgent, "agent crashed", false)}
	}
}

// NewCrashing builds an adapter that, on top of CrashingBehavior's protocol
// message, also pushes a ports.Outbound with Err set for every message it
// receives — modeling a runtime that dies while handling input. Used to test
// agent-failure/crash-detection handling in the manager and orchestrator.
func NewCrashing(id domain.AgentID) *Adapter {
	a := New(id, domain.AgentKindMock, CrashingBehavior())
	a.crashOnDeliver = ErrRuntimeCrashed
	return a
}

// SilentBehavior never responds to anything. Used to test timeout handling.
func SilentBehavior() Behavior {
	return func(self domain.AgentID, t *domain.Task, in domain.Message) []domain.Message {
		return nil
	}
}
