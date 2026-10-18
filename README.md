# Ritam

*Ṛtam* (ऋतम्) is the Vedic principle of order that keeps everything on its proper course. Here it is the orchestrator that keeps every agent within its role.

A local, terminal-based orchestrator that coordinates multiple coding-agent CLIs (Claude Code, Codex) on a single task, with one agent holding final decision-making authority and the others contributing investigation, implementation and review. The orchestrator — not any agent's own reasoning — owns task state, message routing and the authority model, and enforces all of it in code.

The code in this repository is a working scaffold: the coordination core (state machine, message bus, authority enforcement, decisions, shared knowledge, event log, TUI) is implemented and tested end to end against **mock** agents. The adapters for real Claude Code and real Codex now drive the actual CLIs — one process per protocol message, real prompts, real replies parsed back into the protocol. **Claude Code integration is verified against a live run end to end.** **Codex integration is implemented and its invocation confirmed to reach the API, but no live reply has ever been parsed — the test workspace is out of API credits.** See [What is currently implemented](#what-is-currently-implemented) below for the precise line, and [docs/future-integration.md](docs/future-integration.md) for the full detail.

Related documents:

- [docs/architecture.md](docs/architecture.md) — component responsibilities, layering rules, security model, concurrency, storage strategy
- [docs/agent-protocol.md](docs/agent-protocol.md) — the internal message protocol, message types, validation, wire formats
- [docs/task-lifecycle.md](docs/task-lifecycle.md) — the task state machine, transition table, deliberation loops
- [docs/future-integration.md](docs/future-integration.md) — how real Claude Code / Codex adapters and the LLM proxy get connected

## 1. What this is

`ritam` is a single Go binary (`cmd/ritam`) that:

- lets a human create a **task** with a title, description, an **authority agent** and a set of **participant agents**;
- starts each participant's agent runtime and drives a structured, routed conversation between them until the authority agent issues a final decision or a human intervenes;
- records every message, state transition, decision and violation as an append-only **event log**;
- shows all of this in a terminal UI (Bubble Tea), or prints it as a flat event log in `-headless` mode.

It is explicitly not a chat UI in front of an LLM. There is no single model call; there is a stateful, multi-agent protocol with an authority model enforced by the orchestrator, independent of what any agent's own output claims about itself.

## 2. Architecture overview

```
                              HUMAN
                                |
                                v
                              TUI  (internal/tui)
                                |  ports.Service
                                v
                        ORCHESTRATOR  (internal/orchestrator)
                  +-------------+-------------+-----------------+
                  |             |             |                 |
                  v             v             v                 v
               TASKS        MESSAGES      KNOWLEDGE          DECISIONS
          (TaskManager,   (MessageBus,   (internal/         (internal/
           state machine)  AuthorityMgr)  knowledge)          decisions)
                  \             |             |                 /
                   \            v             |                /
                    \      AGENT MANAGER <----+---------------+
                     \     (internal/agents/core)
                      \          |
                       +---------+---------+
                       |         |         |
                       v         v         v
                    opus       codex    (mock / future
                  (claude-    (codex     agents)
                   code)      adapter)
```

The TUI never talks to storage or to agents directly — it only calls `ports.Service` (implemented by `internal/app.Service`), which delegates to the `Orchestrator`. The orchestrator is the single source of truth for task state, message routing and authority; agents supply information and requested actions, never state.

## 3. Agent roles

Four roles exist (`domain.AgentRole`): `AUTHORITY`, `IMPLEMENTER`, `REVIEWER`, `OBSERVER`. The team is recruited in the TUI; the authority of each task is ticked in the new-task form, and the others take part as implementers. For example:

- **The authority (e.g. opus-1).** Holds final decision-making responsibility for a task: only the authority agent may issue `FINAL_DECISION`, `APPROVE`, `REJECT`, `REQUEST_CHANGE`, `IMPLEMENT` or `DELEGATE` messages. **Authority means responsibility, not correctness.** The orchestrator does not treat the authority agent's claims as ground truth — it only ever confirms *who is allowed to make the call*, never *whether the call is right*. Supporting agents may disagree with the authority agent right up to (and including) the final decision; that disagreement is recorded as a `CHALLENGE` message and an `AgentChallenged`/`Disagreement` event, not hidden or discarded.
- **Supporting agents (e.g. sonnet-1, gpt-6-sol-1).** Investigates, proposes, implements, reports results, reviews and may challenge conclusions it does not agree with. It cannot issue a final decision or force a change on its own authority.

Any role other than `OBSERVER` may challenge (`AgentRole.CanChallenge`). Adding a third agent is a recruit in the TUI, not a code change: every agent is wired the same way, whatever its provider (`internal/app/team.go`).

## 4. Task lifecycle (summary)

A task moves through an explicit, orchestrator-owned state machine (`internal/domain/task.go`): `CREATED -> PLANNING -> INVESTIGATING/IMPLEMENTING/DELIBERATING -> REVIEWING -> AWAITING_DECISION -> COMPLETED`, with loops back from `REVIEWING` to `IMPLEMENTING` (on `REQUEST_CHANGE`) and from `DELIBERATING` to `INVESTIGATING` (on `CHALLENGE`), plus `PAUSED`, `FAILED` and `CANCELLED` reachable from almost anywhere. There is no fixed number of rounds — the authority agent decides when it has enough evidence to issue a decision; only human-set limits (`maxDeliberationRounds`, `maxMessages`, `maxTaskDuration`) can force a pause, and reaching a limit never auto-selects a winner. Full detail, including the exact transition table read from the code, is in [docs/task-lifecycle.md](docs/task-lifecycle.md).

## 5. Message lifecycle

Every interaction between agents (and between the human and an agent) is one `domain.Message`, carrying a `Type` from a fixed vocabulary (`TASK_ASSIGNMENT`, `DELEGATE`, `QUESTION`, `ANSWER`, `PROPOSAL`, `REVIEW`, `CHALLENGE`, `APPROVE`, `REJECT`, `REQUEST_CHANGE`, `IMPLEMENT`, `IMPLEMENTATION_RESULT`, `TEST_RESULT`, `FINAL_DECISION`, `STATUS_UPDATE`, `ERROR`, plus `INFORMATION`). A message goes: **agent emits -> orchestrator validates (identity, routing, authority) -> recorded to the message log -> event emitted -> delivered to the recipient's adapter -> task state updated if the message type implies a transition.** Full detail is in [docs/agent-protocol.md](docs/agent-protocol.md).

## 6. Authority model

Authority is enforced by `internal/orchestrator/authority.go`'s `AuthorityManager`, in code, not by prompting an agent to "please only decide if you're the authority." Specifically:

- `MessageType.RequiresAuthority()` marks `FINAL_DECISION`, `APPROVE`, `REJECT`, `REQUEST_CHANGE`, `IMPLEMENT` and `DELEGATE` as authority-only.
- `AuthorityManager.Authorize` checks every outbound message against the task record's `AuthorityAgent` field before it is ever recorded or delivered. A supporting agent attempting one of these message types is rejected with an `AUTHORITY_VIOLATION`.
- The agent's own claimed identity is never trusted for authority purposes: `Orchestrator.handleOutbound` overwrites `Message.FromAgent` with the transport-verified `out.AgentID` if an adapter's message disagrees, and records an `IDENTITY_VIOLATION` instead of acting on the claim.
- A rejected message is **not silently dropped** — it is recorded as an `AuthorityViolation` or `ValidationRejected` event, and the task continues; a misbehaving agent does not get to stall the whole task.
- The human (`domain.HumanAgentID = "human"`) can always act, independent of which agent holds task authority (`Pause`, `Resume`, `Cancel`, `Approve`, `Reject`, `SendInstruction`).

This is exercised directly by `internal/orchestrator/orchestrator_test.go`'s `TestAuthorityViolationIsRejected`, which drives a "rogue" agent that attempts an unauthorized `FINAL_DECISION` and a delegation to a non-participant, and asserts both are rejected while the task keeps running.

## 7. Shared knowledge model

`internal/knowledge` wraps a `ports.KnowledgeRepository` behind a `Store` that agents and the orchestrator use to persist and retrieve durable project context — investigation findings, test results, decisions, agent notes — scoped either to a task or project-wide, instead of every agent replaying the entire message history on every turn. The orchestrator automatically records: the task itself on creation, `ANSWER`/`CHALLENGE` content as `INVESTIGATION` entries, `TEST_RESULT`/`IMPLEMENTATION_RESULT` content as `TEST_RESULT` entries, and every accepted/superseded decision. `Store.ContextFor` is the intended "give me what's relevant" entry point: it searches task-scoped and project-wide entries together, ranked by score then recency. Knowledge is stored with everything else (SQLite by default, see "Storage and restarts"); search is still a naive substring scan, with the same ranking in both backends. There is no full-text or vector search yet.

## 8. Agents communicate only through the orchestrator

Agents never call each other directly, and no agent can execute a command inside another agent's runtime — each agent is an isolated OS process (or, for tests, an isolated in-process mock) with its own stdin/stdout, and the orchestrator is the only component that reads any agent's output or writes to any agent's input.

```
   OPUS                     ORCHESTRATOR                     CODEX
    |                             |                             |
    |--- DELEGATE (to=codex) ---->|                             |
    |                             |  Authorize() checks:        |
    |                             |   - opus is a participant   |
    |                             |   - codex is a participant  |
    |                             |   - DELEGATE requires       |
    |                             |     authority; opus IS      |
    |                             |     the authority agent     |
    |                             |  Route(): record + emit     |
    |                             |  event, then deliver ------>|
    |                             |                              (codex's adapter
    |                             |                               receives it via
    |                             |                               SendMessage, never
    |                             |                               directly from opus)
```

The recipient's adapter only ever receives a message through `AgentGateway.Send`/`SendTask` (`internal/orchestrator/bus.go`'s `MessageBus.deliver`), which is the orchestrator writing to that agent's own process stdin (`ports.AgentAdapter.SendMessage` -> `core.ProcessRuntime.Send`). There is no shared filesystem handoff, no shared shell, and no path by which one agent's adapter can reach another agent's process.

