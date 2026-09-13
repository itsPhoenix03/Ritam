# Agent protocol

See also: [../README.md](../README.md), [architecture.md](architecture.md), [task-lifecycle.md](task-lifecycle.md).

This document describes the internal, provider-neutral message protocol agents use to talk to the orchestrator (and, through it, to each other). It is defined in `internal/domain/message.go` and enforced in `internal/orchestrator/authority.go` and `internal/orchestrator/bus.go`.

## 1. The Message struct

```go
type Message struct {
    ID               MessageID
    TaskID           TaskID
    FromAgent        AgentID
    ToAgent          AgentID
    Type             MessageType
    Content          string
    Timestamp        time.Time
    RequiresResponse bool
    InReplyTo        MessageID
    Round            int
    Metadata         map[string]string
}
```

| Field | Meaning |
|---|---|
| `ID` | Unique message id, format `MSG-<12 hex chars>` (`domain.NewMessageID`). Assigned by `MessageBus.Route` if the sender left it blank. |
| `TaskID` | The task this message belongs to. Overwritten by `MessageBus.Route` to the task actually being routed, regardless of what the sender set. |
| `FromAgent` | Sender identity. For adapter-produced messages, `Orchestrator.handleOutbound` fills this from the adapter's verified `out.AgentID` if the message left it blank, and **rejects** the message as an `IDENTITY_VIOLATION` if the message claims a different sender than the adapter's own id — the transport-verified identity always wins over a claimed one. |
| `ToAgent` | Recipient: a concrete participant agent id, `domain.BroadcastAgentID` (`"*"`), or `domain.HumanAgentID` (`"human"`). |
| `Type` | One of the `MessageType` constants below. Unknown types are rejected (`ViolationProtocol`). |
| `Content` | Free-text body. |
| `Timestamp` | Set by `MessageBus.Route` if left zero. |
| `RequiresResponse` | Advisory flag set by senders (e.g. mock behaviors set it on `DELEGATE`/`IMPLEMENT`/`REQUEST_CHANGE`); not currently enforced by the orchestrator itself. |
| `InReplyTo` | Links a reply back to the message that prompted it. The mock adapter fills this automatically (`deliver` sets `InReplyTo = m.ID` when a behavior's reply leaves it blank); the claude/codex adapters do not currently set it. |
| `Round` | The task's deliberation round at the time of routing (`MessageBus.Route` stamps `m.Round = t.Round`). |
| `Metadata` | Adapter- or type-specific extras, e.g. `FINAL_DECISION` messages carry `"decision"`, `"reasoning"`, `"evidence"` keys (see `Orchestrator.proposeDecision`). **Must never contain credentials** — this is a documented invariant, not something the orchestrator scans for. |

## 2. Message types

All types (`domain.AllMessageTypes()`), in protocol order:

| Type | Meaning | Who may send |
|---|---|---|
| `TASK_ASSIGNMENT` | Orchestrator-issued opening brief, sent to the **authority agent only** (see §3.1). | Only ever sent by the orchestrator itself (`Orchestrator.assign`, `FromAgent = human`); not sendable by an agent. |
| `DELEGATE` | Authority agent hands investigation/work to a supporting agent. | **Authority only.** |
| `QUESTION` | An agent asks another something. | Any participant. |
| `ANSWER` | A reply to a `QUESTION` or `DELEGATE`. | Any participant. |
| `INFORMATION` | Free-form information push; also what `Orchestrator.SendInstruction` uses for human-injected messages. | Any participant, and the human. |
| `PROPOSAL` | An agent proposes an approach. | Any participant. |
| `REVIEW` | Feedback on implemented work. | Any participant. |
| `CHALLENGE` | An agent disputes a claim or conclusion. | Any non-`OBSERVER` role (`AgentRole.CanChallenge`) — this is a role-level allowance, not an authority-only type in the protocol enforcement sense. |
| `APPROVE` | Accept a proposal/result. | **Authority only.** |
| `REJECT` | Refuse a proposal/result. | **Authority only.** |
| `REQUEST_CHANGE` | Send implemented work back for revision. | **Authority only.** |
| `IMPLEMENT` | Instruct an agent to make a change. | **Authority only.** |
| `IMPLEMENTATION_RESULT` | Report the outcome of implementation work. | Any participant (typically the implementer). |
| `TEST_RESULT` | Report the outcome of a test run. | Any participant. |
| `FINAL_DECISION` | Ends deliberation and records a durable `Decision`. | **Authority only.** |
| `STATUS_UPDATE` | Free-form progress note. | Any participant. |
| `ERROR` | An agent reports an internal error (distinct from a *runtime* crash, which is carried on `ports.Outbound.Err` instead of as a message). | Any participant. |

`MessageType.RequiresAuthority()` returns true for exactly six types: `FINAL_DECISION`, `APPROVE`, `REJECT`, `REQUEST_CHANGE`, `IMPLEMENT`, `DELEGATE`. These are the ones marked "authority only" above; every other type may be sent by any task participant, subject only to the routing rules below. `CHALLENGE` is notable: it is *not* authority-gated by `RequiresAuthority`, but role-gated separately by `AgentRole.CanChallenge()` — every role except `OBSERVER` may challenge, precisely so that disagreement with the authority agent is always possible, not reserved to authority.

## 3.1 Only the authority agent is briefed

`Orchestrator.assign` sends the opening `TASK_ASSIGNMENT` to `Task.AuthorityAgent` and to nobody else. Supporting agents receive nothing at task start and stay idle until the authority delegates to them.

This is enforcement, not etiquette. Briefing every participant looks harmless but dissolves the authority model in practice — observed in a live run with two real agents:

```
human -> opus   TASK_ASSIGNMENT
human -> scout  TASK_ASSIGNMENT
scout -> human  ANSWER            <- worked on its own reading, unprompted
PLANNING -> DELIBERATING          <- and drove a state transition
opus  -> scout  DELEGATE          <- the authority only planned afterwards
```

The supporting agent spent a real CLI call on work nobody asked for and advanced task state before the authority had decided what investigation was needed. Deciding what work happens is the whole content of the authority role, so the orchestrator enforces it structurally rather than asking agents to restrain themselves.

Two consequences:

- The authority's brief names the idle participants and states that they will not act until delegated to, so it knows it must say what it wants.
- The task record travels with **every** delivery (`AgentAdapter.SendMessage(ctx, t, m)`), not just the assignment. A supporting agent's first contact is typically a `DELEGATE`, and without the task it would not know the task, its own role, or which agent ids it may reply to.

Regression tests: `TestOnlyTheAuthorityIsBriefed`, `TestEndToEndMockFlow` (asserts exactly one `TASK_ASSIGNMENT`), and `TestRenderPromptOrientsAnAgentThatWasNeverBriefed` in both adapter packages.

## 3.2 Unanswered requests are recorded on the decision

`DELEGATE`, `QUESTION`, `IMPLEMENT` and `REQUEST_CHANGE` inherently ask another agent for something back (`MessageType.ExpectsResponse`). This is inferred from the type rather than read from `Message.RequiresResponse`, because the adapters' wire formats carry no such flag — a real agent emits only a type and a recipient.

Before a `FINAL_DECISION` is recorded, `Orchestrator.unansweredRequests` scans the task history for such messages that never got a reply. A reply is any later message from the recipient addressed back to the asker, or a broadcast (which reaches the asker too). The orchestrator's own opening assignment is excluded: the authority answers it by working, not by replying to `human`.

This **does not block the decision**. Judging the evidence sufficient is the content of the authority role, and an authority that wants to stop waiting is entitled to. But the caveat is recorded — as an event, and as a `CAVEAT:` entry in the durable `Decision.Evidence` — so a weaker decision is visibly weaker instead of being silently folded into an accepted result.

Observed in a live run, which is why this exists:

```
09:47:38  codex -> opus  QUESTION           (RequiresResponse)
09:47:38  opus received QUESTION from codex
09:47:43  codex -> *     FINAL_DECISION     <- five seconds later
09:47:43  TASK-001 completed
```

No real CLI answers in five seconds. Opus was still generating its reply when the task closed, and the decision was accepted with the question outstanding and nothing to show it.

Regression tests: `TestDecisionWithUnansweredQuestionIsRecorded` and `TestAnsweredQuestionsProduceNoCaveat`.

## 3. Routing rules

Enforced by `AuthorityManager.checkRouting` (`internal/orchestrator/authority.go`), called for every message including ones from the human:

- **Broadcast (`*`)**: always legal. `MessageBus.deliver` expands a broadcast into an individual send to every task participant other than the sender (never re-delivered to the sender itself).
- **`human`**: always a legal `ToAgent`. Messages addressed to `human` are never delivered to any adapter — `MessageBus.deliver` skips them; the TUI reads them from history instead.
- **Empty recipient**: rejected — `ViolationRouting`, "no recipient".
- **Self-addressing**: rejected if `ToAgent == FromAgent` — `ViolationRouting`, "agent addressed itself". No agent can talk to itself through the protocol.
- **Non-participant recipient**: rejected unless the recipient is a participant of the same task — `ViolationRouting`, `"recipient %q is not a participant of this task"`. This is what stops `RogueBehavior`'s attempt to `DELEGATE` to `"stranger"`, an agent outside the task, in `TestAuthorityViolationIsRejected`.
- **Non-participant sender**: separately rejected as `ViolationIdentity` ("sender is not a participant of this task") before routing is even checked, unless the sender is the human (`domain.HumanAgentID` always bypasses the participant check and goes straight to `checkRouting`).

## 4. Validation and the five Violation kinds

`AuthorityManager.Authorize` (`internal/orchestrator/authority.go`) is the single validation entry point, called from `MessageBus.Route` before a message is recorded or delivered. It returns a `*Violation` — never a bare error — when rejecting, carrying `Kind`, `TaskID`, `AgentID`, `Message` (the attempted type) and a human-readable `Reason`. The five kinds:

| Kind | When raised | Event emitted |
|---|---|---|
| `AUTHORITY_VIOLATION` | A message type with `RequiresAuthority() == true` sent by an agent that is not `Task.AuthorityAgent`. | `EvtAuthorityViolation` |
| `ROUTING_VIOLATION` | Illegal `ToAgent`: empty, self-addressed, or not a task participant/broadcast/human. | `EvtValidationRejected` |
| `PROTOCOL_VIOLATION` | `Type` is not one of the known `MessageType` constants. | `EvtValidationRejected` |
| `IDENTITY_VIOLATION` | Sender is not a task participant (raised inside `Authorize`); separately, `Orchestrator.handleOutbound` raises the same kind (as an event only, not via `Authorize`) when an adapter's message claims a `FromAgent` different from the adapter's own verified id. | `EvtValidationRejected` |
| `STATE_VIOLATION` | Raised by `AuthorityManager.Authorize`: any agent-originated message (i.e. `FromAgent != domain.HumanAgentID`) is rejected when the task is terminal (`t.State.IsTerminal()`: `COMPLETED`/`FAILED`/`CANCELLED`) or `PAUSED`, with `Reason: "task is <STATE>; only the human can act on it"`. The human check happens first and is exempt, so the human can still drive a `PAUSED` task (e.g. to `Resume` it); no agent can act on a stopped or paused task. This is separate from — and checked before — `domain.CanTransition`, which still governs illegal *transitions* on tasks that are otherwise open to agents (`TaskManager.Transition`/`Orchestrator.handleOutbound` silently keep the current state, logged at debug level, rather than raising a violation, when a message implies an illegal transition on an active task). See `TestAgentsCannotActOnAStoppedTask` in `internal/orchestrator/orchestrator_test.go`. | `EvtValidationRejected` |

A rejected message is **recorded as an event and never delivered**; the message itself is not appended to the message log (`MessageBus.Route` returns before calling `messages.Append`). Critically, a rejection does not abort the task — `Orchestrator.handleOutbound` catches the `*Violation`, logs a warning, and returns `done=false`, so the run loop keeps going. A misbehaving agent cannot stall or crash the task by attempting something it isn't allowed to do; it can only get itself recorded as having tried.

### Rejection is reported back to the agent

Silence is not an acceptable response to a refused action. After recording the violation, `Orchestrator.notifyRejection` routes an `ERROR` message from `human` to the offending agent naming the rejected message type, the violation kind and the reason, with `InReplyTo` set to the rejected message and `Metadata["violation"]` carrying the kind:

```
Your FINAL_DECISION was rejected by the orchestrator (AUTHORITY_VIOLATION):
only the authority agent "opus" may send FINAL_DECISION. The action did not
take effect. Correct it and try again.
```

Without this, an agent that emits something invalid believes it acted while the orchestrator has dropped the action, and the task idles until `IdleTimeout` pauses it for no useful reason. Three guards prevent a feedback loop: an `ERROR` is never answered with an `ERROR`, nothing is sent to a sender that is not a task participant, and nothing is sent when the task is terminal or `PAUSED`. See `TestRejectedActionIsReportedBackToTheAgent`.

### Tasks must have someone to delegate to

`TaskManager.Create` rejects a task whose only participant is the authority agent, and rejects a duplicated participant. An authority-only task is not merely useless — it stalls: the authority has nobody to address, so a `DELEGATE` with an empty recipient is refused as `ROUTING_VIOLATION` and the run idles until the timeout. Failing at creation puts the error in front of the human immediately. The TUI enforces the same rule before submitting, and pre-selects every agent in the new-task form. See `TestAuthorityOnlyTaskIsRejected` and `TestDuplicateParticipantIsRejected`.

## 5. Delegation flow

`DELEGATE` is how the authority agent hands off investigation or other work to a supporting agent. Concretely, in the mock scaffold flow (`mockagent.AuthorityBehavior`):

```
opus receives TASK_ASSIGNMENT
  -> opus sends DELEGATE to codex ("Investigate cookie creation and logout behavior...")
     AuthorityManager.Authorize: DELEGATE requires authority; opus IS authority -> OK
     MessageBus.Route: records message, emits AgentMessageSent + AgentDelegatedTask,
                        delivers to codex's adapter via SendMessage
     StateForMessage(DELEGATE) = INVESTIGATING -> task transitions PLANNING -> INVESTIGATING
```

If a non-authority agent attempts `DELEGATE` (as `RogueBehavior` does, addressing a non-participant no less), `Authorize` rejects it as `AUTHORITY_VIOLATION` before the routing check is even reached — `TestAuthorityViolationIsRejected` asserts exactly this.

## 6. Challenge / disagreement flow — worked example

Disagreement is a first-class, recorded outcome, not something the protocol tries to prevent or hide. `domain.Disagreement` (`internal/domain/decision.go`) holds:

```go
type Disagreement struct {
    ID               DisagreementID
    TaskID           TaskID
    Claimant         AgentID
    Challenger       AgentID
    ChallengeMessage MessageID
    Claim            string
    Counterclaim     string
    Evidence         []string
    Resolved         bool
    Resolution       string
    ResolvedBy       AgentID
    Timestamp        time.Time
    ResolvedAt       *time.Time
}
```

`ID` is generated by `domain.NewDisagreementID()` (format `DIS-<hex>`, same pattern as other domain ids). It is persisted through a dedicated `ports.DisagreementRepository` (`Create`/`Update`/`Get`/`ListByTask`/`ListOpen`), backed today by `storage.MemDisagreementRepository`, and reachable from the application layer via `app.Service.TaskDisagreements(ctx, id)` — see [architecture.md](architecture.md) for the repository and the `TODO(phase-next)` on why that method isn't on `ports.Service` yet.

Lifecycle, driven entirely by `Orchestrator`:

- **Raised on `CHALLENGE`.** Whenever a `CHALLENGE` message is successfully routed, `Orchestrator.recordDisagreement` creates a `Disagreement`: `Claimant` is the challenged agent (`m.ToAgent`), `Challenger` is the sender (`m.FromAgent`), `ChallengeMessage` is the `CHALLENGE` message's id, `Counterclaim` is the challenge's `Content`, and `Evidence` is populated from `Metadata["evidence"]` when present. `Claim` — the thing actually being disputed — is resolved by `claimUnderChallenge`: it uses the content of the message the `CHALLENGE` is `InReplyTo`, and falls back to the most recent prior message from the challenged agent if `InReplyTo` is empty. The record starts `Resolved: false`.
- **Resolved on `FINAL_DECISION`.** `Orchestrator.handleFinalDecision` calls `resolveDisagreements`, which lists every still-open (`ListOpen`) disagreement for the task and, for each, sets `Resolved = true`, `Resolution` to the final decision's text, `ResolvedBy` to the deciding agent, and `ResolvedAt` to now — updating the existing record in place via `Update`, never deleting or replacing it. A disagreement that never gets a `FINAL_DECISION` (e.g. the task instead hits a round limit and pauses) stays open indefinitely, which is deliberate: the round-limit path in the worked example below never manufactures a resolution.

Both storage operations are best-effort: if `o.storage.Disagreements` is nil, or a `Create`/`Update` call fails, the orchestrator logs a warning and continues — a disagreement-tracking failure never blocks message routing or the task's own progress.

Worked example, taken directly from `TestChallengeLoopHitsRoundLimit` using `mockagent.ChallengingImplementerBehavior`:

```
opus  --DELEGATE------------------------> codex
                                            (instead of answering, codex disputes
                                             the premise of the delegation)
opus  <--CHALLENGE------------------------ codex
       "I believe that is not the root cause. Evidence: the cookie is
        cleared correctly in the integration test."
       AuthorityManager.Authorize: CHALLENGE does not require authority
       (any non-OBSERVER role may send it) -> OK
       StateForMessage(CHALLENGE) = DELIBERATING
       Orchestrator.recordKnowledge: stored as an INVESTIGATION entry
       EvtAgentChallenged emitted

opus  --DELEGATE (again)------------------> codex
       "Provide evidence for the alternative root cause."
       StateForMessage(DELEGATE) = INVESTIGATING
       (opus's AuthorityBehavior responds to any CHALLENGE by re-delegating,
        asking for evidence rather than overruling the objection outright)

... this DELEGATE -> CHALLENGE cycle repeats indefinitely, because
    ChallengingImplementerBehavior always challenges a DELEGATE ...

each opus DELEGATE increments Task.Round (rounds advance when the
AUTHORITY agent speaks); once Round >= MaxDeliberationRounds:

  LimitExceeded(t) = (PauseReasonMaxRounds, true)
  EvtLimitReached emitted
  Orchestrator.Tasks.Pause(..., PauseReasonMaxRounds)
  task ends PAUSED, not COMPLETED — the round limit never
  auto-resolves the disagreement in either agent's favor
```

The key property this demonstrates: authority never means "the challenger is automatically wrong." The authority agent's `AuthorityBehavior` responds to a `CHALLENGE` by asking for more evidence (another `DELEGATE`), not by silently overruling it — and if agreement is never reached, the outcome is a `PAUSED` task awaiting a human, never a forced `FINAL_DECISION`.

The first `CHALLENGE` in that exchange also produces a `Disagreement` record, via `Orchestrator.recordDisagreement`, that outlives the pause:

```
Disagreement{
    ID:               "DIS-<hex>",
    TaskID:            <task id>,
    Claimant:          "opus",       // the challenged agent (m.ToAgent)
    Challenger:        "codex",      // the sender (m.FromAgent)
    ChallengeMessage:  <id of the CHALLENGE message>,
    Claim:             <opus's prior DELEGATE content, via claimUnderChallenge>,
    Counterclaim:      "I believe that is not the root cause. Evidence: the
                        cookie is cleared correctly in the integration test.",
    Evidence:          [] or [Metadata["evidence"]] if the CHALLENGE set it,
    Resolved:          false,
    Timestamp:          <CHALLENGE message's timestamp>,
}
```

`TestChallengeLoopHitsRoundLimit` fetches this record via `storage.Disagreements.ListByTask` after the task pauses and asserts it exists with `Resolved == false` — the round limit pauses the task but does not touch the disagreement either way. Contrast this with `TestFinalDecisionResolvesOpenDisagreements`, which seeds an open disagreement, runs a task to `FINAL_DECISION`, and asserts the same record is still present (never deleted) but now has `Resolved == true`, `ResolvedBy` set to the deciding agent, and `Resolution` set to the decision text — demonstrating the `resolveDisagreements` half of the lifecycle.

## 7. Wire formats (live conventions — driving the real CLIs)

Both formats below are this project's own convention, not a feature of either CLI: Claude Code and Codex both return free text (or, for Claude, a JSON envelope wrapping free text), and the header line is how that text is turned back into a routed `domain.Message`. As of the current adapters (`internal/agents/claude`, `internal/agents/codex`), these formats are what is actually sent to and parsed from **real** `claude`/`codex` processes — see [future-integration.md](future-integration.md) for the full invocation detail (exact flags, the one-shot transport, and the knowledge-injection mechanism described in section 8 below).

### Claude (`internal/agents/claude`)

**Outbound prompt** (`renderPrompt`), passed as the `-p` argument to `claude`:

```
You are a coding agent participating in a multi-agent deliberation.
An orchestrator routes every message; you never talk to another agent directly.

YOUR AGENT ID: <id>
YOUR ROLE: AUTHORITY. ...            (or: SUPPORTING AGENT. The authority for this task is "<id>". ...)

TASK <task id>: <title>
<description>

AGENTS YOU MAY ADDRESS (use these exact ids):
  - <participant>  (authority)
  - <participant>
  - human            (to ask the operator)
  - *                (broadcast to all participants)

RELEVANT PROJECT KNOWLEDGE (established earlier in this task):   <- only when present, see section 8
- [<category>] <title>
  <body>

--- INCOMING <TYPE> FROM <agent> ---
<message content>
--- END ---

You are running READ-ONLY: ...        (or: You may edit files and run commands in your working directory.)

REPLY FORMAT — this is mandatory. Your reply MUST begin with a header line:
  @@ORCH type=<MESSAGE_TYPE> to=<agent id>
followed by your message body. ...

Valid MESSAGE_TYPE values:
  TASK_ASSIGNMENT, DELEGATE, QUESTION, ANSWER, INFORMATION, PROPOSAL, REVIEW, CHALLENGE,
  APPROVE, REJECT, REQUEST_CHANGE, IMPLEMENT, IMPLEMENTATION_RESULT, TEST_RESULT,
  FINAL_DECISION, STATUS_UPDATE, ERROR

Example:
  @@ORCH type=DELEGATE to=codex
  Investigate how the session cookie is created and cleared. Do not change code yet.
```

**Inbound reply header** (`parseOutput`), read from the `.result` field of the `--output-format json` envelope (`extractResult` in `adapter.go` unwraps the envelope first — see [future-integration.md](future-integration.md#2-claude-code-adapter-internalagentsclaude)):

- Header grammar: `@@ORCH type=<TYPE> to=<agent-id>` (whitespace-separated `key=value` fields after the `@@ORCH` marker; `parseHeaderFields` splits on `=` once per field, and trims a trailing `.` and surrounding quotes off each value).
- Everything between one `@@ORCH` line and the next (or end of input) becomes that message's `Content`, with leading/trailing blank lines trimmed.
- `type` is matched case-sensitively against `domain.IsKnownMessageType`; an unknown type is a hard parse error (`"unknown message type %q in agent output"`), not silently dropped or passed through.
- A missing `to` field is a hard parse error.
- A leading fence or bullet decoration (`` ` ``, `>`, `-`, `*`, or surrounding whitespace) in front of the marker is tolerated — models routinely decorate headers when they render markdown — but text before the first header, or between recognized blocks, is treated as conversational filler and dropped.
- If the model's entire reply carries no `@@ORCH` header at all, the adapter reports this as an error (`"agent %s replied without an @@ORCH header"`) rather than treating silence as a valid empty response, so the orchestrator surfaces a stuck agent instead of idling out.

### Codex (`internal/agents/codex`)

**Outbound prompt** (`renderPrompt`) — same shape and same section ordering as Claude's (role/authority, task, addressable ids, knowledge, incoming message, sandbox constraint, reply format), passed as the final positional argument to `codex exec`:

```
...
You are running in the read-only sandbox: ...    (or: You are running in the workspace-write sandbox: ...)

REPLY FORMAT — this is mandatory. Your final message MUST begin with a header:
  ##CODEX TYPE=<MESSAGE_TYPE> TO=<agent id>
followed by your message body. ...

Example:
  ##CODEX TYPE=ANSWER TO=opus
  The cookie is created with Path=/ but cleared with Path=/api.
```

**Inbound reply header** (`parseOutput`), read from the file named by `--output-last-message` — never scraped from stdout, which also carries the Codex banner, reasoning summaries and progress text (see [future-integration.md](future-integration.md#3-codex-adapter-internalagentscodex)):

- Header grammar: `##CODEX TYPE=<TYPE> TO=<agent-id>`, **keys case-insensitive** (`parseHeaderFields` lower-cases keys, so both `TYPE=`/`type=` and `TO=`/`to=` are accepted) — this is the one deliberate difference from the claude package's parser, besides the marker string itself.
- `type`'s value is upper-cased before matching against `domain.IsKnownMessageType`.
- Same content-accumulation, fence/bullet tolerance, missing-`to`/`TO`-is-an-error, and unknown-type-is-an-error behavior as the claude adapter.
- If the output-last-message file is empty, missing, or unreadable, the adapter reports an error (`"agent %s produced no final message"`) with the CLI's own first stderr line appended as a hint when there is one (`stderrHint`) — e.g. an out-of-credits or not-logged-in message from Codex itself surfaces directly rather than being swallowed.

### Why the two formats deliberately differ

Both package doc comments state this explicitly: Claude Code and Codex are "different applications" / "separate applications," and "sharing an assumed protocol between them would be wrong the moment either changes." `@@ORCH` vs `##CODEX`, and case-sensitive vs case-insensitive header keys, are a deliberate reminder — enforced by keeping the parsing logic in two independent packages rather than one shared implementation — that nothing about one CLI's real output behavior should be assumed to apply to the other. A change to how Claude Code happens to format its replies must not silently alter how Codex's replies are parsed, and vice versa.

Both formats are validated against real CLI output: both adapters drive the actual `claude`/`codex` binaries, and both have completed confirmed live round trips end to end — prompt sent, real reply received, parsed and routed by the orchestrator. A three-agent run exercised a Claude authority deliberating with a Claude implementer and a Codex implementer simultaneously.

One transport difference matters for the Codex format. The prompt is delivered on **stdin** (`codex exec ... -`), not as an argument: on Windows the `codex` executable on PATH is a `.cmd` shim, so Windows runs it through `cmd.exe`, which re-parses the command line and truncates a multi-line argument at its first newline. That failure was silent and misleading — the agent received only the first two lines of its brief and replied "Understood. Send the coding task", which the adapter then reported as a missing header. See [future-integration.md](future-integration.md).

## 8. Knowledge injection into delivered messages

Real agent processes are one-shot (`core.OneShotRunner` — one CLI invocation per protocol message, the process must exit before its reply counts; see [future-integration.md](future-integration.md#1-the-transport-one-cli-process-per-protocol-message)), so nothing survives in the agent's own memory between calls. `MessageBus.Enrich` — a hook on `MessageBus` (`internal/orchestrator/bus.go`), wired by `orchestrator.New` to `Orchestrator.attachKnowledge` — is what replaces that memory:

- It runs inside `MessageBus.deliver`, after the message has already been recorded via `Route`/`Append`, on a **clone** of the message made specifically for that recipient.
- `attachKnowledge` retrieves up to `knowledgeContextLimit` (6) relevant entries from `knowledge.Store.ContextFor`, keyed by the task and the message's own content (or the task title if the content is empty), and renders them into `delivered.Metadata["knowledge"]`.
- Both adapters' `renderPrompt` read that key and, when non-empty, inject a `RELEVANT PROJECT KNOWLEDGE` section into the prompt ahead of the incoming message (see the outbound prompt sketches above).
- **The message log is unaffected.** `Enrich` mutates only the per-recipient delivered copy; the message appended to `ports.MessageRepository` by `Route` earlier in the pipeline never carries the injected knowledge. `MessageBus.History` and anything reading the recorded conversation sees exactly what was sent, not what a particular recipient's prompt was padded with.

This is the mechanism, not a convenience feature: without it, a real one-shot agent would have no way to know what happened in the task before the message currently in front of it. See [future-integration.md](future-integration.md#4-context-across-stateless-calls-the-knowledge-injection-mechanism) for the full walkthrough.

## 9. Parsing tolerance and rejection, summarized

Both adapters' `parseOutput` apply the same discipline to untrusted model output:

- **Tolerated:** leading markdown fencing or bullet/quote decoration directly in front of the header marker; prose before the first header or between recognized blocks (dropped as filler, not an error); multiple header blocks in one reply (each becomes a separate `domain.Message`); extra whitespace around `key=value` fields; a trailing period or surrounding quotes on a field value.
- **Rejected as a hard parse error, aborting that invocation's message production entirely:** an unknown `MESSAGE_TYPE` value (checked against `domain.IsKnownMessageType`, the same vocabulary `AllMessageTypes()` lists in the prompt); a header with no recipient (`to`/`TO` missing or empty); a reply that contains no header at all.

A rejected parse never becomes a routed message — it becomes an adapter-level error (`ports.Outbound.Err`), which `Orchestrator.handleOutbound` treats as an agent runtime failure and fails the task (see [task-lifecycle.md](task-lifecycle.md#8-terminal-states)), the same as a CLI crash or timeout would. This is deliberately stricter than the routing-time validation in section 4 above (which rejects *known but disallowed* actions and keeps the task alive) — a reply the adapter cannot parse at all as protocol never reaches `MessageBus.Route` to be evaluated that way in the first place.
