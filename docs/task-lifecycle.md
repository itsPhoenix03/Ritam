# Task lifecycle

See also: [../README.md](../README.md), [architecture.md](architecture.md), [agent-protocol.md](agent-protocol.md).

The task state machine lives in `internal/domain/task.go`. It is the authoritative, orchestrator-owned model of "where a task is" — no agent conversation state, message count, or adapter status is ever consulted to answer that question; only `Task.State`, set exclusively by `TaskManager.Transition`, is.

## 1. States

| State | Meaning |
|---|---|
| `CREATED` | Task record exists; participants have not yet been started or assigned. |
| `PLANNING` | Task has started; opening `TASK_ASSIGNMENT` messages are going out. |
| `INVESTIGATING` | Someone has been delegated to, or asked a question, to gather information. |
| `IMPLEMENTING` | An `IMPLEMENT` or `REQUEST_CHANGE` is in flight — code/content is being changed. |
| `REVIEWING` | An implementation result or test result is being evaluated. |
| `DELIBERATING` | Agents are exchanging answers, proposals or challenges without a concrete implementation action in flight. |
| `AWAITING_DECISION` | The authority agent has issued a `FINAL_DECISION`; if `requireHumanApproval` is set, the task sits here until a human `Approve`/`Reject`s it. |
| `PAUSED` | Halted — by a human, by loop-protection limits, or by an idle timeout — with `PreviousState` remembered so it can resume where it left off. |
| `COMPLETED` | Terminal. A decision was accepted (by the orchestrator automatically, or by human approval) and the task is done. |
| `FAILED` | Terminal. The task could not proceed (an agent runtime error, or a start failure). `FailureReason` records why. |
| `CANCELLED` | Terminal. A human ended the task deliberately. |

`TaskState.IsTerminal()` is true only for `COMPLETED`, `FAILED`, `CANCELLED`.

**`PAUSED` and every terminal state are closed to agents.** This is enforced one layer below the transition table, in `AuthorityManager.Authorize` (`internal/orchestrator/authority.go`): any message from an agent (`FromAgent != domain.HumanAgentID`) is rejected with `STATE_VIOLATION` ("task is `<STATE>`; only the human can act on it") whenever `t.State.IsTerminal()` or `t.State == domain.TaskPaused`. The human is checked first and is exempt from this rule entirely, so a paused task can still be driven forward — e.g. to `Resume` it — by the human, even though no agent's message will be accepted while it sits `PAUSED`. See [agent-protocol.md](agent-protocol.md#4-validation-and-the-five-violation-kinds) and `TestAgentsCannotActOnAStoppedTask`.

## 2. Full transition table (corrected)

Reproduced exactly from the `transitions` map in `internal/domain/task.go`. `CanTransition(from, to)` is the sole authority for whether a transition is legal; `from == to` is always rejected as a no-op-by-design (`TaskManager.Transition` short-circuits and returns the unchanged task rather than calling `CanTransition` in that case).