## 9. How Claude Code and Codex plug in

`internal/agents/claude` and `internal/agents/codex` drive the real CLIs. Both are built on `core.OneShotRunner` (`internal/agents/core/oneshot.go`): one `claude`/`codex` process per protocol message, resolved on `PATH` via `exec.LookPath`, invoked with `exec.CommandContext` using a `[]string` of arguments — never a shell string — and required to exit before its output counts, under a configurable timeout.

**Why one process per message instead of a long-lived session.** These CLIs are built for a human at a terminal; a long-lived session can block forever on an interactive permission prompt nobody is there to answer, and a wedged session looks the same as a slow one. A process that must exit before its reply counts gives a hard timeout, a clean retry boundary, and a crash that costs one message instead of the whole task. The cost — no native memory between calls — is paid back by the orchestrator re-supplying context every call via shared knowledge (`MessageBus.Enrich` / `Orchestrator.attachKnowledge`, see [docs/agent-protocol.md](docs/agent-protocol.md#8-knowledge-injection-into-delivered-messages)).

**Claude invocation:** `claude -p <prompt> --output-format json`, plus `--allowedTools Read,Grep,Glob --disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit --permission-mode dontAsk` when the agent is read-only (default), or `--permission-mode acceptEdits` when `agents.<id>.allowWrites` is set. The CLI returns one JSON object; the adapter reads `.result` and checks `.is_error`/`.subtype`. Reply header format: `@@ORCH type=<TYPE> to=<agent-id>`.

**Codex invocation:** `codex exec --sandbox <read-only|workspace-write> --skip-git-repo-check --color never --output-last-message <tmpfile> [--model M] [--cd DIR] <prompt>`. The final message is read from the `--output-last-message` file, never scraped from stdout (which also carries banner/reasoning/progress text). `--skip-git-repo-check` is required because this orchestrator deliberately does not depend on being run inside a git repository. The prompt is the last positional argument so it can never be parsed as a flag. Reply header format: `##CODEX TYPE=<TYPE> TO=<agent-id>`.

The two reply header formats (`@@ORCH` vs `##CODEX`, case-sensitive vs case-insensitive keys) are kept deliberately different — the two CLIs are separate applications, and sharing an assumed wire protocol between them would be wrong the moment either one changes.

**Verified:** a real task ran to completion against two live Claude Code processes (opus as AUTHORITY, a sonnet-backed "scout" as IMPLEMENTER), completing in 82 seconds via `TASK_ASSIGNMENT -> ANSWER -> DELEGATE -> ANSWER -> FINAL_DECISION -> COMPLETED`.

**Verified:** the Codex adapter has completed live round trips through the orchestrator — `opus -> codex DELEGATE`, `codex -> opus ANSWER`, parsed, routed and accepted into a `FINAL_DECISION`. A three-agent run (Claude authority, Claude implementer, Codex implementer) exercised both providers across ten rounds and then paused for a human on `maxDeliberationRounds`, which is loop protection behaving as designed.

Two real defects were found by running it, both fixed:

- **The prompt was truncated at its first newline.** On Windows the `codex` executable on PATH is a `.cmd` shim, so Windows runs it through `cmd.exe`, which re-parses the command line. The agent received only the opening two lines of its brief and replied *"Understood. Send the coding task"*. The prompt is now written to the process's stdin (`codex exec ... -`), which avoids shell parsing, quoting and argument-length limits entirely.
- **Configured `args` could defeat the read-only sandbox.** They were appended after `--sandbox read-only`, and a later flag wins, so a config could add `--dangerously-bypass-approvals-and-sandbox`. Sandbox-, working-root- and output-affecting flags are now rejected from config, the orchestrator's own flags are appended last, and bare positional arguments are dropped (Codex reads the first positional as the prompt). This one was found by a Codex agent auditing `buildArgs` through the orchestrator.

**Still not implemented**, regardless of CLI: session/thread continuity (`--resume` / `codex exec resume` — every call is stateless), streaming/structured output modes (`--output-format stream-json` for Claude, `--json`/`--output-schema` for Codex), and a patch-review step for `IMPLEMENT` (which is why `allowWrites` should stay off). See [docs/future-integration.md](docs/future-integration.md) for the complete detail and the full deferred list.

## 10. How the proxy will eventually fit

`internal/proxy` defines the seam for a future local LLM proxy (`ports.LLMProxy`). **The proxy is not the orchestrator.** Its job, once built, is entirely about provider traffic: translating between the Anthropic/OpenAI wire formats, request logging, token accounting, and context optimization (caching, trimming). The orchestrator never depends on a provider SDK and does not depend on the proxy at all in this scaffold — `Orchestrator` has no reference to `ports.LLMProxy` anywhere. Today `Proxy.Start` returns `ports.ErrNotImplemented` whenever it is enabled in config (it is a no-op when disabled, which is the default). See [docs/future-integration.md](docs/future-integration.md) for the plan and `ports.LLMProxy.EnvironmentFor`.

## 11. Running the scaffold

Prerequisites: Go 1.26 or later, on PATH. No other services, no database, no network access and no API keys are required — the scaffold ships with mock agents.

```powershell
# from the repository root
go run ./cmd/ritam                  # terminal UI, using ritam.yaml
go run ./cmd/ritam -demo            # ...and immediately create the demo task
go run ./cmd/ritam -headless        # no TUI: run one task and print the event log
go run ./cmd/ritam -config path\to\other.yaml

# force one mode regardless of the config file:
go run ./cmd/ritam -real                                   # real Claude Code / Codex CLIs
go run ./cmd/ritam -mock                                   # offline scripted agents
go run ./cmd/ritam -real -headless -task "your question"   # one real task, headless
```

### The screen

```
┌──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ RITAM │ TASK-001 test                                                                           ● PAUSED  │ 31 msgs │ 139.2k tok │ $4.18 │
│ PAUSED  TIMEOUT: no agent activity   r  resume   i  instruct                                                                             │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
┌─ TASKS ──────── 2 ─┐ [conversation] activity  events  decisions     f follow · tab switch  ┌─ AGENTS ──────────────────────── 1 working ─┐
│❯● 001 test         │ ┌─ CONVERSATION ─────────────────────────────────────────── 7 msgs ─┐ │❯○ [C] opus authority                 STOPPED│
│ ✓ 002 Follow-up: w…│ │  14:20:00 you ↳ opus  TASK_ASSIGNMENT                             │ │ | [O] codex implementer              WORKING│
│                    │ │    Review the planted-bug exercise and rate both reviewers.       │ │   ↳ working on ANSWER from opus         0.0s│
│                    │ │  14:20:20 opus ↳ codex  DELEGATE                                  │ │ ○ [S] scout implementer              STOPPED│
│                    │ │    Review bugtest/inventory.py cold. List every bug with a line n…│ └─────────────────────────────────────────────┘
│                    │ │  14:22:20 codex ↳ opus  ANSWER                                    │ ┌─ DECISION ────────────────────────── D-001 ─┐
│                    │ │    Findings: Off-by-one in average() at line 12: range(len(xs)-1)…│ │ACCEPTED · opus · 14:23                      │
│                    │ │  14:22:30 scout ↳ opus  REVIEW approve                            │ │Both reviewers pass: 7/7 each. Apply codex's │
│                    │ │    I agree with codex on all seven. One gap: no tests for empty i…│ │fixes; keep scout's report as documentation. │
│                    │ │❯ 14:23:49 opus ↳ you  ANSWER                                      │ │                                             │
│                    │ │    Both reviews are accurate. Each found all 7 planted bugs, with…│ └─────────────────────────────────────────────┘
│                    │ │  14:23:49 opus ↳ codex  ANSWER                                    │ ┌─ ANSWER ─────────────────────────── v zoom ─┐
│                    │ │    I've sent the evaluation to the human directly in the ANSWER a…│ │opus ↳ you  ANSWER                           │
│                    │ │  14:24:00 codex ↳ opus  ANSWER                                    │ │14:23:49 · round 4                           │
│                    │ │    Understood. Thanks for the evaluation.                         │ │─────────────────────────────────────────────│
│                    │ │  14:34:06 ── PAUSED · TIMEOUT: no agent activity                  │ │Both reviews are accurate. Each found all 7  │
│                    │ │                                                                   │ │planted bugs, with no false positives and no │
│                    │ │                                                                   │ │wrong line numbers. But this test was too    │
│                    │ │                                                                   │ │easy to tell the reviewers apart.            │
│                    │ │                                                                   │ │                                             │
│                    │ │                                                                   │ │Verdict                                      │
│                    │ │                                                                   │ │───────                                      │
│                    │ │                                                                   │ │                                             │
│                    │ │                                                                   │ │• Accuracy: 7/7, all lines correct           │
└────────────────────┘ └───────────────────────────────────────────────────────────────────┘ └─────────────────────────────────────────────┘
┌──────────────────────────────────────────────────────────────────────────────────────────────────────── : command, i instruct, / filter ─┐
│❯ ready                                                                                                                                   │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
 tab view ] pane j/k move enter open v zoom r resume i instruct n new / filter : cmd ? help q quit                     3 agents · 1 working 
```

The layout puts things where they matter most while agents work:

- **Header.** The task, its state, elapsed time, messages, tokens and cost. When something needs you, a second row says what and names the key: a decision waiting for approval, a pause and why, an agent that still owes you an answer, or the fact that every agent is a scripted mock.
- **Tasks (left).** Every task, persisted across restarts. Select one with `j`/`k` or a click, or cycle with `{` / `}`.
- **Centre.** Four views. `tab` / `shift+tab` or a click switches between them:
  - **conversation** (the default): one compact entry per message, showing who wrote to whom, the type and verdict, and its gist as one line of plain prose. It also shows the events that change what you should do: pauses, checkpoints, requests closed unanswered, decisions. Raw CLI output, turn bookkeeping and agent start/stop are left out.
  - **activity**: each agent's raw live output (thinking, text, tool calls, status). This is where to look for what they are doing right now.
  - **events**: the task's full audit log.
  - **decisions**: every decision record, including superseded ones.
- **Right column.** **Agents**: one line each, plus what an agent is doing while it works. **Decision**: the latest one at a glance; it turns yellow with `a approve` / `x reject` while it waits on you. **Inspector**: the selected item in full. A message is rendered as the markdown its author wrote: headings, bold, lists, quotes, code blocks and tables.

The inspector follows whatever you last selected: a message in the conversation, an agent, a task (its description, progress, usage and who is waiting on whom), or a decision (its text, reasoning and evidence). `enter` moves focus to it, so `j`/`k` scroll it. `v` zooms it to the whole screen for long reads, and `v` or `esc` returns.

The layout drops columns rather than squeezing them. Below 130 columns the task list becomes a centre tab. Below 96 columns the agents and the inspector do too, and `enter` opens the inspector tab.

Identity is **sigil plus colour** (`[C] opus`, `[O] codex`, …), never colour alone, so it survives `NO_COLOR`. Selection is reverse video plus a leading `❯` / `>` marker, and the active tab is bracketed. Set `NO_COLOR` or `RITAM_ASCII=1` for a plain-ASCII, monochrome screen.

Tokens and cost are what the CLIs report, and read `--` until they report something. Codex reports tokens but not cost.

### Keys and commands

| | |
|---|---|
| `tab` `shift+tab` | switch the centre view: conversation, activity, events, decisions, team, settings |
| `]` `[` | move keyboard focus between panes: tasks, centre, agents, decision, inspector |
| `j` `k` `↑` `↓` | in a list (tasks, agents, conversation, decisions): move the selection; in text: scroll |
| `pgup` `pgdn` `g` `G` | page / jump to either end |
| `enter` | open the selection in the inspector; in the inspector, zoom |
| `v` `esc` | zoom the inspector · back out of zoom or a focused agent |
| `a` `x` `i` | approve · reject · instruct (on a completed task, a follow-up) |
| `n` `p` `r` `c` | new task · pause · resume · cancel |
| `{` `}` | previous / next task |
| `d` `e` | jump to the decisions / events view |
| `J` `K` | move the agent selection from any pane |
| `1`-`9`, `0` | expand one agent's live output over the centre · back |
| `f` `t` `h` | follow the newest entry · show/hide thinking (activity) · help bar |
| mouse | click a tab, task, agent, message or decision to select it; the wheel scrolls what is under the pointer (hold Shift to select text) |
| `:` `/` `?` `q` | command line · filter by agent · help · quit |

While following, the conversation keeps the newest entry selected. Moving up stops following, so new messages don't pull you away mid-read. Arriving back at the newest entry, or pressing `f`, resumes following.

`:` opens a command line with completion, for the actions that take an argument:

```
instruct <agent> "..."   inject a human turn into the round
pause [reason]           stop issuing turns, keep the processes
resume | cancel          continue · end the task, terminate child CLIs
approve | reject         answer the pending FINAL_DECISION
task new "..."           create and start another task
filter <agent|all>       show only messages from or to one agent
follow | thinking        <on|off>
tab <name>               switch the centre view
inspect <agent>          point the inspector at an agent
authority <agent>        hand the task's authority to another participant
models                   list every model you can recruit (the team tab)
recruit <provider> <model> [name] [role] [writes]
                         add an agent running a model to the team
dismiss <agent>          remove a recruited agent
settings                 the workspace's access level and command list
access <0-3>             set the access level (raising it asks you to type yes)
allow | disallow <cmd>   add a command to the list · take one off
export jsonl [path]      dump the event log this session has seen
```

Text fields (the instruction prompt, the command line, the new-task form) show everything you type, wrapped over as many rows as it needs, up to a third of the screen, past which they scroll with the end in view. `ctrl+a` selects the whole text (typing replaces it, backspace clears it), `ctrl+u` clears the field and `ctrl+w` deletes the last word. Your own messages stand out in the conversation: a `YOU` badge, a bar down the left edge, and up to four lines of what you wrote.

`export jsonl` writes the TUI's own bounded buffer, not the durable log. It is capped at 500 events, so a long run's oldest events are not there to export. The full history is in `.ritam/ritam.db`.

### Agent sessions and follow-ups

Each agent keeps its own Claude Code / Codex conversation for the task and resumes it on every turn (`claude --resume <id>`, `codex exec resume <id>`), so it remembers the deliberation instead of being re-briefed from scratch by every call.

- **Which session is whose.** The orchestrator records one session per (task, agent) in `.ritam/sessions.json`, in the project's hidden folder. Claude sessions get an id the orchestrator chooses (`--session-id`); Codex announces its thread id at the start of the first turn and that is recorded. The CLIs' "most recent session" shortcuts (`--continue`, `--last`) are never used: the most recent session may be yours, or another agent's.
- **Same place, same permissions.** Claude files sessions by working directory, so every turn runs where the session was created. Read-only permissions are not saved with a session, so they are passed again on every turn; for Codex, whose `exec resume` has no `--sandbox` flag, read-only is forced with `-c sandbox_mode="read-only"`.
- **Compaction.** The CLIs summarise long sessions. Every resumed turn therefore carries a short standing brief (agent id, role, who it may address, the reply format), so a summary that dropped them cannot break the agent's replies.
- **Lost sessions.** If a CLI no longer has a session, a new one is started and the agent is re-briefed with a transcript of the task so far. The registry marks it `rebuilt`, keeps the old id, and the log says so.
- **Follow-ups.** Press `i` (or `:instruct`) on a completed task: it is reopened, the agents resume their sessions, and your message is delivered once they are up. A new `FINAL_DECISION` supersedes the old one, which stays on record. Instructing a paused task asks you to resume it first, rather than failing. A follow-up that only asks something completes again once it is answered. One in which the authority hands the other agents new work (a `TASK_ASSIGNMENT` or `IMPLEMENT`) stays open until the authority sends a `FINAL_DECISION`, as the original task did; the authority is told this when the follow-up reaches it.

### Where ritam keeps its data

ritam keeps files in two places. The app's name is defined once (`internal/brand`), so every folder and file below is named after it.

- **The app home**, one per user, made on first run: `%LOCALAPPDATA%\ritam` on Windows, `~/.ritam` elsewhere, or `$RITAM_HOME`. It holds what belongs to you rather than to a project:
  - `ritam.yaml`, the global settings;
  - `logs/`, one log per project;
  - `projects.json`, the projects ritam knows;
  - `backups/<project>-<id>/`, each project's database snapshots;
  - `archives/`, archives made by `ritam archive`;
  - `models.json`, what was learned about models on your account;
  - `canary/`, for sandbox checks.
- **The project's hidden folder**, `<project>/.ritam`, made the first time ritam runs in a project. It holds the project's own state:
  - `ritam.db`, the database;
  - `team.json`, the project's team;
  - `sessions.json`, the agents' CLI sessions;
  - `ritam.yaml`, the project's settings, which override the global ones;
  - `project.json`, the project's id, so a moved project keeps its backups;
  - `tmp/`, the agents' scratch folder.

**Settings** are read in layers, each overriding the one before: the defaults, the app home's `ritam.yaml`, then the project's (`ritam.yaml` in the project folder and `.ritam/ritam.yaml`). Only what a file sets is overridden; a provider entry is merged field by field.

**Backups.** Each project's database is snapshotted into the app home:
- when a task finishes;
- when ritam exits;
- every `backups.interval` (10 minutes) while work is going on;
- before the database is upgraded.

A snapshot is skipped when nothing changed, so an idle project writes nothing. The newest `backups.keep` (5) are kept. Snapshots use SQLite's `VACUUM INTO`, so they are consistent while ritam is running.

**Deleted projects.** If a project's hidden folder is deleted, ritam finds the project again by its path the next time it starts there, keeps its id, and offers to restore the latest backup. If a project folder is deleted, ritam says so at start (`backups.whenProjectDeleted: ask`) and keeps its backups until you choose, or keeps or deletes them automatically (`keep`, or `delete` after `backups.deleteAfterDays`).

**Commands** (run in the project folder, with ritam closed for those that change data):

| Command | Does |
|---|---|
| `ritam where` | shows the app home, the project folder, the database, log and backups |
| `ritam backups` / `backups now` | lists this project's backups / takes one now |
| `ritam backups projects` | lists every known project, its backups, and whether it still exists |
| `ritam backups delete <project>` / `keep <project>` | deletes a project's backups / keeps a deleted project's backups without asking again |
| `ritam restore [latest\|<file>]` | restores this project from a backup or an archive; what it replaces is backed up first |
| `ritam archive [-clear]` | zips this project's tasks, team and sessions into the app home; `-clear` then starts the project fresh |
| `ritam rename -from <old>` | after the app is renamed, renames the app home and every known project's hidden folder made under the old name (also done automatically for names listed in `brand.FormerNames`) |

### Storage and restarts

- **Everything is kept in one SQLite file per project**, `.ritam/ritam.db` in the project's hidden folder: tasks, messages, events, decisions, disagreements, shared knowledge and artifacts. A completed task can be followed up in a later session; its agents resume their own CLI sessions (`.ritam/sessions.json`), so they still remember it. Task numbers carry on from the last run. Set `storage.backend: memory` to keep nothing.
- **Tasks running when ritam exited come back `PAUSED`** with the reason `INTERRUPTED`. Their agents' turns in progress were lost. Resume the task (`r`) and each agent that still owes a reply is told what it owes; if nobody owes one, the authority is told to carry on. The same happens on resuming from any pause: a pause stops the agent processes, so whatever they were in the middle of is asked for again.
- **One ritam per database.** A second ritam on the same file refuses to start and names the pid holding it. The lock is an OS lock, so a crashed ritam never leaves it stuck.
- **Inspecting it.** Messages and events have ordinary columns (`from_agent`, `to_agent`, `type`, `content`, ...), so any SQLite tool can read the history. Other records are JSON in a `data` column. The schema version is `PRAGMA user_version`; a build refuses a database newer than it understands rather than mangling it.

### Long tasks, consultations and limits

- **Supporting agents can consult each other.** Ask a supporting agent something that needs another agent's check: it sends that agent a QUESTION, gets the ANSWER, and reports back to you. The deliberation pane's **WAITING ON** list shows the chain ("you → scout", nested "scout → codex"), and the inspector shows what each agent is waiting on and owes.
- **Whoever can answer you, does.** A question you ask one agent may be passed along. An agent receiving it is told it is your question and is asked to answer you directly (preferred), telling the agent that passed it on. Either way counts: an answer from anyone along the chain closes your question, while other branches stay open until they answer too. When everyone goes quiet with something still open, the agent that owes the reply gets one reminder, starting from the end of the chain (if opus is waiting on codex, it is codex that is reminded, not opus). An agent that stays silent after its reminder has that request closed as unanswered and recorded: if you already have an answer the task completes; if you do not, the agent that asked is told to give you what it has. If your own question still goes unanswered, the task pauses naming it. Any pause for silence names the exact reply it is waiting for.
- **Verdicts, not approvals.** A supporting agent records agreement or objection with `REVIEW verdict=approve|changes|reject`. Only the authority's APPROVE, REJECT and FINAL_DECISION change a task; a supporting agent that sends one of those is told which message to use instead. Verdicts count towards the consensus shown in the deliberation pane.
- **Agents work until done.** The limits meter autonomous agent work only: your input resets them and messages to or from you never count. Rounds trigger a **checkpoint** (the authority posts a progress update, you are notified, work continues) rather than a pause. What does pause a task:
  - a **stall**: `stallMessages` agent messages in a row with no progress, such as two agents repeating the same point, word for word or nearly. Results, reviews and verdicts, answers and new requests are progress. Status reports and the authority's own instructions are neutral: agents keeping the authority informed while they work are not stalling;
  - long **unattended** work (`maxMessages`, `maxTaskDuration`, both since your last input);
  - the **budget** (`maxTaskCostUSD`, `maxTaskTokens`, from the usage the CLIs report);
  - the absolute `maxTotalMessages` ceiling.

  Resuming grants a fresh allowance. A working agent never trips the idle timeout.
- **Spending is shown, not guessed.** The header's `tok` and `$` are what the CLIs reported. Codex reports tokens but not cost. The inspector breaks a task's spend down by agent (turns, cost, tokens in and out, and cache reads: the session history re-read each turn), and every turn is logged as an `AgentUsage` event. Both CLIs report some figures as running totals for the session (Claude its cost, Codex its tokens); each turn is charged only what it added.
- **The turn that crosses the budget is still delivered.** Usage is reported after a turn's messages, so its reply reaches you before the task pauses.

### Delivery: one turn for everything waiting, and the team in view

Each agent turn is a CLI call of a minute or more, so messages are not handed to an agent one turn each. They wait in the agent's inbox and are handed over when it is free:

- **Combined.** Everything waiting reaches the agent as one turn, oldest first, your messages marked to be answered first. In a real run, before this, codex received 64 messages, took 16 turns, and ended 48 messages behind, answering messages ten minutes old and never reaching the question you asked it.
- **Held while the agent waits on a co-worker.** An agent that asked another for something, while that one is still working on it, is not woken by the chatter in between. Its messages wait for the answers, so it acts on everything together. Your messages, and requests addressed to the agent, always go straight through, and a hold never lasts more than four minutes.
- **The team board.** Every delivery carries the team status: who is working, how many messages each has unread, who is waiting on whom and who owes whom a reply. Every agent can read it; only the authority may act on it (re-delegate, wait for an agent that is behind). The agents pane shows each agent's unread count too.

A final decision does not drop a question you asked: the decision is accepted, and the task completes once the agent you asked has answered you (or, after a reminder, completes and records that it did not).

A reply that is partly malformed is not thrown away: an unknown message type is delivered as INFORMATION, a block with no recipient is dropped, and the agent is told what to fix.

### The team: recruiting agents and choosing models

The **team** tab lists the team and every model the providers offer. Claude Code's are what its `/model` picker offers (the opus, sonnet, haiku and fable tiers, plus your account's extra options); Codex's come from `codex debug models`, with its cache as the fallback, and models the CLI hides are listed last. Select a model, press `enter`, name the recruit, and it joins the team. Or use `:recruit claude opus opus-2 reviewer`.

- **The same model, many agents.** Recruit `opus-2` alongside `opus`: two participants running the same model.
- **Choose the team while creating a task.** The new-task form (`n`) starts with nobody on it. **Recruit** lists every model (models a CLI hides are left out): up/down to move, space to tick a model, left/right or a digit for how many agents of it. **Participants** is built only from that, every agent named after its model: Opus x2 and Sonnet give `opus-1`, `opus-2`, `sonnet-1`. No other agent is pulled in; a name of that form already on the team for the same model is simply used again (an agent is only a definition, and each task gets its own instance). The **authority** is ticked there by you; nobody is pre-selected, and the task needs one and at least two participants. On create the agents not yet on the team are recruited (able to write, within the workspace, unless their provider says `allowWrites: false`), join the task, and stay on the team for later tasks.
- **Models your account cannot run.** A turn that fails because the account has no credits, the plan does not include the model, the CLI does not know it, it is not logged in, or a usage limit was hit is recognised and said plainly, instead of the agent being asked to reply again. A supporting agent that cannot run drops out and the authority is told to carry on without it; an authority that cannot run pauses the task with the reason and what to do. A model that failed for credits, plan or name is remembered as unavailable (in `.ritam/models.json`) and marked in the team tab and the form, which will not let you pick it. `c` in the form's Recruit field, or `:check` in the team tab, makes one short call (a few cents) to check a model runs on your account; only when you ask.
- **The same agent, many tasks.** Every task gets its own instance of each of its agents, with its own CLI processes and session, so one agent can work on several tasks at once (raise `maxConcurrentTasks`). The agents pane shows each agent's state on the selected task.
- **Recruits are kept** in `.ritam/team.json`, beside the database: each project has its own team. Settings files are never written to. `:dismiss` removes a recruit, not while it is working on a running task.
- **Changing the authority.** `:authority codex` hands the selected task's authority to another participant, mid-task or between follow-ups. Both agents and the rest of the team are told, and from then on only the new authority may issue the FINAL_DECISION.

### Watching the agents work

**In the conversation.** An agent working on its turn has a `thinking…` entry in the conversation, at the time the turn began, drawn like any other entry; the message it posts replaces it. Following the conversation keeps the newest message selected rather than jumping to a thinking entry; select one to see that agent's live output in the inspector. Messages an agent sends you are marked in orange (a bar down the left and a `YOU` badge), and the final decision in violet (a `FINAL DECISION` badge); both show up to four lines of their text, like your own messages in white. With a task selected, the agents pane lists only that task's participants; the whole team is in the team tab.

Both adapters stream while they think, so the TUI shows each agent's output live (the activity view, the roster, and `1`-`9` for one agent) rather than a frozen screen for the minutes a real CLI call takes. Claude streams token deltas (including `thinking` blocks and tool-use starts); Codex streams item-level events (reasoning steps, command executions, the final message), so its pane advances in visible steps rather than continuously — a difference in the CLIs, not in this code.

This output is presentation only: it is never persisted, never routed, and never influences task state. The protocol message an agent finally emits is still the only thing the orchestrator acts on. See [docs/architecture.md](docs/architecture.md) §7.2.

**Which agents am I actually running?** The roster marks scripted agents `mock`, the inspector names each agent's CLI (`claude-code`, `codex`, `mock`), and the header shows a `MOCK AGENTS` banner when every agent is scripted. Mock replies are a fixed script that ignores your task entirely, and on screen they are indistinguishable from real ones — so check the pane before concluding anything from a run.

`config.Default()` (used when no config file exists) and `ritam.example.yaml` both default `tui.useMockAgents` to `true`, so a fresh checkout with no config never reaches a real CLI or the network by accident.

**`-real`** sets `useMockAgents = false` and raises `orchestrator.idleTimeout` to at least 10 minutes if it is lower — real CLI calls routinely take minutes and the idle timeout must not fire mid-answer. **`-mock`** forces the scripted agents and overrides the config; the two flags are mutually exclusive. **`-task`** (only meaningful with `-headless`) supplies the question to run instead of the built-in demo.

### ritam.yaml says how to run the providers, not who the agents are

An "agent" is an orchestrator participant, not a CLI installation: one CLI backs as many agents as you recruit. `ritam.yaml` defines no agents. It says how to start each provider's CLI, and every agent of that provider is started that way:

```yaml
providers:
  claude-code:
    executable: claude      # on PATH, or an absolute path
    responseTimeout: 5m
    allowWrites: true       # false: this provider's agents are read-only
    allowedCommands: []     # extra commands, besides access.commands
  codex:
    executable: codex
    responseTimeout: 5m
```

Both entries are optional; left out, `claude` and `codex` on PATH are used. The team itself is recruited in the TUI (the new-task form, or the team tab) and kept in `.ritam/team.json`. An older `agents:` section still loads, but is no longer needed: delete it and recruit from the form instead.

**Auth** is each CLI's own: run `claude /login` and `codex login` once, out of band, before using `-real`. No API key is ever read from `ritam.yaml`, an environment variable, or any other config source by this project — see the constraints section below.

**Read-only agents** (`providers.<name>.allowWrites: false`, or a recruit made without writes). Claude Code is launched with an explicit tool allowlist (`Read`,`Grep`,`Glob`) and denylist (`Bash`,`Edit`,`Write`,`MultiEdit`,`NotebookEdit`) plus `--permission-mode dontAsk`; Codex is launched with `--sandbox read-only`.

**The workspace.** ritam is meant to be launched inside a project folder. The workspace is the folder holding `ritam.yaml` (or `workspace.root`): agents run there, an agent's `workingDir` must lie inside it, and an agent with `allowWrites` may write there and nowhere else. ritam's own files stay in its `.ritam` folder there too, temp files included. Launched at a global location (your home folder, a drive root, a system folder), every agent is read-only and the header says why; `workspace.allowGlobal: true` lets them write, but only into ritam's own folder, `~/.ritam/workspace`.

**Access levels.** What a write-enabled agent may run is the workspace's access level, chosen in the settings tab (`:settings`, or `:access <0-3>`) and saved to `.ritam/ritam.yaml`:

| Level | | What agents may run |
|---|---|---|
| 0 | Files only (default) | edit files, run read-only commands; a build or test is asked of you |
| 1 | Sandboxed builder | only agents whose CLI runs in a sandbox (Codex) run the listed commands; the others are told to ask them |
| 2 | Build and test | every agent runs the listed commands (`:allow cmake`, `:disallow cmake`) |
| 3 | Any command | anything inside the workspace except the always-blocked list |

Raising the level asks you to type `yes`, after saying plainly what it hands the agents; lowering applies at once. A change applies from each agent's next turn, the header shows the current level, and each task's events record the level it started at and every change. A global launch stays at level 0.

A project's settings files travel with it, so a level they ask for is only granted once you approved it on this machine, for this project in this folder. The approval is recorded in the app home (`projects.json`) when you choose a level in the settings tab. A cloned or copied project, or one moved to another folder, starts at level 0, and a notice says what was held back until you approve it. Levels set in the global settings (the app home's `ritam.yaml`) are your own and need no approval.

