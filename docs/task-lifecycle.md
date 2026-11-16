# Task lifecycle

See also: [../README.md](../README.md), [architecture.md](architecture.md), [agent-protocol.md](agent-protocol.md).

The task state machine lives in `internal/domain/task.go`. `Task.State` is the only answer to "where is this task", and `TaskManager` (`internal/orchestrator/taskmanager.go`) is the only component that changes it. Every change goes through `TaskManager.change`: read the task, apply the change, write it back under a per-task lock, and if the stored row's version moved in between, read it again and redo the change (up to 8 attempts). The run loop and the UI therefore cannot undo each other's writes.

## 1. States

| State | Meaning |
|---|---|
| `CREATED` | Recorded; no run has started yet. |
| `PLANNING` | The run has started; the authority has its opening assignment. |
| `INVESTIGATING` | A `DELEGATE` or `QUESTION` is being worked on. |
| `IMPLEMENTING` | An `IMPLEMENT` or `REQUEST_CHANGE` is being worked on. |
| `REVIEWING` | A result, test result or review came in. |
| `DELIBERATING` | Answers, proposals, information or challenges are being exchanged. |
| `AWAITING_DECISION` | The authority sent a `FINAL_DECISION` that was taken up. The task passes through on its way to `COMPLETED`, or stays here while the decision is held for the human. |
| `PAUSED` | Waiting for the human, with `PauseReason` saying why and `PreviousState` remembered for resuming. |
| `COMPLETED` | Terminal for the agents. The human can reopen it with a follow-up. |
| `FAILED` | Terminal for the agents. Nothing in this version moves a task here; a task an older version failed can be retried by the human. |
| `CANCELLED` | Terminal. The human ended it; it cannot be resumed or reopened. |

`PAUSED` and the terminal states are closed to agents: `AuthorityManager.Authorize` refuses any agent message with `STATE_VIOLATION`. The human and the orchestrator's own notes are not subject to this check.

## 2. Transition table

From the `transitions` map in `internal/domain/task.go`. `CanTransition(from, to)` decides; `from == to` is never a transition, and `TaskManager.Transition` treats it as no change.

| From | Legal to |
|---|---|
| `CREATED` | `PLANNING`, `PAUSED`, `CANCELLED`, `FAILED` |
| `PLANNING` | `INVESTIGATING`, `IMPLEMENTING`, `DELIBERATING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `INVESTIGATING` | `PLANNING`, `IMPLEMENTING`, `DELIBERATING`, `REVIEWING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `IMPLEMENTING` | `REVIEWING`, `INVESTIGATING`, `DELIBERATING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `REVIEWING` | `IMPLEMENTING`, `INVESTIGATING`, `DELIBERATING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `DELIBERATING` | `INVESTIGATING`, `IMPLEMENTING`, `REVIEWING`, `AWAITING_DECISION`, `PAUSED`, `CANCELLED`, `FAILED` |
| `AWAITING_DECISION` | `COMPLETED`, `DELIBERATING`, `IMPLEMENTING`, `INVESTIGATING`, `PAUSED`, `CANCELLED`, `FAILED` |
| `PAUSED` | `CREATED`, `PLANNING`, `INVESTIGATING`, `IMPLEMENTING`, `REVIEWING`, `DELIBERATING`, `AWAITING_DECISION`, `CANCELLED`, `FAILED` |
| `COMPLETED`, `FAILED`, `CANCELLED` | none |

`AWAITING_DECISION` is reachable from every working state, because a question can be settled without implementation or review; `COMPLETED` only from `AWAITING_DECISION`, so every completed task passed the point where the authority decided. `Orchestrator.completeAfterDecision` walks `<state> -> AWAITING_DECISION -> COMPLETED` explicitly (`TestDecisionFromAnyWorkingStateCompletes`, `TestImmediateFinalDecisionCompletesTheTask`). An illegal transition asked of `TaskManager.Transition` is an error (`TestInvalidTransitionRejected`).

Two moves are deliberately not in the table, because nothing an agent or the run loop does should cause them; only the human does, each through its own method:

- **Reopen** (`TaskManager.Reopen`): `COMPLETED -> DELIBERATING` for a follow-up. The accepted decision stays; a new `FINAL_DECISION` supersedes it.
- **Retry** (`TaskManager.Retry`): `FAILED -> DELIBERATING`, with the failure reason cleared (it stays in the events), dropped agents and unreadable-reply counts cleared, and a fresh allowance. The authority is told the task had failed and why.

## 3. What moves a task

`StateForMessage` maps each routed message to the state it implies:

| Message type | Implied state |
|---|---|
| `TASK_ASSIGNMENT` | `PLANNING` |
| `DELEGATE`, `QUESTION` | `INVESTIGATING` |
| `ANSWER`, `INFORMATION`, `PROPOSAL`, `CHALLENGE` | `DELIBERATING` |
| `IMPLEMENT`, `REQUEST_CHANGE` | `IMPLEMENTING` |
| `IMPLEMENTATION_RESULT`, `TEST_RESULT`, `REVIEW` | `REVIEWING` |
| `FINAL_DECISION` | `AWAITING_DECISION` (not when the decision is deferred, section 5) |
| `APPROVE`, `REJECT`, `STATUS_UPDATE`, `ERROR` | none |

`handleOutbound` makes the transition only when it differs from the current state and is legal; an illegal one is logged at debug level and the state kept. The loops are therefore unbounded by design: `REQUEST_CHANGE` sends review back to implementation, and a further `DELEGATE` sends deliberation back to investigation, as often as the authority wants.

