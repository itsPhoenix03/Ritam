# Architecture

See also: [../README.md](../README.md), [agent-protocol.md](agent-protocol.md), [task-lifecycle.md](task-lifecycle.md), [future-integration.md](future-integration.md).

## 1. Layering rules

The codebase is layered strictly inward-to-outward, and the layering is enforced by import direction, not just convention:

```
      internal/domain            (innermost: no imports of anything below)
            ^
            |
      internal/ports              declares interfaces over domain; no provider
            ^                     SDKs, no concrete implementations
            |
  +---------+----------+-----------------+------------------+
  |         |          |                 |                  |
internal/  internal/  internal/         internal/          internal/
storage    agents/*   knowledge,        proxy               config,
                       decisions,                            logging
                       orchestrator
            ^
            |
      internal/app                 wires everything (Build), exposes ports.Service
            ^
            |
      internal/tui                 presentation only
            ^
            |
      cmd/ritam                     entry point
```

Rules, taken directly from the package doc comments:

- `internal/domain` (`domain.go` package comment on `ids.go`): "Nothing in this package may import provider SDKs, the TUI, storage engines or any other outer layer." It has no dependency on anything else in the module.
- `internal/ports`: "Everything here is provider-neutral on purpose. No Anthropic- or OpenAI-specific type may appear in this package, and the domain must never import a concrete implementation of any of these interfaces." Ports depends only on `domain` and the standard library.
- `internal/orchestrator`: depends on `domain`, `ports`, `knowledge`, `decisions`, `logging` — never on a concrete storage or agent-adapter package. It reaches storage and agents exclusively through `ports.Storage` and the `AgentGateway` interface it declares itself (`internal/orchestrator/bus.go`), which `internal/agents/core.Manager` happens to satisfy.
- `internal/tui`: "The package is a pure presentation layer: it renders state obtained from `ports.Service` and turns key presses into calls on that same interface. It must never import the domain's mutating internals directly, never talk HTTP, and never touch storage." Every state-changing action goes `keypress -> Action -> tea.Cmd -> ports.Service method -> result tea.Msg -> Model`.
- `internal/app`: "the only thing the TUI talks to, keeping the call chain TUI -> application service -> domain instead of routing the user interface through HTTP or through the domain directly." `internal/app/wire.go`'s `Build` is the **only** place that knows which concrete adapter (mock/claude-code/codex) or storage backend (in-memory today) is wired to which agent — "a change here and nowhere else" is how the package doc puts it.

Provider details (the `@@ORCH`/`##CODEX` wire formats, `exec.LookPath`-resolved executables, Anthropic/OpenAI-specific environment variables) stay at the very edge: inside `internal/agents/claude`, `internal/agents/codex` and `internal/proxy`. Nothing above those packages — not the orchestrator, not the domain, not the TUI — knows those formats exist.

## 2. Components and responsibilities