Claude Code has no sandbox here, so at level 2 it runs the listed commands directly on your machine and level 3 means you trust these agents: the blocked list is a deny list, not a sandbox.

**Builders.** The new-task form's **Builder** field says who runs builds and tests: **Auto** (the default) follows the access level, **Manual** leaves them to you, or pick one participant. The choice narrows the level, never widens it:

- At level 1, Auto means the participants whose CLI passed the sandbox check; a named participant must have passed it too. With none, you build.
- At levels 2 and 3, Auto means every agent; a named participant builds alone.
- At level 0, or with Manual, you build. The agents are told to ask you with the exact commands.

Every agent that builds uses its own folder, `build/<agent>/`, so outputs never mix. When several agents build, the authority hands out the work; they do not divide it among themselves.

A decision cannot close a task while an agent is waiting for your answer, for example to "run the build and send me the output". It is held for you to answer, approve or reject.

**The sandbox check.** Each adapter declares whether its CLI runs commands in a sandbox: Codex does (`workspace-write`), Claude Code does not. Ritam checks the claim itself. The agent is asked to write one file inside the workspace and one in the app home's `canary/` folder, and Ritam judges the files it finds, not the agent's reply. The file inside must be there (the agent really ran the commands) and the file outside must not (the sandbox stopped it). The result is kept per model in `models.json` with the CLI's version and checked again when the version changes. It runs when a task that could use the model as a builder starts, or with `:check`. Models that passed are tagged **builds & tests** in Recruit and the team tab. Leftover canary files are cleared at every start.