The mock flow `-headless` runs is: `CREATED -> PLANNING -> INVESTIGATING -> DELIBERATING -> IMPLEMENTING -> REVIEWING -> IMPLEMENTING -> REVIEWING -> AWAITING_DECISION -> COMPLETED`. The shortest is `CREATED -> PLANNING -> AWAITING_DECISION -> COMPLETED`.

## 4. Runs

A task makes progress only while a **run** (`Orchestrator.RunTask`) holds it. The app starts runs on its own goroutines: creating a task, resuming, retrying, a follow-up, and a rejection or an answer sent while no run is alive. At most `maxConcurrentTasks` (default 1) runs are live at once.

- **Queued.** A run is registered before it waits for a slot, so the UI marks the task `queued`, pause and cancel withdraw the run, and a second start for the same task returns at once. When the slot comes free the task is read again; a run on a task paused or ended meanwhile starts no agent.
- **Kickoff.** The run starts the agents and refills their inboxes from the delivery queue; queued messages are delivered either way. If any of them is from the human, that is what the run opens with. Otherwise it sends the opening assignment if the task has no history, or, after a resume or a retry, tells each agent that owes a reply to send it (the authority to carry on, if nobody owes one).
- **Ending.** When the run ends (a decision, a pause, a cancel, quitting), the agents whose message ended it get a second to finish their turn, every agent is stopped at once (a turn in progress is killed and its messages queued again), and the usage they still report is billed to the task and the day.
- **A run that ends on an error** (storage, routing) pauses the task with the error as the reason. Agents that cannot start (a CLI not installed) pause it too.
- **Quitting** stops every run and waits up to 15 seconds. The tasks keep their state; at the next start every task in a working state is paused as `INTERRUPTED`. `CREATED`, `PAUSED` and `AWAITING_DECISION` tasks are left as they are.

## 5. Decisions

When the authority's `FINAL_DECISION` arrives, in order:

1. **Deferred while work is outstanding.** If another participant is in the middle of a turn or about to retry one, or an `IMPLEMENT` or `REQUEST_CHANGE` to another participant has not been reported on, the decision is not taken up: it is recorded with a mark, delivered to nobody, creates no decision record and does not move the task. The authority is told what is outstanding and to send it again. Dropped agents do not count; the response timeout and the reminder-then-close chase keep this from holding a task for ever. Test: `TestADecisionSentWithAnImplementWaitsForTheReport`.
2. **Recorded.** A `Decision` is proposed (superseding, in one transaction, any earlier accepted or still-proposed one), open disagreements are resolved, and requests still open between agents become a `CAVEAT:` in its evidence. An open `DELEGATE` or `QUESTION` does not hold the decision.
3. **Held for the human** when `requireHumanApproval` is on or an agent is waiting for the human's answer. The task stays `AWAITING_DECISION` and the run ends. The human then approves (the decision is accepted and the task completes; this also works on a task paused while its decision waited), rejects (the decision is marked rejected with the note, the disagreements it resolved reopen, the task returns to `DELIBERATING` with a fresh allowance, and the authority receives the note as a `REJECT`), or answers with an instruction (delivered with a note asking the authority to confirm or revise; its next `FINAL_DECISION` supersedes the held one). A run is started to deliver a rejection or an answer. A resume of a task that only waits on the human starts no run.
4. **Accepted** otherwise. If the human still has a question open, the task is marked `Closing` (stored with the task, so a pause or restart keeps it) and completes once the question is answered, or after a reminder goes unanswered, with that recorded. Otherwise it completes at once.

## 6. Pauses

`TaskManager.Pause` records `PreviousState` and `PauseReason`. Pauses come from:

- the human (`p`, `:pause`), which also cancels the run;
- loop protection (`LimitExceeded`), checked as messages are routed: spending first (`maxTaskCostUSD`, `maxTaskTokens`), then `maxTaskDuration` since the human last spoke, then `maxTotalMessages`, a stall (`stallMessages` messages without progress), and `maxMessages` since the human last spoke. Duration and budget are also checked on replies that are refused or unreadable;
- the day's spending (`maxDailyCostUSD`, `maxDailyTokens`), checked as each turn is billed and before each turn is handed over, failing closed when the record cannot be read;
- a turn that cannot be given even the minimum spending allowance;
- the idle timeout, only when no agent is working and no retry is waiting (the reason names the reply it waits for, if any);
- the human's question still unanswered after a reminder;
- agent failures that give up on the authority, and a usage limit that resets later than `maxRateLimitWait` (the reason says when);
- the authority's replies unreadable twice in a row;
- a run that ends on an error, or agents that cannot start;
- a restart, for tasks that were running (`INTERRUPTED`).

`maxDeliberationRounds` does not pause: every that many authority-led rounds without human input a checkpoint notifies the human and asks the authority for a status update, and work continues.

`TaskManager.Resume` restores `PreviousState` (falling back to `DELIBERATING` if it is empty or terminal) and resets the allowance in the same write. `Orchestrator.Resume` also puts dropped agents back on the task and clears their failure counts, and `app.Service.Resume` starts the run. Limits can be raised before resuming (`:raise`, `TaskManager.RaiseLimits`): the message ceiling, the cost budget and the token budget, upward only.

## 7. Agents leaving a task

A supporting agent whose turns keep failing, whose replies cannot be read, or whose account cannot run its model is **dropped** (`Task.Dropped`, stored with the task): its queued messages are discarded, it receives nothing more, the requests it owed are closed, and the authority is told once to carry on without it. A message later sent to a dropped agent is recorded but not delivered, and its sender is told. The failure policy is in [architecture.md](architecture.md#5-failure-handling). The authority is never dropped; its failures pause the task. Resuming or retrying the task puts every dropped agent back.
