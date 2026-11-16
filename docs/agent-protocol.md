# Agent protocol

See also: [../README.md](../README.md), [architecture.md](architecture.md), [task-lifecycle.md](task-lifecycle.md).

The provider-neutral message protocol agents use to talk to the orchestrator and, through it, to each other. Types are defined in `internal/domain/message.go`; validation is in `internal/orchestrator/authority.go`; routing and recording in `internal/orchestrator/bus.go`; the reply parser in `internal/agents/core/reply.go`; the prompts in `internal/agents/core/prompt.go`.

## 1. The Message

| Field | Meaning |
|---|---|
| `ID` | `MSG-<hex>`, assigned by `MessageBus.Route` if empty. |
| `TaskID` | Set by `Route` to the task being routed, whatever the sender wrote. |
| `FromAgent` | The sender. For an agent's reply, the adapter's own agent id; a reply claiming another sender is refused (`IDENTITY_VIOLATION`). |
| `ToAgent` | A participant id, `*` (every other participant), or `human`. `orchestrator` is never a valid recipient. |
| `Type` | One of the types in section 2. |
| `Content` | Free text. |
| `Timestamp`, `Round` | Set by `Route`; `Round` is the task's round at routing time. |
| `RequiresResponse` | Set by the human's messages and the opening assignment; for agents, whether a message asks for a reply is read from its type (`ExpectsResponse`: `DELEGATE`, `QUESTION`, `IMPLEMENT`, `REQUEST_CHANGE`). |
| `InReplyTo` | Pins a reply to one request. The mock agents and the orchestrator's own notes set it; the Claude and Codex adapters do not (the header parser reads an `in_reply_to` key but does not use it). |
| `Metadata` | Extras: a `REVIEW`'s `verdict`, a `FINAL_DECISION`'s `decision`/`reasoning`/`evidence`, the orchestrator's note kind, and the per-delivery context added for the recipient (section 7). Never credentials. |

## 2. Types and who may send them

| Type | Meaning (as the prompts state it) | Sendable by |
|---|---|---|
| `TASK_ASSIGNMENT` | hand over a whole part of the task | authority only; the task's opening brief is one, sent as `human` |
| `DELEGATE` | investigate and report back | authority only |
| `IMPLEMENT` | change files | authority only; refused if the recipient is read-only |
| `QUESTION` | you need information | anyone |
| `ANSWER` | reply to a request | anyone |
| `IMPLEMENTATION_RESULT` | what you changed, where | anyone |
| `TEST_RESULT` | command, exit code, output | anyone |
| `STATUS_UPDATE` | progress on longer work | anyone |
| `INFORMATION` | something others should know; asks nothing | anyone |
| `PROPOSAL` | a plan or change for others to weigh | anyone |
| `REVIEW` | a judgement, with `verdict=approve\|changes\|reject` | anyone |
| `CHALLENGE` | you disagree; give the evidence | anyone |
| `REQUEST_CHANGE` | send work back, saying what must change | authority only |
| `APPROVE` | accept a proposal or result | authority only |
| `REJECT` | turn it down, saying why | authority only |
| `FINAL_DECISION` | the outcome; ends the task | authority only |
| `ERROR` | how the orchestrator reports a refused or unreadable reply | offered to no agent |

`MessageType.RequiresAuthority()` is true for the seven authority-only types. A supporting agent is offered only the others (`domain.AllowedTypes`), and a refusal tells it what to send instead (`domain.SupportingAlternative`): a `REVIEW` with a verdict instead of `APPROVE`/`REJECT`/`REQUEST_CHANGE`, a `QUESTION` instead of `DELEGATE`/`IMPLEMENT`/`TASK_ASSIGNMENT`, a `PROPOSAL` instead of `FINAL_DECISION`. Agent role labels (`REVIEWER`, `OBSERVER`) change none of this; only the task's authority does.

## 3. The three kinds of sender