**What a write-enabled agent may do.** Claude Code edits files with `--permission-mode acceptEdits` and may run what the access level grants: no shell at level 0 (and at level 1, having no sandbox), the listed commands plus the agent's own `allowedCommands` as `Bash(<command>:*)` rules at level 2, any `Bash` command at level 3; destructive commands (deleting, disks and system settings, permissions, killing processes, the network, rewriting git history, installing software) and the PowerShell tool are denied outright, and a deny rule wins over an allow rule. Codex runs in its `workspace-write` sandbox with writing to temp folders and network access turned off, at every level; the level decides what its brief allows it to run. Every write-enabled agent is told the rules in its brief, on every turn. There is still no patch-review step (see [docs/future-integration.md](docs/future-integration.md#8-still-deferred--be-precise-about-this)), so writes belong in a human-supervised run.

`go run ./cmd/ritam -headless` runs the complete mock flow end to end and prints exactly this event sequence: task creation; `CREATED -> PLANNING`; a single `TASK_ASSIGNMENT` to the authority agent (supporting agents are not briefed — they act only once delegated to); an `opus -> codex DELEGATE`; `PLANNING -> INVESTIGATING`; `codex -> opus ANSWER`; `-> DELIBERATING`; `opus -> codex IMPLEMENT`; `-> IMPLEMENTING`; `codex -> opus IMPLEMENTATION_RESULT`; `-> REVIEWING`; `opus -> codex REQUEST_CHANGE`; `-> IMPLEMENTING`; `codex -> opus IMPLEMENTATION_RESULT`; `-> REVIEWING`; `opus -> * FINAL_DECISION`; `-> AWAITING_DECISION`; a `DecisionCreated D-001`; `DecisionAccepted`; `-> COMPLETED`.