| From | Legal To states |
|---|---|
| `CREATED` | `PLANNING`, `PAUSED`, `CANCELLED`, `FAILED` |
| `PLANNING` | `INVESTIGATING`, `IMPLEMENTING`, `DELIBERATING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `INVESTIGATING` | `PLANNING`, `IMPLEMENTING`, `DELIBERATING`, `REVIEWING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `IMPLEMENTING` | `REVIEWING`, `INVESTIGATING`, `DELIBERATING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `REVIEWING` | `IMPLEMENTING` (via `REQUEST_CHANGE`), `INVESTIGATING`, `DELIBERATING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `DELIBERATING` | `INVESTIGATING` (via `CHALLENGE`), `IMPLEMENTING`, `REVIEWING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `AWAITING_DECISION` | `COMPLETED`, `DELIBERATING`, `IMPLEMENTING`, `INVESTIGATING`, `PAUSED`, `CANCELLED`, `FAILED` |
| `PAUSED` | `CREATED`, `PLANNING`, `INVESTIGATING`, `IMPLEMENTING`, `REVIEWING`, `DELIBERATING`, `AWAITING_DECISION`, `CANCELLED`, `FAILED` |
| `COMPLETED` | *(none — terminal)* |
| `FAILED` | *(none — terminal)* |
| `CANCELLED` | *(none — terminal)* |

### `AWAITING_DECISION` is reachable from every working state, not just `REVIEWING`/`DELIBERATING`

This changed from an earlier version of the table that only allowed `REVIEWING -> AWAITING_DECISION` and `DELIBERATING -> AWAITING_DECISION`. The current table adds `AWAITING_DECISION` as a legal successor of `PLANNING`, `INVESTIGATING` and `IMPLEMENTING` as well — i.e. every non-terminal, non-paused working state.

The reason is a real failure, not a hypothetical: the authority agent reached a conclusion and issued `FINAL_DECISION` while the task was still `INVESTIGATING` — a question answered in one round, with no implementation or review phase ever entered. The decision was recorded and accepted, and the run then died with `invalid task transition INVESTIGATING -> COMPLETED`, leaving an already-decided task stranded short of `COMPLETED`. The fix has two parts:

1. The transition table itself now allows `AWAITING_DECISION` from `PLANNING`, `INVESTIGATING`, `IMPLEMENTING`, `REVIEWING` and `DELIBERATING` alike — the authority agent may issue `FINAL_DECISION` from any point in the working lifecycle, because real deliberations frequently resolve without ever touching implementation or review.
2. `Orchestrator.completeAfterDecision` (see section 8 below) walks the task through `AWAITING_DECISION` explicitly rather than assuming the task is already there, so a decision reached mid-investigation still lands on `COMPLETED` instead of erroring out.

`COMPLETED` itself did **not** change: it remains reachable **only** from `AWAITING_DECISION`. This is structural, not incidental — the authority agent's decision point is mandatory, so the minimum possible path through the lifecycle is three hops: `CREATED -> PLANNING -> AWAITING_DECISION -> COMPLETED`. No task can complete without passing through a state that represents "the authority decided."

Regression coverage: `TestDecisionFromAnyWorkingStateCompletes` and `TestImmediateFinalDecisionCompletesTheTask` (`internal/orchestrator/orchestrator_test.go`) — the latter is exactly the one-round, decide-from-`INVESTIGATING` case that used to crash.

Two things worth noting directly from the code beyond the above: the lifecycle is "deliberately not a linear pipeline: review can send work back to implementation, and deliberation can send work back to investigation" (package comment), and an illegal transition is always a hard error (`domain.ErrInvalidTransition`) — `TaskManager.Transition` never silently no-ops on an illegal request. `TestInvalidTransitionRejected` asserts this directly (a task fresh out of `CREATED` cannot jump straight to `COMPLETED`).

Also note: `Orchestrator.handleOutbound` does not always call `Transition` even when a message implies a new state — if `StateForMessage(m.Type)` maps to a state that `CanTransition` says is illegal from the task's current state, the orchestrator logs it at debug level and **keeps the current state** rather than erroring the whole task out. This is a defensive measure against a message arriving in an order the state machine doesn't expect (e.g. adapter timing), not a normal part of the lifecycle.

## 3. Happy-path ASCII diagram

This is exactly the sequence `go run ./cmd/orch -headless` produces against the mock agents:

```
CREATED
  |  (RunTask starts)
  v
PLANNING  <---------------------------------------------+
  |  TASK_ASSIGNMENT to the AUTHORITY AGENT ONLY         |
  |  DELEGATE (opus -> codex)                             |
  v                                                       |
INVESTIGATING                                             |
  |  ANSWER (codex -> opus)                                |
  v                                                        |
DELIBERATING                                               |
  |  IMPLEMENT (opus -> codex)                              |
  v                                                         |
IMPLEMENTING  <--------------------------+                  |
  |  IMPLEMENTATION_RESULT (codex -> opus)|                  |
  v                                       |                  |
REVIEWING  --- REQUEST_CHANGE (opus)------+                  |
  |                                                          |
  |  FINAL_DECISION (opus -> *)                              |
  v                                                          |
AWAITING_DECISION                                            |
  |  (orchestrator auto-accepts unless requireHumanApproval) |
  v                                                          |