- **Agents**, through their adapters. Everything they send is validated.
- **The human** (`human`): instructions, follow-ups, the opening assignment, a rejection of a held decision (sent as a `REJECT` to the authority). The human's messages are checked only for routing, never for authority or the task's state.
- **The orchestrator** (`orchestrator`): its own notes to agents, sent as `INFORMATION` (reminders, checkpoints, resume and retry notes, authority changes, requests closed as unanswered, a dropped agent, a deferred decision) or `ERROR` (a refused action, an unreadable or partly readable reply), each marked with metadata naming its kind. The prompts show them as notices that need no reply. A note never opens or answers a request, and an agent addressing `orchestrator` is refused with a reason. History written before the orchestrator had its own id recorded these notes as `human`; `domain.FromOrchestrator` recognises them by their metadata, and every reader of the history uses that one rule.

Neither `human` nor `orchestrator` (nor `*`) can be an agent's id: recruiting and task creation refuse them.

## 4. Only the authority is briefed

`Orchestrator.assign` sends the opening `TASK_ASSIGNMENT` to the authority and nobody else; supporting agents stay idle until asked. Briefing everyone was tried and broke the authority model in a live run: a supporting agent answered unprompted and moved the task before the authority had planned anything. The brief names the idle participants and includes the team roster with guidance on choosing among them. The task travels with every delivery, so a supporting agent's first `DELEGATE` still tells it the task, its role and who it may address. Tests: `TestOnlyTheAuthorityIsBriefed`, `TestEndToEndMockFlow`.

## 5. Requests and answers

The request ledger (`domain.BuildLedger`) is derived from the history whenever it is needed. A message opens a request when it is addressed to one party and asks for a reply (by type for agents, by `RequiresResponse` for the human); the opening assignment, `ERROR`, `FINAL_DECISION` and broadcasts do not. A later message from the recipient to the asker (or a broadcast) closes it, with two exceptions: a `QUESTION` back does not answer the request it asks about, and a `STATUS_UPDATE` does not finish a `DELEGATE`, `IMPLEMENT` or `REQUEST_CHANGE` (it does answer a `QUESTION`, or the human). A reply to whoever started a chain of requests closes the chain down to the agent replying. The orchestrator's notes neither open nor close requests, except its closing note for a request left unanswered after a reminder.