### Running tests

```powershell
go test ./...
```

There are 14 packages with tests (`internal/agents/claude`, `internal/agents/codex`, `internal/agents/core`, `internal/agents/mock`, `internal/app`, `internal/config`, `internal/decisions`, `internal/domain`, `internal/knowledge`, `internal/logging`, `internal/orchestrator`, `internal/proxy`, `internal/storage`, `internal/tui`) plus two packages with no tests (`cmd/ritam`, `internal/ports`); all 14 pass.

On this Windows machine, an Application Control policy intermittently blocks a freshly built test binary from running (it varies which package is affected from run to run — sometimes none). If `go test ./...` reports a package failing to run for what looks like this reason, just re-run `go test ./...`; the blocked package is rarely the same one twice, and a clean run where all 14 packages pass is the expected end state, not the exception.

Two packages worth calling out specifically: `internal/logging/logging_test.go` proves secrets are redacted both by the standalone `Redact()` function and by the `slog` handler's own attribute redaction (a secret passed as a structured log attribute, not just in a free-text message, still gets stripped before it reaches a log file); `internal/app/e2e_test.go` proves the full stack wiring (`config.Config` -> `core.Registry`/`core.Manager` -> `Orchestrator` -> `Service`) end to end against mock agents — including that an arbitrary number of agents register correctly from configuration (not just the two the example config ships with), that `useMockAgents: false` really does select the real `claude`/`codex` adapters instead of the mock one, and that the proxy stays disabled by default.

