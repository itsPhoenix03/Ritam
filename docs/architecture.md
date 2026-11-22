# Architecture

See also: [../README.md](../README.md), [agent-protocol.md](agent-protocol.md), [task-lifecycle.md](task-lifecycle.md), [future-integration.md](future-integration.md).

## 1. Layering

Dependencies point inward, and the import graph enforces it:

```
      internal/domain            types and rules; imports nothing else in the module
            ^
      internal/ports             interfaces over the domain; no provider SDKs, no implementations
            ^
  +---------+-------------+------------------------+-------------------+
internal/  internal/      internal/orchestrator,   internal/config,
storage    agents/*       knowledge, decisions     logging, layout, backup, fsutil
            ^
      internal/app               wires everything (Build), implements ports.Service
            ^
      internal/tui, cmd/ritam    presentation and entry point
```

- `internal/orchestrator` imports only `domain`, `ports`, `knowledge`, `decisions` and `logging`. It reaches storage through `ports.Storage` and agents through its own `AgentGateway` interface, which `core.Manager` satisfies.
- `internal/tui` talks only to `ports.Service`; every action is `key -> Action -> tea.Cmd -> ports.Service call -> result message -> Model`, and live updates come from `Service.Subscribe`.
- `internal/app/wire.go`'s `Build` is the one place that knows which adapter backs which agent and which storage backend is used.
- Provider details (the `@@ORCH` / `##CODEX` headers' markers, CLI flags, stream formats) stay inside `internal/agents/claude` and `internal/agents/codex`.

## 2. Components

| Component | Where | Responsibility |
|---|---|---|
| domain | `internal/domain` | Task, Message, Decision, Agent, Event, Delivery, Turn, the transition table, the request ledger (`BuildLedger`), failure kinds (`AgentFailure`). |
| `EventBus` | `orchestrator/events.go` | Appends every event to the event repository and notifies subscribers; `PublishEphemeral` notifies without storing (live agent output). |
| `AuthorityManager` | `orchestrator/authority.go` | Validates every message: type, state, sender, authority, verdict, routing, and an `IMPLEMENT` to a read-only agent. Returns a typed `*Violation`. |
| `TaskManager` | `orchestrator/taskmanager.go` | The only writer of tasks: every change is read-apply-write under a per-task lock, redone on a version conflict. Transitions, counters, limits, `Reopen`, `Retry`, `RaiseLimits`, dropped agents. |
| `MessageBus` | `orchestrator/bus.go` | Validates, records (the message and its queued deliveries in one transaction), emits events, and hands each recipient its own enriched copy. |
| inboxes | `orchestrator/inbox.go` | Per-agent queues for a running task: combined turns, holds while an agent waits on a co-worker (at most 4 minutes), the team board, turn records, the durable delivery queue. |
| failure policy | `orchestrator/failures.go` | Acts on each failed turn by its kind: retry, wait, drop a supporting agent, pause for the authority (section 5). |
| turn budgets | `orchestrator/turnbudget.go` | Each turn's spending allowance, reserved until the turn is billed, fails or is killed (section 6). |
| roster | `orchestrator/roster.go` | The team roster in every delivery (provider, model, role, file and build rights, state, spend), with rights read through `RightsReporter`. |
| `Orchestrator` | `orchestrator/orchestrator.go` | The run loop (`RunTask`), the human's actions, decisions and their gates, reminders, checkpoints, knowledge recording, disagreements. |
| `core.Manager` / `Registry` | `agents/core` | Agents and their per-task instances; starts and stops adapters, reports agent state and rights. Validates nothing. |
| `core.OneShotRunner` | `agents/core/oneshot.go` | Runs one CLI process per turn: resolves the executable on `PATH`, arguments as a slice, prompt on stdin, streams output, kills the process tree on cancel or timeout, then runs the file guard. |
| `core.InvocationQueue` | `agents/core/queue.go` | Runs an agent's turns one at a time; the agent is `WORKING` while anything is running or queued. |
| `core.SessionRegistry` | `agents/core/sessions.go` | Which CLI session belongs to which (task, agent), kept in `.ritam/sessions.json`. |
| reply parser, prompts | `agents/core/reply.go`, `prompt.go` | One header parser and one prompt layout for both CLIs. |
| `core.Access`, `core.FileGuard` | `agents/core/access.go`, `guard.go` | What a write-enabled agent may run at the access level; ritam's own and the workspace's protected files put back after each turn. |
| adapters | `agents/claude`, `agents/codex` | Drive the real CLIs (see [future-integration.md](future-integration.md)). |
| `review.Server`, `ritam hook` | `internal/review`, `cmd/ritam` | The tool hook: each CLI runs `ritam hook pretool` before a tool call, which asks the review server (127.0.0.1, a token per turn). With real agents its `Policy` decides each call (refuse, refuse and pause the task, ask the human through the app's approvals queue, pass) by the rules and the review mode; calls are recorded as `ToolCall` or `ToolBlocked` events, questions as `ToolApproval`, and each turn's calls are compared with what its CLI reported (`HookMissed`). |
| `facts`, `rules` | `internal/review/facts`, `internal/review/rules` | What a tool call would do, without running it: bash (mvdan.cc/sh), PowerShell and cmd parsed, wrappers unwrapped, paths placed in zones, URLs and written text found. The embedded `harmful-commands.yaml`, loaded strictly, matched against those facts and the turn's history; every rule's examples run as tests. |
| `mockagent`, `fake` | `agents/mock`, `agents/fake` | Scripted in-process agents (the default, tests, `-headless`); a scripted agent that fails like the real CLIs, for tests. |
| `knowledge.Store`, `decisions.Manager` | `internal/knowledge`, `internal/decisions` | Shared knowledge with substring search; decisions (`Propose`, `Accept`, `Reject`, `Supersede` in one transaction). |
| storage | `internal/storage` | SQLite (default) and in-memory implementations of every repository, held to the same conformance tests. |
| `app.Service`, `app.Build` | `internal/app` | `ports.Service`; tracked background runs; the team (`team.json`), model availability, the daily spend ledger, the sandbox canary check, access control. |
| config, layout, backup | `internal/config`, `layout`, `backup` | Settings (strict decoding, validation, project restrictions, `Explain` for `config check`); the app home, projects and their registry; snapshots, restore, archives. |

## 3. Storage

`storage.OpenSQLite` (pure-Go driver, one file per project, `.ritam/ritam.db`) is the default; `storage.backend: memory` uses `storage.NewInMemory()`. Messages and events have real columns; tasks, decisions, disagreements, knowledge and artifacts are JSON documents beside their lookup columns. Schema migrations are applied in order and counted in `PRAGMA user_version`: 1 the base tables, 2 task versions, 3 the `deliveries` table, 4 the `turns` table. A newer database than the build understands is refused. Before an older database is upgraded, `app.OpenDatabase` (used by the TUI, `-headless` and every data command) takes a backup when backups are on.

- **Transactions.** `ports.Storage.WithTx` runs a function against repositories that all act in one transaction, committed if it returns nil and rolled back if it fails or panics. SQLite uses one `sql.Tx` on its single connection, nesting with savepoints; the in-memory store holds a store-wide gate and restores a snapshot on failure. Used for a message with its deliveries and for superseding a decision.
- **Versioned tasks.** Each task row has a version (`Task.Version`, not part of the stored document). An update from a copy read at an older version returns `ports.ErrConflict`; `TaskManager.change` reads again and redoes the change.
- **Deliveries.** A message for an agent that will be handed it is queued (`DeliveryQueued`) in the same transaction as the message, marked sent with the turn that carries it, acknowledged when the turn ends well, and queued again when it is killed or fails. A run starts by refilling the inboxes from the queue. A finished task's waiting rows are cleared.
- **Turns.** Every turn is recorded: task, agent, the message ids it carried, start and end, outcome (`completed`, `failed` with its kind, `killed`) and the usage billed, including usage that arrives after the run ended. `Service.TaskTurns` reads them.
- **One ritam per database.** An OS lock file next to the database; a second instance refuses to start, naming the pid.
- **Artifacts** have a repository and a table, but nothing writes them yet.

Files outside the database: `sessions.json` and `team.json` in the project's hidden folder; `projects.json`, `models.json` and `spend.json` in the app home. Writes go through a temp file and a rename (flushed to disk for `sessions.json` and `spend.json`); an unreadable `sessions.json` or `spend.json` is kept aside as `<name>.bad-<time>`.

## 4. Concurrency

- **Runs.** `app.Service` starts every run (`StartTask`, `Resume`, `Retry`, `FollowUp`, and `Reject` or `SendInstruction` when no run is alive) on its own goroutine under one cancellable context, and counts them. `Service.Shutdown` cancels them all and waits (`cmd/ritam` gives it 15 seconds, printing `stopping agents...`); no run starts after it. A run's result reaches the UI as events, so creating a task returns at once.
- **Slots.** `RunTask` registers itself before waiting for one of `maxConcurrentTasks` slots (events `RunQueued`, `RunStarted`, `RunUnqueued`). A second run for the same task returns at once; a pause or cancel cancels a queued run; once it has a slot it reads the task again and starts nothing on a paused or finished task.
- **The run loop** is one goroutine per task, selecting on: the merged outbound channel of its participants, a liveness signal from their streamed output, a 250 ms dispatch tick (hand inboxes to agents that became free), a 2 s settle tick (complete a closing task or an answered follow-up, chase unanswered requests), the idle timer and its context.
- **Adapters** return from `SendMessage` at once: the turn is queued on the agent's invocation queue, so the run loop never blocks on a CLI. Each agent runs one turn at a time per task instance.
- **Messages from the human** sent while no run is live are queued in the store; a run going live waits for any such write in progress before reading the queue, so none falls between.
- **Only the orchestrator reads an adapter's outbound channel**; `mergeOutbound` is its single consumer, and it outlives the loop long enough to bill the last turns.

## 5. Failure handling

Adapters report a failed turn as a `domain.AgentFailure` on `Outbound.Err`: its kind, whether it is retryable, a reset time for rate limits, a detail, and for a malformed reply the raw text. Claude's come from its result event, Codex's from its `error` / `turn.failed` events, with stderr as the last resort. The orchestrator switches on the kind and never reads the error's text (an error with no kind is taken as a crash):

| Kind | Policy |
|---|---|
| `malformed` | told what it sent and the exact header; asked again; after 2 in a row, give up |
| `timeout` | sent again at once, told it was cut off; a second: give up |
| `overloaded`, `crash`, `session-missing` | sent again after 10 s, 30 s, 90 s; then give up |
| `limit` | reset within `maxRateLimitWait` (2 min): wait and send again, up to 3 times; a later reset: pause, saying when; no reset given: pause for the authority, drop a supporting agent |
| `auth`, `credits`, `plan`, `model` | drop a supporting agent, pause for the authority; `credits`, `plan` and `model` mark the model unavailable |
| `budget` | the CLI stopped the turn at its cap: pause for the budget |
| `cancelled` | nothing: the turn was stopped on purpose |

Giving up drops a supporting agent (see [task-lifecycle.md](task-lifecycle.md#7-agents-leaving-a-task)) and pauses the task for the authority. A retry re-sends the agent's last batch with a note in front, after asking for its own spending allowance; it is not routed again. A readable message from the agent clears its failure counts. No failure moves a task to `FAILED`.

There is no process to restart between turns: each turn is a new CLI process, and the retry is the recovery. `core.Manager.Restart` exists but nothing calls it.

**Idle.** The idle timer (`idleTimeout`, 60 s, raised to 10 minutes when real agents are used) is reset by routed messages and streamed output, not by errors or usage reports. When it fires while an agent is working or a retry is waiting, it is reset; a working agent is bounded by its turn's time limit (`responseTimeout` 5 min read-only, `writeResponseTimeout` 20 min write-enabled). Otherwise the task pauses, naming the reply it is waiting for if there is one.

## 6. Spending

Usage is reported after a turn's messages. It is added to the task (overall and per agent) and to the day's ledger, and recorded on the turn. When a run ends, the agents whose message ended it get a second to finish, all agents are stopped in parallel, and what they still report until they have stopped is billed, so the deciding turn and every killed turn are paid for.

**Per-turn allowance.** When a task or the day has a cost limit, each turn's allowance is decided when it is handed over: what is left of the task's budget and of the day's, after what other turns in progress hold, shared among the agents that could start at the same time, at least $0.05. It is held until the turn is billed, fails or is killed. Below the minimum, the turn waits for co-workers' reservations, or the task pauses for its budget or the day's. A retried turn asks for its own allowance. Codex turns get no allowance (Codex reports no cost). The Claude adapter passes the allowance as `--max-budget-usd`; whether the CLI counts only that invocation on a resumed session is unverified (see the comment above `budgetArgs`).

**The day's ledger** (`app/spend.go`, `spend.json` in the app home) totals spending per local calendar day over every task and project. Each addition runs under an OS file lock with a bounded wait and writes through a unique, flushed temp file, so several ritam instances lose nothing. A record that cannot be read or written fails closed: the task pauses with the error, and a corrupt file is kept aside. `Service.SpendToday` feeds the header.

## 7. Live agent activity

Claude streams `stream-json` with partial messages (token deltas, thinking, tool starts); Codex streams `--json` item events. `core.ActivityEmitter` coalesces them (about 80 ms) and drops rather than blocks when its buffer is full. `Orchestrator.streamActivity` forwards them with `EventBus.PublishEphemeral`: never stored, never routed, never acted on, except as a sign of life for the idle timer. The reply is still read from the final result (Claude) or the `--output-last-message` file (Codex).

## 8. Security model

- **Agent output is untrusted.** The reply parser reads only known header keys; an unknown type becomes `INFORMATION`, never something stronger; text is sanitised before it is shown or fed to another agent, and lines that imitate the orchestrator's framing or the human's mark are quoted.
- **One validation path.** Every message, from any adapter, goes through `MessageBus.Route` and `AuthorityManager.Authorize` before it is recorded or delivered. The transport identity replaces any claimed sender.
- **Agents cannot reach each other's processes.** Each turn is its own process started by its own adapter; the orchestrator writes the prompt and reads the reply.
- **Agents cannot change ritam's configuration through ritam.** No adapter or runtime method accepts configuration. What they can reach through the file system is what the protected-file rules and the file guard cover: Claude's file tools are denied ritam's files and the files that run code later; after every turn the guard puts back any of them that changed (the app home's shared files are re-read when agent turns go from none to one, so another instance's writes are kept). The database is not guarded.
- **Every tool call goes through ritam's hook first.** The CLIs run `ritam hook pretool` by its absolute path before any tool; it reaches the review server only with the running turn's token, and a hook that cannot check a call blocks it (exit 2; for Claude, `onFailure: block` also covers a hook that cannot start or times out). The review then decides the call by ritam's rules and the review mode (auto: the rules decide and suspicious calls are put to the human; manual: every command is too), and a passed call still meets the CLI's own permission rules. A critical call pauses its task. Rules cannot foresee every destructive command, so the sandbox, the access level and the file guard stay in place behind it. A turn whose reported calls did not all reach the hook is flagged.
- **Arguments are a slice, the prompt is stdin.** Nothing is assembled into a shell string; configured `args` pass only an allowlist of flags, and ritam's own flags come last.
- **Read-only unless granted.** `AgentConfig.AllowWrites` is false unless set; recruits from the new-task form and providers that do not set `allowWrites` do set it, within the workspace. See the README's safety section for what protects a write-enabled agent and what does not.
- **No credentials.** No settings field carries an API key; the CLIs use their own logins.
- **Project settings are not trusted** like the global ones: they cannot pick executables or arguments, grant write access, switch the mock agents off, add commands, point outside the project, loosen limits, or change backups or the proxy (`config.RestrictProject`).

## 9. Adding an agent

Recruiting is configuration, not code: every agent of a provider (`claude-code`, `codex`) is built the same way by `app.Team`, and nothing in the orchestrator assumes a number of agents or their names. A new provider needs an adapter implementing `ports.AgentAdapter` (reporting failures as `domain.AgentFailure`, usage after messages, and accepting batches if it can), and a case in `app.buildAdapter`.