The ledger drives reminders, pauses for an unanswered human question, the caveat recorded on a decision taken with requests open (`CAVEAT:` in the decision's evidence, see `TestDecisionWithUnansweredQuestionIsRecorded`), and the hold on a decision while implementation is outstanding ([task-lifecycle.md](task-lifecycle.md)).

## 6. Validation

`AuthorityManager.Authorize` runs on every message before it is recorded. In order:

1. The type must be known, else `PROTOCOL_VIOLATION`.
2. A message from `human` or `orchestrator` goes straight to the routing check.
3. A task that is `PAUSED`, `COMPLETED`, `FAILED` or `CANCELLED` accepts nothing from an agent: `STATE_VIOLATION`.
4. The sender must be a participant: `IDENTITY_VIOLATION`.
5. An authority-only type from anyone but the task's authority: `AUTHORITY_VIOLATION`.
6. A `REVIEW` verdict must read as approve, changes or reject (common spellings are normalised): else `PROTOCOL_VIOLATION`. A verdict on any other type is dropped.
7. Routing: the recipient must be `*`, `human` or another participant of the task; empty, self-addressed, `orchestrator` or a non-participant is a `ROUTING_VIOLATION`.
8. An `IMPLEMENT` to an agent that cannot write files is a `ROUTING_VIOLATION` naming the agents that can. `REQUEST_CHANGE` is let through: it is also how the authority asks a read-only reviewer to revise its own work.

Separately, `Orchestrator.handleOutbound` refuses a reply whose claimed sender differs from the adapter that produced it, and one from an agent running under a reserved id.

A refused message is recorded as an event (`AuthorityViolation` or `ValidationRejected`), never stored as a message and never delivered. The sender gets an `ERROR` notice naming the type, the violation and the reason (not when the refused message was itself an `ERROR`, the sender is not a participant, or the task is paused or finished). The task continues; refused actions do not stall it. Tests: `TestAuthorityViolationIsRejected`, `TestAgentsCannotActOnAStoppedTask`, `TestRejectedActionIsReportedBackToTheAgent`.

A task must have an authority and at least one other participant, with no participant listed twice (`TaskManager.Create`; `TestAuthorityOnlyTaskIsRejected`, `TestDuplicateParticipantIsRejected`).

## 7. What a delivery carries

A valid message is recorded together with a queued delivery for each recipient that will be handed it, in one transaction. It reaches the recipient in its next turn, combined with whatever else waits for it (see [architecture.md](architecture.md)). Each recipient's copy, never the stored message, gets:

- `knowledge`: up to six relevant shared-knowledge entries, shown as `RELEVANT PROJECT KNOWLEDGE`;
- a recap of the task so far (the last 15 messages the recipient was party to, everything for the authority), used only when the agent starts or rebuilds a session;
- a thread note when the request is part of a question the human asked, saying it may answer the human directly;
- the team roster and board (who may write and build, who is working, unread counts, who waits on whom);
- when the task or the day has a cost limit, the turn's spending allowance (Codex turns get none; the Claude Code adapter passes it as `--max-budget-usd`).

A message to `human` is never delivered to an agent; a message to a dropped agent is recorded and its sender told it was not delivered; a `FINAL_DECISION` that completes the task at once is recorded without waking the recipients.

## 8. The reply format

Each prompt ends with the reply format. The agent does its work, then writes one final message that starts with a header line, followed by the body; several messages are several header-and-body blocks:

```
@@ORCH type=ANSWER to=lead            (Claude Code)
##CODEX TYPE=ANSWER TO=lead           (Codex)
```

The example in every prompt is built from the task: a type that agent may send, to an id it may address. Both adapters parse replies with `core.ParseReply`; only the marker differs.

- **Header fields.** Keys are case-insensitive; `key=value`, `key: value` and `key = value` are read, and a value may be quoted. Only `type`, `to` and `verdict` are used (`item` and `in_reply_to` are read and ignored); reading stops at the first word that is not a field, so prose on the header line is ignored. Trailing punctuation and markdown emphasis are trimmed from a value (a lone `*` stays the broadcast address).
- **Decoration.** A header that is fenced, bulleted or quoted is still a header. Text before the first header is dropped.
- **`NO_REPLY`.** A reply that is empty or exactly `NO_REPLY` (code fences ignored) is the agent saying nothing; no message is produced and it is not a failure. The prompts offer it for when nothing is asked of the agent, and ask agents not to send thanks or acknowledgements.
- **Partly readable.** An unknown type is delivered as `INFORMATION`; a block with no recipient is dropped. The rest of the reply is delivered and the agent is sent an `ERROR` notice saying what to correct.
- **Unreadable.** A reply with text but no header, or none of whose blocks can be delivered, is a `malformed` failure carrying the raw text. It never becomes a message, and it does not fail the task: the agent is shown what it sent and the exact header to write, with this task's real ids, and asked again. After two unreadable replies in a row a supporting agent is dropped from the task and the authority is told; an authority's task is paused, quoting what it sent. Unreadable replies count toward the stall limit.

For Claude Code the reply is the `result` of the final `stream-json` event; for Codex it is read from the `--output-last-message` file. Only the CLI's final message is delivered, which is why the prompts say text before the last tool call is lost.

## 9. The prompts

`core.RenderBrief` (a new or rebuilt session) and `core.RenderTurn` (every resumed turn) lay a prompt out in the same order: who the agent is and its role on this task (authority or supporting), the task (a resumed turn repeats its title and the first lines of the description), who it may address (dropped agents left out), shared knowledge, the recap (new sessions only), what arrived, the rules (how it works with the others, and either the read-only note or its write rules for the access level), and last the reply format with the types this agent may send, each with its meaning. Golden files under `internal/agents/core/testdata/prompts` and `internal/orchestrator/testdata/prompts` pin the text (`TestGoldenPromptsMatchTheirFiles`).

## 10. Disagreements

Every routed `CHALLENGE` creates a `domain.Disagreement`: the challenger, the challenged agent, the disputed claim (the message the challenge replies to, else the challenged agent's latest message), the counterclaim and any evidence. A `FINAL_DECISION` resolves every open disagreement with the decision's text; a human rejecting that decision reopens them. Records are updated, never deleted. They are stored and readable through `app.Service.TaskDisagreements`, which is not yet on `ports.Service`; the UI does not show them. Tests: `TestChallengeLoopIsPausedAsAStall`, `TestFinalDecisionResolvesOpenDisagreements`.