## 12. What is currently implemented

| Area | Status |
|---|---|
| Domain model (task/message/decision/knowledge/artifact/event/agent types, task state machine) | Implemented, tested |
| Orchestrator: `TaskManager`, `MessageBus`, `AuthorityManager`, `EventBus`, `RunTask` loop | Implemented, tested |
| Authority enforcement (authority-only message types, identity verification, routing checks) | Implemented, tested |
| Loop protection (`maxDeliberationRounds`, `maxMessages`, `maxTaskDuration`, idle timeout) — pauses for a human, never auto-picks a winner | Implemented, tested |
| Decisions (propose/accept/reject/supersede, durable history) | Implemented, tested |
| `STATE_VIOLATION` enforcement — a terminal or `PAUSED` task rejects any agent-originated message; only the human is exempt | Implemented, tested |
| `AWAITING_DECISION` reachable from every working state (`PLANNING`/`INVESTIGATING`/`IMPLEMENTING`/`REVIEWING`/`DELIBERATING`); `Orchestrator.completeAfterDecision` walks `AWAITING_DECISION -> COMPLETED` explicitly so a decision reached in one round still completes the task | Implemented, tested (`TestDecisionFromAnyWorkingStateCompletes`, `TestImmediateFinalDecisionCompletesTheTask`) — see [docs/task-lifecycle.md](docs/task-lifecycle.md) |
| `TaskManager.Create` rejects an authority-only task (no one to delegate to) and rejects duplicate participants; a rejected agent action is reported back to the agent as an `ERROR` message (`Orchestrator.notifyRejection`) instead of leaving it in silence | Implemented, tested |
| `core.OneShotRunner` — shared one-shot CLI transport (`exec.LookPath` resolution, args as a slice, timeout, stdout/stderr capture) used by both real adapters | Implemented; executable resolution is unit-tested through each adapter's tests (e.g. `TestResolveReportsAMissingCLIClearly`), and the full invocation path is confirmed against real `claude`/`codex` processes rather than by a dedicated `oneshot_test.go` |
| Disagreement recording (`Orchestrator.recordDisagreement`/`resolveDisagreements`, `DisagreementRepository`, `Service.TaskDisagreements`) — a `Disagreement` record is raised on every `CHALLENGE` and resolved on `FINAL_DECISION`, never deleted | Implemented, tested at the orchestrator/storage layer; **not yet surfaced in the TUI**, and `TaskDisagreements` is not yet part of `ports.Service` (`TODO(phase-next)` in `internal/app/service.go`) |
| Shared knowledge store (put/get/update/delete/search, task+project-wide context retrieval) | Implemented, tested; naive substring search only |
| Storage for tasks/messages/events/decisions/knowledge/artifacts/disagreements: SQLite (default, persistent, pure-Go driver, schema migrations, single-instance lock) and in-memory, held to the same conformance tests; interrupted tasks are recovered as `PAUSED` on startup | Implemented, tested |
| Mock agent adapter + scripted behaviors (authority/implementer/challenging/rogue/crashing/silent) | Implemented, tested; this is what actually drives `-headless` and `-demo` |
| Agent runtime manager/registry, generic `os/exec`-backed `ProcessRuntime` (start/stop/restart, piped I/O, graceful-then-kill shutdown) | Implemented, tested |
| Terminal UI (Bubble Tea): three-column responsive layout (agent roster with per-agent live activity, merged event stream, deliberation history, agent inspector), 60-second throughput window, `FINAL_DECISION` approval gate, `:` command line with completion, stream filtering, per-agent columns and `1`-`9` full-screen focus, task list, human override actions, 16-colour palette with sigil-based identity and a full ASCII/`NO_COLOR` fallback | Implemented |
| Live agent activity streaming (`core.ActivityEmitter`, `--output-format stream-json` / `codex exec --json`) | Implemented, verified live: 417 activity events across three agents in one task. Presentation only — never persisted, routed, or acted on |
| Claude Code adapter (`internal/agents/claude`) | **Implemented and verified live**: one-shot `claude -p ... --output-format json` invocation, read-only tool policy / `acceptEdits` write mode, JSON envelope parsing, `@@ORCH` header parsing, knowledge injection into the prompt. A real task ran end to end against two live Claude Code processes and completed. Persistent sessions (`--session-id` / `--resume`), stream-json output and per-turn, per-agent cost and token accounting are implemented |
| Codex adapter (`internal/agents/codex`) | **Implemented and verified live**: one-shot `codex exec --sandbox ... --output-last-message <file> -` invocation with the prompt on **stdin** (a `.cmd` shim on Windows truncates a multi-line argument), `##CODEX` header parsing, knowledge injection, and filtering of configured `args` that would defeat the sandbox. Confirmed end to end through the orchestrator: delegate sent, real reply parsed, routed and accepted. Session continuity, streaming/`--output-schema` structured output are **not implemented** |
| LLM proxy (`internal/proxy`) | Seam only: `Start` returns "not implemented" when enabled; no HTTP listener, no request transformation, no token accounting |
| SQLite storage backend | Not implemented; `internal/storage/sqlite.go` is a documented schema sketch with no compiled logic |
| Automatic agent restart/recovery after a crash | Not implemented (manual `Restart` only; see `TODO(phase-next)` in `internal/agents/core/manager.go`) |
| Human-approval gate keeping the run loop alive after `FINAL_DECISION` for an immediate human `REJECT` | Not implemented (`TODO(phase-next)` in `internal/orchestrator/orchestrator.go`); `RequireHumanApproval` currently ends the run and a fresh run must be started to resume |