| Component | File(s) | Responsibility |
|---|---|---|
| `domain` | `internal/domain/*.go` | Task/Message/Decision/Knowledge/Artifact/Event/Agent types and the task state-transition table. No behavior beyond validation helpers (`CanTransition`, `RequiresAuthority`, `CanChallenge`, ...). |
| `ports` | `internal/ports/ports.go` | Every interface boundary: repositories, `AgentRuntime`, `AgentAdapter`, `LLMProxy`, the application `Service`. |
| `EventBus` | `internal/orchestrator/events.go` | Fans out `domain.Event`s to subscribers (synchronously — handlers must not block) and appends every event to `ports.EventRepository`. The single place "something happened" becomes both a log line and a UI update. |
| `AuthorityManager` | `internal/orchestrator/authority.go` | Validates a message's sender identity, its recipient routing, and whether its type requires the sending agent to hold task authority. Returns a typed `*Violation` rather than a generic error. |
| `TaskManager` | `internal/orchestrator/taskmanager.go` | The only component permitted to change a task's state; validates every transition against `domain.CanTransition`, tracks round/message counters, applies loop-protection limits, restores pre-pause state on resume. |
| `MessageBus` | `internal/orchestrator/bus.go` | Validates (via `AuthorityManager`), records (via `ports.MessageRepository`), and delivers (via `AgentGateway`) every message. Broadcast (`*`) expansion into per-participant sends happens here. Also owns the `Enrich` hook (wired by `orchestrator.New` to `Orchestrator.attachKnowledge`), called inside `deliver` on a per-recipient clone of the message *after* it has already been recorded — this is what attaches relevant shared knowledge to a real, one-shot agent's prompt without altering the message log. See [agent-protocol.md](agent-protocol.md#8-knowledge-injection-into-delivered-messages). |
| `Orchestrator` | `internal/orchestrator/orchestrator.go` | Wires the above together, owns the `RunTask` event loop, human override methods (`Pause`/`Resume`/`Cancel`/`Approve`/`Reject`/`SendInstruction`), decision creation on `FINAL_DECISION` (`completeAfterDecision` walks a task to `COMPLETED` via `AWAITING_DECISION`, tolerating a decision reached from any working state — see [task-lifecycle.md](task-lifecycle.md#8-terminal-states)), and knowledge recording (`attachKnowledge`, `recordKnowledge`). |
| `core.Registry` / `core.Manager` | `internal/agents/core/{registry,manager}.go` | Registry: goroutine-safe id -> (domain.Agent, adapter) map. Manager: starts/stops/restarts adapters, routes `Send`/`SendTask` to the right one, and (via `ReportOutbound`) turns adapter crash signals into agent-state changes. Manager itself performs no protocol validation — that is the orchestrator's job. |
| `core.OneShotRunner` | `internal/agents/core/oneshot.go` | The real transport both the Claude and Codex adapters drive: resolves the configured executable on `PATH` via `exec.LookPath` (`Resolve`), then for each protocol message runs it exactly once (`Run`) with `exec.CommandContext`, waits for exit, and captures stdout/stderr separately with a bounded timeout (`ErrTimeout` on expiry). Arguments are always passed as a `[]string`, never assembled into a shell string — see the security model below. Knows nothing about either CLI's prompt or reply format; that lives one layer up, in each adapter package. |
| `claude.ClaudeCodeAdapter` / `codex.CodexAdapter` | `internal/agents/claude`, `internal/agents/codex` | Drive the real `claude`/`codex` CLIs: render `domain.Message` into a CLI prompt (`renderPrompt`), invoke the CLI once per message via `core.OneShotRunner`, and parse the reply back into `domain.Message`s using each package's own reply-header format (`@@ORCH` for Claude, `##CODEX` for Codex). Claude's replies are unwrapped from a `--output-format json` envelope first; Codex's final reply is read from the file named by `--output-last-message`, never scraped from stdout. See [future-integration.md](future-integration.md) for the full invocation and [agent-protocol.md](agent-protocol.md#7-wire-formats-live-conventions--driving-the-real-clis) for the wire formats. |
| `mockagent.Adapter` | `internal/agents/mock/mock.go` | In-process `ports.AgentAdapter` driven by a pure-Go `Behavior` function instead of any process. Used by every orchestrator test, `-headless`, `-demo`, and by default in the TUI (`tui.useMockAgents: true`). |
| `knowledge.Store` | `internal/knowledge/store.go` | Validates and stores `domain.KnowledgeEntry` records, offers `RecordInvestigation`/`RecordDecision`/`RecordTestResult`/`RecordAgentNote` convenience wrappers, and `ContextFor` — merged task-scoped + project-wide search, deduplicated and ranked. |
| `decisions.Manager` | `internal/decisions/manager.go` | Decision lifecycle: `Propose` (assigns sequential `D-00N` id), `Accept`, `Reject`, `Supersede` (creates a new decision, marks the old one `SUPERSEDED`, never deletes). |
| `DisagreementRepository` | `internal/ports/ports.go` (interface), `internal/storage/memory_disagreement.go` (`MemDisagreementRepository`) | Persists `domain.Disagreement` records raised by `Orchestrator.recordDisagreement` on `CHALLENGE` and closed out by `resolveDisagreements` on `FINAL_DECISION`. `Create`/`Update`/`Get`/`ListByTask`/`ListOpen`; exposed at the application layer via `app.Service.TaskDisagreements`. See [agent-protocol.md](agent-protocol.md#6-challenge--disagreement-flow--worked-example). |
| `storage` (in-memory) | `internal/storage/memory*.go` | Goroutine-safe map-backed implementations of every `ports.*Repository`, including `MemDisagreementRepository`. `NewInMemory()` is the only implementation wired up today. `sqlite.go` documents (but does not implement) the intended schema. |
| `proxy.Proxy` | `internal/proxy/proxy.go` | Implements `ports.LLMProxy`. `Start` is a no-op when disabled and returns `ports.ErrNotImplemented` when enabled — the proxy is not wired into the orchestrator's request path at all in this phase. |
| `app.Service` / `app.Build` | `internal/app/{service,wire}.go` | `Service` implements `ports.Service` on top of `*orchestrator.Orchestrator` and holds no state of its own. `Build` is the composition root: reads `config.Config`, registers agents (mock unless real adapters are explicitly enabled), constructs storage, knowledge, decisions and the orchestrator. |
| `tui` | `internal/tui/*.go` | Bubble Tea model/update/view/commands. Talks only to `ports.Service`; subscribes to `Service.Subscribe` so the UI updates from live events instead of polling. |
| `config` | `internal/config/config.go` | Loads/validates `ritam.yaml`, falling back to `Default()` when absent. Deliberately does not check that agent executables exist, since the scaffold must start without Claude Code or Codex installed. |
| `logging` | `internal/logging/logging.go` | `slog`-based structured logger plus `Redact`, which strips credential-shaped substrings (API keys, bearer tokens, cookies) from any string before it is logged, since agent output is untrusted and may contain anything. |

## 3. Ports/adapters boundary

`ports.AgentRuntime` is the low-level boundary: "owns an agent's underlying process (or in-process simulation). It knows nothing about tasks, authority or the message protocol." `core.ProcessRuntime` is the only current implementation (real `os/exec` process), used by both the Claude and Codex adapters.

`ports.AgentAdapter` is the protocol-level boundary: "normalises one concrete agent CLI into the internal protocol." Three implementations exist: `mockagent.Adapter` (pure Go, no process), `claude.ClaudeCodeAdapter` (wraps `core.OneShotRunner` + `@@ORCH` parsing, drives the real `claude` CLI), `codex.CodexAdapter` (wraps `core.OneShotRunner` + `##CODEX` parsing, drives the real `codex` CLI). The orchestrator, the `MessageBus`, and `core.Manager` never see anything below `ports.AgentAdapter` — swapping mock for real Claude Code for a given agent id is a one-line change in `app.buildAdapter`, gated at the top level by `cfg.TUI.UseMockAgents` (flipped off by `-real`, see [future-integration.md](future-integration.md#6-running-against-the-real-clis)).

`ports.Storage` bundles seven repository interfaces (`TaskRepository`, `MessageRepository`, `EventRepository`, `DecisionRepository`, `KnowledgeRepository`, `ArtifactRepository`, `DisagreementRepository`). `storage.NewInMemory()` is the only constructor; a SQLite-backed one would implement the same seven interfaces and be substituted in `app.Build`, "a change confined entirely to this package's constructor — no other package needs to change" (package doc, `internal/storage/doc.go`).

`ports.LLMProxy` is the boundary for the not-yet-implemented local proxy. The orchestrator has zero references to it; only `cmd/ritam/main.go` calls `sys.Proxy.Start`/`.Enabled()`, and only `app.Build` constructs it.

`ports.Service` is the boundary the TUI depends on; `app.Service` is the only implementation.

## 4. Security model

**Agent output is untrusted.** This assumption runs through every layer that touches it:

- `ports.Outbound`'s doc comment: "Treat every field as untrusted input: the orchestrator re-validates the sender, the recipient, the message type and the agent's authority before any state changes."
- `ports.RuntimeOutput`'s doc comment: "Contents are untrusted."
- The Claude/Codex adapters' `parseOutput` functions reject unknown message types and missing recipients rather than passing them through; `translate()` logs the raw text through `logging.Redact` before it ever reaches a log file.
- `Orchestrator.handleOutbound` never trusts an adapter's claimed `Message.FromAgent`: if it disagrees with the adapter's own verified `out.AgentID`, the message is rejected as an `IDENTITY_VIOLATION` and the transport identity wins, never the claimed one.

**The validation -> permission check -> action -> event -> state update pipeline.** `MessageBus.Route`'s doc comment states this explicitly: "The pipeline is deliberately explicit: agent -> message -> validation -> permission check -> action -> event -> state update. A rejected message is recorded as a violation event and is never delivered." Concretely, for every outbound message:

1. **Validation**: `AuthorityManager.Authorize` checks the message type is known (`domain.IsKnownMessageType`), that the task is open to the sender — a terminal (`COMPLETED`/`FAILED`/`CANCELLED`) or `PAUSED` task rejects every agent-originated message with `STATE_VIOLATION` ("task is `<STATE>`; only the human can act on it"), while `domain.HumanAgentID` is exempt from this check and can always act — the routing target is legal (`checkRouting`), and — if the human isn't the sender — that the sender is a task participant.
2. **Permission check**: `MessageType.RequiresAuthority()` combined with `AuthorityManager.IsAuthority` — only the task's `AuthorityAgent` may send an authority-only type.
3. **Action**: on success, `messages.Append` records the message; on failure, `Violation` is turned into an `AuthorityViolation` or `ValidationRejected` event and the message is never delivered (`Route` returns early).
4. **Event**: `events.emit(EvtAgentMessageSent, ...)`, plus type-specific events (`EvtAgentDelegatedTask`, `EvtAgentChallenged`).
5. **State update**: `deliver()` hands the message to the recipient's adapter; back in `Orchestrator.handleOutbound`, `NoteMessage` and `StateForMessage`-driven `Transition` update the task record.

**Agents cannot modify configuration.** No `ports.AgentAdapter` method or `ports.AgentRuntime` method accepts configuration; `config.Config` is loaded once at process start (`cmd/ritam/main.go`) and handed to `app.Build`, which is not reachable from any agent-facing interface.

**Agents cannot invoke each other's OS processes.** Every agent is a distinct `core.ProcessRuntime` (or, for tests, a distinct in-process `mockagent.Adapter`) with its own stdin/stdout. The only component that ever reads one agent's `Outbound()`/`Output()` channel or writes to another's `Send`/`SendMessage` is the orchestrator (see the concurrency section below on why `mergeOutbound` is the sole consumer). There is no code path by which one adapter can reach another adapter's process handle.

**Agents cannot bypass authority.** Every message, regardless of which adapter or CLI produced it, passes through the same `MessageBus.Route` -> `AuthorityManager.Authorize` call before it is recorded or delivered. There is no alternate delivery path (e.g. a direct write to another adapter, or a message-type-specific shortcut) anywhere in `internal/orchestrator`.

**Agents cannot act on a stopped or paused task.** `AuthorityManager.Authorize` rejects any agent-originated message (`Kind: STATE_VIOLATION`) once a task is terminal or `PAUSED` — a crashed loop, a rogue adapter, or a stale in-flight goroutine cannot push a task further once a human has stopped it. Only `domain.HumanAgentID` is exempt from this check, so the human retains the ability to `Resume`, `Cancel`, or otherwise intervene on a task no agent can touch. See `TestAgentsCannotActOnAStoppedTask` (`internal/orchestrator/orchestrator_test.go`).

**Real-CLI-specific security notes (`core.OneShotRunner`, `internal/agents/claude`, `internal/agents/codex`):**

- **Process arguments are always a slice, never a shell string.** `OneShotRunner.Run` builds `exec.CommandContext(ctx, path, args...)` from a `[]string` assembled by each adapter's `buildArgs`; nothing concatenates prompt text (which is agent/model-generated and therefore untrusted, same as any other agent output) into a string that ever reaches a shell. This holds for both `claude`/`codex` — including the prompt text itself, which for Codex is deliberately the last positional argument so it can never be parsed as a flag no matter what it contains.
- **Read-only by default.** `AgentConfig.AllowWrites` defaults to `false` (the zero value, chosen deliberately so an omitted config field is the safe one). Claude Code is launched with an explicit tool allowlist (`Read`,`Grep`,`Glob`) *and* an explicit denylist (`Bash`,`Edit`,`Write`,`MultiEdit`,`NotebookEdit`) plus `--permission-mode dontAsk`; Codex is launched with `--sandbox read-only`. Both close the same gap: in headless mode, a tool that is merely absent from an allowlist can still surface as an interactive approval prompt with nobody there to answer it, so the deny/sandbox side is explicit rather than assumed.
- **No API keys anywhere in this codebase.** `config.AgentConfig` has no credential field; `core.OneShotRunner.Env` exists as a generic mechanism for extra environment variables but nothing in `claude.New`/`codex.New` ever populates it with anything credential-shaped. Both CLIs authenticate out of band via their own login flow (`claude /login`, `codex login`) before the orchestrator ever launches them as a subprocess.
- **One CLI process per protocol message, by design, for a security-adjacent reason as much as a reliability one.** A long-lived interactive session can wedge indefinitely on a permission prompt nobody answers, and a wedged session is indistinguishable from a slow one without a timeout that kills it anyway. A process that must exit before its reply counts gives a hard timeout (`OneShotRunner.Timeout`), a clean retry boundary, and a crash that costs one message rather than the whole task. See [future-integration.md](future-integration.md#1-the-transport-one-cli-process-per-protocol-message).

## 5. Failure handling

Agent lifecycle failure states are explicit in `domain.AgentState`: `STOPPED`, `STARTING`, `IDLE`, `WORKING`, `AGENT_UNAVAILABLE`, `AGENT_ERROR` (deliberately distinct from task state). How they arise:

- **`AGENT_UNAVAILABLE`**: `core.ProcessRuntime.Start` sets this when `exec.LookPath` cannot resolve the configured executable — the CLI isn't installed or isn't on `PATH`. `core.Manager.ReportOutbound` also promotes an agent to `AGENT_UNAVAILABLE` (rather than `AGENT_ERROR`) if the adapter's own `Status()` has already settled there.
- **`AGENT_ERROR`**: set by `core.Manager.Start`/`Stop` on adapter-level errors, by `core.ProcessRuntime`'s wait goroutine when the child process exits with a non-nil error, and by `core.Manager.ReportOutbound` whenever an `Outbound` carries a non-nil `Err` (e.g. a crashed mock agent via `mockagent.NewCrashing`, or a translate-loop error in the claude/codex adapters).
- **`TIMEOUT`** (not an agent state, a task-pause reason): `Orchestrator.RunTask`'s idle timer (`idleTimeout`, default 60s, configurable via `orchestrator.idleTimeout`) fires when no agent has produced any outbound message for that long. The task is paused with reason `"TIMEOUT: no agent activity"` — this never fails the task outright, it hands control back to a human. `TestIdleAgentPausesForHuman` exercises this directly.
- **`TASK_FAILED`** (`domain.TaskFailed`, terminal): set by `Orchestrator.handleOutbound` when an `Outbound` carries a runtime error (`out.Err != nil`) — the failure reason is recorded verbatim as `"AGENT_ERROR: <agent>: <err>"`. `TestAgentFailureFailsTaskExplicitly` exercises this. A task can also fail if `startParticipants` cannot start every participant's adapter before the run loop begins.

Note the current gap, called out as a `TODO(phase-next)` directly above `Orchestrator.handleOutbound`'s error branch: there is no retry/backoff/recovery policy yet — one agent's runtime error fails the whole task immediately, rather than attempting a bounded restart first. `core.Manager.Restart` exists for a human (or future policy) to call manually, but nothing calls it automatically today.

## 6. Loop protection

Three independent limits, all configured on `domain.TaskLimits` and checked by `orchestrator.LimitExceeded` after every processed message:

- `MaxDeliberationRounds` — a round increments (`TaskManager.NoteMessage`) each time the **authority agent** sends a message, so "a round" means one authority-led exchange, not any message.
- `MaxMessages` — a hard cap on the total number of routed messages for the task.
- `MaxDuration` — wall-clock time since `Task.CreatedAt`.

Whichever limit is hit first, `Orchestrator.handleOutbound` pauses the task (`domain.PauseReasonMaxRounds`/`MaxMessages`/`MaxDuration`) and emits `EvtLimitReached`. Critically, **reaching a limit never selects a winner or forces a decision** — `LimitExceeded`'s doc comment is explicit about this, and `TestChallengeLoopHitsRoundLimit` asserts the task ends up `PAUSED`, not `COMPLETED`, when the round limit trips mid-challenge-loop.

## 7. Concurrency model

**The run loop.** `Orchestrator.RunTask` is a single-goroutine `select` loop per task: it reads from one merged inbound channel, an idle timer, and the run's own cancellable context. Concurrent task execution across *different* tasks is bounded by a semaphore (`o.sem`, sized by `Deps.MaxConcurrentTasks`, default 1) so the orchestrator does not need to reason about more than `MaxConcurrentTasks` `RunTask` invocations touching shared storage at once (the repository implementations are themselves goroutine-safe regardless).

**The merged outbound channel.** `Orchestrator.mergeOutbound` fans every participant's `AgentAdapter.Outbound()` channel into one buffered channel (`merged`, capacity 64) via one goroutine per participant, closing `merged` only once every participant's channel has closed (`sync.WaitGroup`). `RunTask`'s select loop reads only from this merged channel — it is the single point where the orchestrator learns "an agent said something."

**Why only the orchestrator drains an adapter's channel.** `core.Manager.Adapter`'s doc comment states this directly: "The orchestrator needs it to drain the adapter's Outbound channel; only one consumer may own that channel, which is why the manager does not drain it itself." A channel can have exactly one effective consumer without races or dropped messages; if both the manager and the orchestrator read from the same adapter's `Outbound()`, messages would be nondeterministically split between them. By construction, `core.Manager` never calls `Outbound()` on anything itself — it only exposes the adapter so `mergeOutbound` can do so exclusively, once, per task run.

**Idle-timer reset.** Every value read off `inbox` resets the idle timer (`idle.Reset(o.idleTimeout)`) after safely draining any pending fire (`idle.Stop()` + non-blocking channel drain) — this is the standard Go idiom for reusing a `time.Timer` and avoids a spurious double-fire race.

## 7.2 Live agent activity

A real CLI call takes tens of seconds to minutes. Without streaming, the UI shows a frozen pane for that entire time and the human cannot tell a working agent from a hung one. Both adapters therefore stream while they work.

```
claude --output-format stream-json      codex exec --json
  --include-partial-messages                  |
        |                                     |
   token deltas                        item-level events
        |                                     |
        +------> core.ActivityEmitter <-------+
                 (coalesces ~80ms)
                        |
                 ports.AgentActivity
                        |
              Orchestrator.streamActivity
                        |
            EventBus.PublishEphemeral
                        |
                 TUI per-agent panes
```

Four properties matter, and each is deliberate:

- **Presentation only.** Activity is never persisted, never routed, and never influences task state. The protocol message an agent finally emits remains the only thing the orchestrator acts on, so a noisy or malformed stream cannot affect a deliberation. `streamActivity` runs entirely outside the outbound message path.
- **Never stored or logged.** It goes through `EventBus.PublishEphemeral`, which notifies subscribers without appending to the event repository or the log file. Token deltas arriving many times a second would otherwise bury the record of what actually happened.
- **Coalesced.** `core.ActivityEmitter` batches same-kind text on an 80 ms timer. Claude emits many small token deltas and Codex emits occasional large items; both arrive at the reader as periodic chunks rather than a flood.
- **Never blocking.** The emitter drops rather than blocks when its channel is full. A stalled UI must not be able to wedge an agent mid-answer: losing presentation output is always preferable to losing the agent's actual work.

The two CLIs stream at different granularities, which is a property of the CLIs and not of this code. Claude gives token-level deltas plus `thinking` blocks and tool-use starts, so its pane updates continuously. Codex gives item-level events — whole reasoning steps, command executions, the final message — so its pane updates in visible steps. Codex's authoritative reply is still read from `--output-last-message`, never scraped from the stream.

Verified live: one three-agent task produced 417 activity events across all three agents (362 text, 35 tool, 20 status), all arriving while the agents were still working.

## 7.1 Shutdown cancels in-flight work

When a task ends — completed, cancelled or failed — `RunTask`'s deferred `stopParticipants` calls `Stop` on every adapter, and `Stop` **kills any in-flight CLI invocation** rather than waiting for it.

Each adapter creates a `lifetime` context at `Start`. Every invocation is parented on it, and separately bound to the caller's context via `context.AfterFunc`, so whichever is cancelled first takes the child process with it. Because the invocation runs under `exec.CommandContext`, cancellation terminates the OS process.

This matters for cost, not just tidiness. Previously `Stop` waited on a `sync.WaitGroup` for in-flight calls to finish naturally. Observed live:

```
09:55:38  opus -> codex DELEGATE
09:55:38  opus -> scout DELEGATE
09:55:38  opus -> human FINAL_DECISION
09:55:38  TASK-001 completed
09:55:53  CODEX is STOPPED      <- 15 seconds later
09:55:53  OPUS is STOPPED
09:55:53  SCOUT is STOPPED
```

The authority delegated to two agents and decided in the same instant. Both delegations ran to completion after the task was already over, billing tokens for answers that were discarded, and holding the process open for 15 seconds. With the default `responseTimeout: 5m` the stall could be minutes per agent. After the fix the same scenario stops within the same second.

Note this is about *in-flight* calls, not orphaned processes: no leak was ever observed on normal exit, since the children are bound to the run context and reaped when the orchestrator returns. Regression test: `TestStopKillsInFlightInvocation`.

## 8. Storage strategy

`storage.NewInMemory()` is the only backend wired up (`app.Build` calls it unconditionally regardless of `config.KnowledgeConfig.Backend`, which is otherwise unused at present). It is goroutine-safe (each repository guards its maps with a `sync.RWMutex`) and returns deep copies (`Clone()`) on every read/write so callers cannot mutate repository-held state through a returned pointer.

`internal/storage/sqlite.go` is "intentionally contains no compiled logic" (its own comment) — a documented schema sketch (one table per repository, `task_sequence`/`decision_sequence` tables for the `NextSequence` counters, an `FTS5`-based plan for replacing the in-memory package's naive substring `Search`). Because every caller above `internal/storage` depends only on `ports.*Repository`, adding `NewSQLite(path string) (*ports.Storage, error)` and switching `app.Build` to call it is, per the package doc, "a change confined entirely to this package's constructor — no other package needs to change." No database *server* is ever assumed; SQLite, when added, is embedded and file-based, matching the project's local-first constraint.

## 9. Extensibility: adding a third agent

Adding a new agent is configuration, not code, provided its adapter kind (`claude-code`, `codex`, or `mock`) already exists. `ritam.example.yaml` documents this directly with a commented-out `reviewer` entry. Mechanically: `app.Build` iterates `cfg.Agents` in sorted-name order, and for each enabled entry calls `buildAdapter` (switches only on `ac.Type`) and `registry.Register` — nothing in `app.Build`, `Orchestrator`, `MessageBus`, or `AuthorityManager` assumes exactly two participants or hard-codes `"opus"`/`"codex"` as identifiers anywhere (`mockagent.counterpart`, for instance, finds "the other agent" generically by scanning `Task.Participants`). The only place agent count matters is `TaskManager.Create`'s soft warning ("A single-agent task is legal but pointless for deliberation") when fewer than two participants are given, and `runHeadlessDemo`'s check for "at least two enabled agents" — both are guardrails, not hard requirements of the coordination core itself.