COMPLETED
```

(The dashed loop back into `PLANNING` at the top is not part of the happy path — it illustrates that `PAUSED` can resume into any of these states; see section 5.)

### Shortest real path: a decision reached in one round

The mock happy path above exercises the full pipeline, but it is not the minimum. Since `AWAITING_DECISION` is now reachable from every working state (see the callout in section 2), a task can complete in exactly three hops when the authority agent has enough evidence immediately:

```
CREATED --(RunTask starts)--> PLANNING --(FINAL_DECISION)--> AWAITING_DECISION --> COMPLETED
```

This is not hypothetical: it is exactly what happened in a real verified run against live Claude Code agents (opus as AUTHORITY, a sonnet-backed "scout" as IMPLEMENTER) answering "Can a task ever go straight from CREATED to COMPLETED?" — the authority reached `FINAL_DECISION` after one delegated verification round without the task ever entering `IMPLEMENTING` or `REVIEWING`. See [future-integration.md](future-integration.md#7-a-verified-real-run).

## 4. The two deliberate loops

### `REVIEWING -> IMPLEMENTING -> REVIEWING` (revision loop)

Driven by `REQUEST_CHANGE`, an authority-only message type. `StateForMessage(MsgRequestChange) = TaskImplementing`. In the mock end-to-end flow, this happens exactly once (`AuthorityBehavior` requests one change, then accepts on the second `IMPLEMENTATION_RESULT`), but nothing in the state machine or `AuthorityBehavior`'s design caps how many times this can repeat — `AuthorityBehavior` uses an internal `implResultCount` closure variable purely as its own scripted policy for *this* mock behavior, not a system-enforced limit. A real (or differently-scripted) authority agent could request changes any number of times.

### `DELIBERATING -> INVESTIGATING -> DELIBERATING` (challenge loop)

Driven by `CHALLENGE` (`StateForMessage(MsgChallenge) = TaskDeliberating`, and a subsequent `DELEGATE` back in response, `StateForMessage(MsgDelegate) = TaskInvestigating`). This is the disagreement path: a supporting agent disputes the authority agent's direction, the authority agent (in the mock scaffold) responds by delegating again and asking for evidence rather than overriding the objection, and the two states alternate for as long as the disagreement continues. `TestChallengeLoopHitsRoundLimit` exercises this with `ChallengingImplementerBehavior`, which always challenges a `DELEGATE` — the loop runs until `MaxDeliberationRounds` is hit and the task is `PAUSED`, never `COMPLETED`. See [agent-protocol.md](agent-protocol.md#6-challenge--disagreement-flow--worked-example) for the full worked message sequence.

## 5. PAUSED: how the previous state is remembered and restored

`TaskManager.Transition` records `t.PreviousState = from` **only** when transitioning *into* `PAUSED` (`if to == domain.TaskPaused { t.PreviousState = from }`) — the field is otherwise left untouched, so it always reflects the state the task was in immediately before its most recent pause.

`TaskManager.Resume`:

1. Requires the task to currently be `PAUSED` (else a plain error, not a state-machine violation).
2. Reads `t.PreviousState`. If it is empty or itself terminal (defensive — should not normally happen), it falls back to `TaskDeliberating` rather than resuming into an invalid or already-finished state.
3. Calls `Transition(ctx, id, prev, by, "resumed")` — this goes through the exact same legality check as every other transition (`PAUSED -> prev` must itself be a legal edge, which it always is per the table above, since `PAUSED` can transition to every non-terminal state).
4. Emits `EvtTaskResumed` in addition to the standard `EvtTaskStateChanged`.

Note `TaskManager.Resume` only restores the task's *state field* — it does not resume the `RunTask` deliberation loop by itself. `app.Service.Resume` is the layer that, after calling `orch.Resume`, separately kicks off a fresh `RunTask` goroutine so the task actually keeps deliberating (`TestPauseResumeRestoresPreviousState` in the orchestrator package tests the state-restoration in isolation, without re-running).

Pauses can originate from four places, each with a distinct `PauseReason` constant:

- `PauseReasonHuman = "paused by human"` — `Orchestrator.Pause`, human-initiated, also cancels the task's run context via `cancelRun`.
- `PauseReasonMaxRounds`, `PauseReasonMaxMessages`, `PauseReasonMaxDuration` — loop protection, via `LimitExceeded` inside `Orchestrator.handleOutbound`.
- The literal string `"TIMEOUT: no agent activity"` — the idle timer in `Orchestrator.RunTask`. (Not a named `domain` constant; used verbatim as the reason string.)
- `"all agents stopped"` — every participant's adapter closed its outbound channel; the merged inbox itself closed.

## 6. Which message types drive which transitions (`StateForMessage`)

From `internal/orchestrator/taskmanager.go`:

| Message type | Implied state |
|---|---|
| `TASK_ASSIGNMENT` | `PLANNING` |
| `DELEGATE`, `QUESTION` | `INVESTIGATING` |
| `ANSWER`, `INFORMATION`, `PROPOSAL`, `CHALLENGE` | `DELIBERATING` |
| `IMPLEMENT`, `REQUEST_CHANGE` | `IMPLEMENTING` |
| `IMPLEMENTATION_RESULT`, `TEST_RESULT`, `REVIEW` | `REVIEWING` |
| `FINAL_DECISION` | `AWAITING_DECISION` |
| `APPROVE`, `REJECT`, `STATUS_UPDATE`, `ERROR` | *(no mapping — returns `""`; these types do not by themselves move the task)* |

`Orchestrator.handleOutbound` only calls `Transition` when the mapped state differs from the task's current state **and** `domain.CanTransition` says the edge is legal (see the caveat about silently keeping state, above).

## 7. Multi-round deliberation: no fixed round count

There is no hard-coded number of rounds anywhere in the domain or orchestrator layer. `Task.Round` is a simple counter (`TaskManager.NoteMessage` increments it whenever the message's sender is the task's `AuthorityAgent`, on the reasoning that "a round is one authority-led exchange"), and the *only* thing that ever forces a task to stop deliberating because of round count is the human-configured `TaskLimits.MaxDeliberationRounds` ceiling — checked by `LimitExceeded`, which pauses rather than concludes. `Orchestrator.RunTask`'s own doc comment states the design intent directly: "There is no fixed number of rounds. The authority agent decides when enough evidence exists; the configured limits only ever pause the task and ask for a human, they never declare a winner." The mock `AuthorityBehavior` happens to converge after a specific, small number of exchanges because that is how it was scripted, not because the orchestrator enforces any such count.

## 8. Terminal states

- **`COMPLETED`** — reached only via `AWAITING_DECISION -> COMPLETED`, either automatically (`Orchestrator.handleFinalDecision`, when `requireHumanApproval` is false) or by a human calling `Approve` (`Orchestrator.Approve`). `Task.CompletedAt` is stamped at this transition.

  The automatic path goes through `Orchestrator.completeAfterDecision`, which performs the walk to `COMPLETED` explicitly rather than assuming the task is already sitting in `AWAITING_DECISION`: if the current state is anything else (e.g. `INVESTIGATING`, because the authority decided in the first round), it first transitions `<current> -> AWAITING_DECISION`, then `AWAITING_DECISION -> COMPLETED`. Both hops are checked against `domain.CanTransition` exactly like any other transition — nothing here bypasses the state machine. This two-step walk exists specifically because a one-hop `<working state> -> COMPLETED` transition is illegal by design (`COMPLETED` is only ever a legal successor of `AWAITING_DECISION`, per the table in section 2) — doing the walk in one hop used to fail outright whenever a decision was reached without passing through implementation or review, which stranded an already-decided, already-accepted task with no legal way to reach `COMPLETED`. See the `AWAITING_DECISION`-reachability callout in section 2 for the incident that prompted this.
- **`FAILED`** — reached from most non-terminal states (every row in the table above lists `FAILED` as reachable) whenever `Orchestrator.handleOutbound` sees a runtime error on `ports.Outbound`, or when `startParticipants` cannot start every agent before the run loop begins. `Task.FailureReason` records why (e.g. `"AGENT_ERROR: codex: runtime exited unexpectedly"`).
- **`CANCELLED`** — reached only via a human calling `Cancel` (`Orchestrator.Cancel`), which also cancels the task's run context first so `RunTask` stops promptly.

No terminal state has any outgoing transition; reopening a completed/failed/cancelled task is not supported anywhere in the current code.