## 13. What is intentionally deferred to future phases

- **A patch-review step for `IMPLEMENT`**, which is why `allowWrites` should stay off. See [docs/future-integration.md](docs/future-integration.md).
- **The local LLM proxy** (phase 10): HTTP listener, wire-format transformation, request logging, token accounting, context optimization, credential handling.
- **Full-text / vector knowledge search**: FTS5 (or an embedding index) behind `KnowledgeRepository.Search`; today it is a substring scan.
- **Automatic crash recovery**: restart-with-backoff policy for agents that land in `AGENT_ERROR`/`AGENT_UNAVAILABLE`.
- **Keeping a task's run loop alive across a human-approval gate** so a human `REJECT` can send the task straight back into deliberation without a fresh `RunTask` call.
- **Vector/embedding-based knowledge search**, as a replacement for the current substring scan.
- **A TUI surface for knowledge search and disagreements**: `Store.ContextFor` and `Service.TaskDisagreements` exist and are tested, but neither has a pane in the terminal UI, and `TaskDisagreements` is not yet part of `ports.Service` (`TODO(phase-next)` in `internal/app/service.go`).

## Directory structure

```
cmd/ritam/                 entry point: flag parsing, wiring, headless demo runner
internal/domain/          dependency-free core types: Task, Message, Decision, Agent, Event, Artifact, Knowledge
internal/ports/           interfaces connecting domain to storage, agents, proxy and the TUI
internal/orchestrator/    coordination core: EventBus, AuthorityManager, TaskManager, MessageBus, Orchestrator
internal/agents/core/     shared agent plumbing: Registry, Manager, os/exec-backed ProcessRuntime, OneShotRunner
internal/agents/mock/     in-process scripted agent adapter used for tests, -headless and -demo
internal/agents/claude/   Claude Code adapter: drives the real `claude` CLI (@@ORCH wire format)
internal/agents/codex/    Codex adapter: drives the real `codex` CLI (##CODEX wire format)
internal/knowledge/       shared-knowledge store built on ports.KnowledgeRepository
internal/decisions/       decision lifecycle manager (propose/accept/reject/supersede)
internal/storage/         repository implementations: SQLite (sqlite*.go) and in-memory (memory*.go)
internal/proxy/           seam for the future local LLM proxy (not implemented)
internal/app/             application service (ports.Service) and Build/wire.go
internal/tui/             Bubble Tea terminal UI
internal/config/          ritam.yaml loading and validation
internal/logging/         structured logging with credential redaction
docs/                     detailed documentation (see links at the top of this file)
ritam.example.yaml         example configuration; copy to ritam.yaml to use
```

## Constraints

- **No Git.** This project deliberately has no source control checked in; do not initialize one.
- **Local-first.** No cloud services are used or assumed. Configuration is a local YAML file only (`internal/config`); there is no remote config source.
- **No database server.** Storage is SQLite, an embedded file-based engine (pure Go, no cgo), never a server process.
- **No credentials in configuration.** Claude Code and Codex authenticate through their own CLI login mechanisms; `AgentConfig` and the proxy never carry API keys.
- **Windows/PowerShell is the primary target.** Process management (`core.ProcessRuntime`) and path handling (`config.ResolvePath`) are written with Windows semantics in mind (e.g. no portable SIGTERM, so shutdown is close-stdin-then-kill; executables resolved via `PATH`, since Windows commonly uses shims).
