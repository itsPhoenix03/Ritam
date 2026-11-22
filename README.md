# Ritam

*Ṛtam* (ऋतम्) is the Vedic principle of order that keeps everything on its proper course. Here it is the orchestrator that keeps every agent within its role.

A local, terminal-based orchestrator that coordinates several coding-agent CLIs (Claude Code, Codex) on one task, with one agent holding final decision-making authority and the others investigating, implementing and reviewing. The orchestrator, not any agent's own reasoning, owns task state, message routing and the authority model, and enforces them in code.

It runs offline against scripted mock agents by default; `-real` drives the real `claude` and `codex` CLIs, one process per agent turn, each agent resuming its own CLI session for the task.

## Status

| | What |
|---|---|
| **Implemented** | Task state machine, authority enforcement and routed messages; decisions with a superseded-never-deleted history; the event log; the terminal UI and `-headless`. Claude Code and Codex adapters: one CLI process per turn, the prompt on stdin, the agent's session resumed every turn, output streamed live to the UI. SQLite storage (the default) with transactions, versioned task rows, a durable delivery queue and a record of every agent turn. Decisions held for the human that can be answered, approved or rejected, and that survive a pause and a restart. A failure policy by kind: retries with backoff, a supporting agent dropped, the task paused when it is the authority; a task is never failed over one agent. Retry of a `FAILED` task. Queued runs shown and cancellable. Spending limits per task and per day, the day's record shared safely between ritam instances and failing closed. Raising a task's limits. Strict, validated settings and `ritam config check`. Access levels, the protected-file guard, backups and restore. Every tool call a Claude Code agent makes goes through ritam's hook first and is decided by ritam's review: parsed (bash, PowerShell, cmd, file tools), checked against ritam's rules and the turn's history, then refused, refused with the task paused, put to you, or passed, by the review mode (auto or manual); each turn is checked for calls that got past the hook. |
| **Partly implemented** | Per-turn spending cap: Claude Code only; whether the CLI's `--max-budget-usd` counts only the turn on a resumed session is unverified (see [Limits](#long-tasks-limits-and-spending)). Codex spending: tokens are counted and limited, no dollar figure. Roles `REVIEWER` and `OBSERVER`: labels shown in the roster, not enforced. Claude Code's command sandbox: macOS, Linux and WSL2 only, none on native Windows. The review: PowerShell is read by ritam's own tokenizer, not PowerShell's parser; the scripts behind `npm run` and `make` are not followed; no reviewer model; in auto mode a destructive command the rules do not foresee still runs. Codex's tool hook: installed, not yet run live. Disagreement records: stored and resolved, not shown in the UI. Turn records: stored, not shown in the UI. Shared knowledge: substring search only. `-demo` / `tui.demoOnStart`: present, but the task it creates is refused (see [Running](#11-running)). |
| **Not implemented** | A verification gate that runs the project's tests before a decision is accepted. An enforced independent reviewer. Per-turn capture of the files an agent changed. A task board with dependencies between tasks. Git worktrees per agent (all agents work in one folder). Long-lived agent sessions with interrupt (a turn can only be killed). Structured-output replies (`--output-schema` or a JSON schema); replies are free text with a header line. Codex dollar pricing. Automatic resume when a rate limit resets. A review step before an agent's file edits land. The local LLM proxy. Full-text or vector knowledge search. |

Related documents:

- [docs/architecture.md](docs/architecture.md): components, layering, storage, concurrency, failure handling, security
- [docs/agent-protocol.md](docs/agent-protocol.md): message types, validation, the reply header, prompts
- [docs/task-lifecycle.md](docs/task-lifecycle.md): the task state machine and what moves a task
- [docs/future-integration.md](docs/future-integration.md): how the CLIs are driven, what was run live, and what remains

## 1. What this is

`ritam` is a single Go binary (`cmd/ritam`) that:

- lets a human create a **task** with a title, a description, an **authority agent** and the **participant agents**;
- runs each participant's CLI turn by turn and drives a routed conversation between them until the authority issues a final decision or the human intervenes;
- records every message, state change, decision, violation and agent turn in the project's database, and every event in an append-only **event log**;
- shows all of this in a terminal UI (Bubble Tea), or prints the event log in `-headless` mode.

It is not a chat UI in front of an LLM. There is no single model call; there is a stateful, multi-agent protocol with an authority model enforced by the orchestrator, whatever an agent's output claims about itself.

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
           state machine)  inboxes,       knowledge)          decisions)
                           AuthorityMgr)
                  \             |             |                 /
                   \            v             |                /
                    \      AGENT MANAGER <----+---------------+
                     \     (internal/agents/core)
                      \          |
                       +---------+---------+
                       |         |         |
                       v         v         v
                    claude     codex      mock / fake
                    adapter    adapter    (scripted)
```

The TUI never talks to storage or to agents directly: it calls `ports.Service` (implemented by `internal/app.Service`), which delegates to the `Orchestrator`. The orchestrator is the single source of truth for task state, routing and authority; agents supply information and requested actions, never state.

## 3. Agent roles

What the orchestrator enforces is the **task's authority**: one participant, chosen per task (`Task.AuthorityAgent`), who alone may send the authority-only message types. Every other participant is a supporting agent.

- **The authority** decides what is investigated and by whom, and issues the `FINAL_DECISION`. Authority means responsibility, not correctness: the orchestrator only checks *who may make the call*, never *whether it is right*. Supporting agents may disagree right up to the decision; a `CHALLENGE` is recorded as a disagreement, not discarded.
- **Supporting agents** investigate, propose, implement, report, review (`REVIEW verdict=approve|changes|reject`) and challenge. They cannot decide, approve, reject, delegate or assign work.

Agents also carry a role label (`AUTHORITY`, `IMPLEMENTER`, `REVIEWER`, `OBSERVER`): `IMPLEMENTER` unless `:recruit` names `authority` or `reviewer`, or an `agents:` entry in the settings sets one. The roster shows `REVIEWER` and `OBSERVER`, but nothing enforces them: an `OBSERVER` may send what any supporting agent may send, and a `REVIEWER` is not given any independent say. The authority of a task is whoever is ticked as authority in the new-task form, or set later with `:authority`. Every agent is wired the same way, whatever its provider (`internal/app/team.go`).

## 4. Task lifecycle (summary)

A task moves through an orchestrator-owned state machine (`internal/domain/task.go`): `CREATED -> PLANNING -> INVESTIGATING / IMPLEMENTING / DELIBERATING / REVIEWING -> AWAITING_DECISION -> COMPLETED`, with loops (`REQUEST_CHANGE` back to `IMPLEMENTING`, a further `DELEGATE` back to `INVESTIGATING`), and `PAUSED` and `CANCELLED` reachable from any working state. There is no fixed number of rounds: the authority decides when it has enough evidence. Limits only pause a task for the human; they never pick a winner. `maxDeliberationRounds` is a checkpoint interval, not a limit. Full detail: [docs/task-lifecycle.md](docs/task-lifecycle.md).

`FAILED` is still a state, but nothing in this version moves a task to it: an agent failure drops that agent or pauses the task, and a run that ends on an error pauses it. A task an older version left `FAILED` can be brought back with `R` (or `:retry`).

## 5. Message lifecycle

Every interaction is one `domain.Message` with a `Type` from a fixed vocabulary (`TASK_ASSIGNMENT`, `DELEGATE`, `QUESTION`, `ANSWER`, `INFORMATION`, `PROPOSAL`, `REVIEW`, `CHALLENGE`, `APPROVE`, `REJECT`, `REQUEST_CHANGE`, `IMPLEMENT`, `IMPLEMENTATION_RESULT`, `TEST_RESULT`, `FINAL_DECISION`, `STATUS_UPDATE`, `ERROR`). A message goes: **agent replies -> adapter parses the reply header -> orchestrator validates (identity, state, authority, routing) -> recorded, with a queued delivery for each recipient, in one transaction -> event emitted -> handed to the recipient in its next turn -> task state updated if the type implies a change.** Messages from the orchestrator itself (reminders, checkpoints, notices about refused or unreadable replies) are sent as `orchestrator`, never as the human. Full detail: [docs/agent-protocol.md](docs/agent-protocol.md).

## 6. Authority model

Authority is enforced in code by `internal/orchestrator/authority.go`'s `AuthorityManager`, not by asking agents to behave:

- `MessageType.RequiresAuthority()` marks seven types as authority-only: `FINAL_DECISION`, `APPROVE`, `REJECT`, `REQUEST_CHANGE`, `IMPLEMENT`, `DELEGATE` and `TASK_ASSIGNMENT`.
- `AuthorityManager.Authorize` checks every agent message against the task record before it is recorded or delivered. A supporting agent sending an authority-only type gets an `AUTHORITY_VIOLATION`, and the refusal says what to send instead (a `REVIEW` with a verdict, a `QUESTION`, a `PROPOSAL`).
- An `IMPLEMENT` addressed to a read-only agent is refused, naming the agents that can write.
- The sender's claimed identity is never trusted: a message whose sender differs from the adapter that produced it is refused as an `IDENTITY_VIOLATION`.
- A refused message is **not silently dropped**: it is recorded as an event, the agent is told why, and the task continues.
- The human (`human`) can always act, whichever agent holds authority: pause, resume, retry, cancel, approve, reject, instruct, change the authority. The orchestrator's own notes (`orchestrator`) pass the same way; neither name can be an agent's.

`TestAuthorityViolationIsRejected` (`internal/orchestrator/orchestrator_test.go`) drives a rogue agent that attempts an unauthorised `FINAL_DECISION` and a delegation to a non-participant, and asserts both are refused while the task keeps running.

## 7. Shared knowledge

`internal/knowledge` stores durable task context: the task itself on creation, `ANSWER` and `CHALLENGE` content as investigation entries, `TEST_RESULT` and `IMPLEMENTATION_RESULT` content, and every decision as it is recorded. Up to six relevant entries (`Store.ContextFor`) are attached to each delivery and shown to the agent as `RELEVANT PROJECT KNOWLEDGE`. Knowledge is stored with everything else (SQLite by default); search is a substring scan. The `knowledge:` settings section is still accepted so older files load, but it has no effect.

## 8. Agents communicate only through the orchestrator

Agents never send each other anything directly. Each agent turn is its own CLI process, started by the orchestrator's adapter for that agent with a prompt the orchestrator wrote; the orchestrator is the only component that reads an agent's reply.

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
    |                             |  Route(): record, queue     |
    |                             |  the delivery, emit event   |
    |                             |  codex free: next turn ---->|
    |                             |                              (a new codex process,
    |                             |                               prompt on stdin,
    |                             |                               its session resumed)
```

What agents do share is the **workspace folder**: every agent of a task runs in the same project folder, and a write-enabled agent's edits are visible to the others. There are no per-agent copies or worktrees; builds go to `build/<agent>/` so outputs do not mix.

## 9. How Claude Code and Codex plug in

`internal/agents/claude` and `internal/agents/codex` drive the real CLIs through `core.OneShotRunner` (`internal/agents/core/oneshot.go`): one process per turn, resolved on `PATH`, arguments always a `[]string` (never a shell string), the prompt written to stdin, and a time limit per turn (`responseTimeout`, 5 minutes, for a read-only agent; `writeResponseTimeout`, 20 minutes, for one that may write). An agent's turns on a task run one at a time; several waiting messages go over in one turn.

**Claude Code:** `claude [configured args] --verbose -p --output-format stream-json --include-partial-messages --setting-sources "" --strict-mcp-config [--settings <file>]`, then the permission flags (read-only: `--allowedTools Read,Grep,Glob --disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit --permission-mode dontAsk`; write-enabled: `--permission-mode acceptEdits` with allow and deny rules from the access level), `--model`, the session (`--session-id <uuid> -n <name>` on the first turn, `--resume <uuid>` after) and `--max-budget-usd <allowance>` when the task has a cost limit. Reply header: `@@ORCH type=<TYPE> to=<agent-id>`.

**Codex:** `codex exec [configured args] [--model M] --json --sandbox <read-only|workspace-write> --skip-git-repo-check --color never --output-last-message <file> [write confinement] -c mcp_servers={} [--cd DIR] -`, where a write-enabled agent's confinement is `-c` overrides that turn off temp-folder writes and network access; on later turns `codex exec resume ... -c sandbox_mode="<mode>" ... <thread-id> -`. The reply is read from the `--output-last-message` file, never scraped from stdout. Reply header: `##CODEX TYPE=<TYPE> TO=<agent-id>`.

Both adapters share one reply parser (`core.ParseReply`); the two markers differ, the parsing rules do not. An agent with nothing to add may reply `NO_REPLY`.

**The tool hook.** With real agents, ritam runs a review server on `127.0.0.1` (`internal/review`), and each CLI is told to run `ritam hook pretool` before every tool call. Claude Code gets it in the `--settings` file: matcher `*`, ritam's absolute path with its arguments apart (no shell), and `onFailure: block`, since Claude otherwise runs a call whose hook failed. Codex gets `-c hooks.PreToolUse=[...]` and `--dangerously-bypass-hook-trust`, which is withheld when the workspace defines Codex hooks of its own. Each turn gets a token, passed to that CLI process only (`RITAM_HOOK_URL`, `RITAM_HOOK_TOKEN`). A call that cannot be checked is refused: no token, ritam not answering, an unreadable or over-1-MiB call. Every other call goes through [the review](#the-review-of-tool-calls) and is recorded as a `ToolCall` event (events tab), or `ToolBlocked` when refused; a passed call still meets the CLI's own permission rules. After a finished turn, the tool calls in the CLI's output are compared with those the hook saw, and any that got past it raise a `HookMissed` event: a settings file Claude cannot parse is dropped without a word, hooks and all. Claude's side was checked live on 2.1.330 (`RITAM_LIVE=1 go test -run TestLiveEveryToolCallGoesThroughTheHook ./internal/agents/claude/`); Codex's has a matching live test that needs an account.

**What has been run live.** Earlier versions completed tasks against live Claude Code processes, and a three-agent run with a Claude authority, a Claude implementer and a Codex implementer had Codex replies parsed, routed and accepted. Running them found two defects that are fixed: a multi-line prompt passed as an argument to Windows' `codex.cmd` shim was cut at its first newline (the prompt now goes on stdin), and configured `args` could override the sandbox (only an allowlist of flags now passes). The test suite drives no real CLI: it uses the mock and scripted fake agents and stored samples of CLI output, so the changes listed as implemented above are covered by tests, not by a fresh live run.

## 10. How the proxy will eventually fit

`internal/proxy` defines the seam for a future local LLM proxy (`ports.LLMProxy`). **The proxy is not the orchestrator.** Its job, once built, is provider traffic only: wire-format translation, request logging, token accounting, context optimisation. The orchestrator has no reference to it. Today `Proxy.Start` returns `ports.ErrNotImplemented` when enabled in the settings (it is off by default) and ritam logs a warning and carries on.

## 11. Running

Prerequisites: Go 1.26.4 or later on `PATH` (the version `go.mod` names). No other service, database server or API key is needed; ritam starts with mock agents. For `-real`, install `claude` and/or `codex` and log in with each once (`claude /login`, `codex login`).

ritam works in the folder it is started in, the **project**. It does not need git and never runs git itself. A project may or may not be a git repository; Codex is started with `--skip-git-repo-check` so it runs in either. Where `.git` exists, its settings and hooks are among the files agents may not change (see [Protected files](#safety-what-a-write-enabled-agent-may-do)), and agents are refused the commands that rewrite git history or point git elsewhere. ritam makes no commits, branches or worktrees.

```powershell
# from the repository root
go run ./cmd/ritam                  # terminal UI in the current folder
go run ./cmd/ritam -headless        # no TUI: run one task and print its event log
go run ./cmd/ritam -config path\to\project\ritam.yaml   # that file's folder is the project

# force one mode regardless of the settings:
go run ./cmd/ritam -real                                   # real Claude Code / Codex CLIs
go run ./cmd/ritam -mock                                   # scripted mock agents
go run ./cmd/ritam -real -headless -task "your question"   # one real task, headless
```

- **`-headless`** runs one task with every agent on the team and prints its event log, final state and decisions. With mock agents and fewer than two on the team, it first recruits `demo-lead` (authority) and `demo-helper`, **and keeps them on the project's team** (`.ritam/team.json`). The authority is the first team agent whose role is `AUTHORITY`, else the first agent. With real agents it needs two recruited agents.
- **`-task "..."`** (with `-headless`) is the task to run instead of the built-in one.
- **`-real` / `-mock`** choose the agents for this run, over the settings; they cannot be combined. Whenever real agents are used, `orchestrator.idleTimeout` is raised to at least 10 minutes.
- **`-demo`** (and `tui.demoOnStart: true`) does not work in this version: it asks to create a task whose only participant is `orchestrator.defaultAuthority` (unset by default), and task creation refuses a task with no authority or with only one participant. The error shows in the status line and no task is created. Create the task with `n` instead.

### The screen

```
┌──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┐
│ RITAM │ TASK-001 test                                                                           ● PAUSED  │ 31 msgs │ 139.2k tok │ $4.18 │
│ PAUSED  TIMEOUT: no agent activity   r  resume   i  instruct                                                                             │
└──────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────────┘
┌─ TASKS ──────── 2 ─┐ [conversation] activity events decisions team settings                ┌─ AGENTS ──────────────────────── 1 working ─┐
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

- **Header.** The task, its state, elapsed time, messages, tokens and cost. With a daily limit set it also shows **today's spending** on this machine, over every task and project (`today $3.20 of $50`, or tokens when only `maxDailyTokens` is set), in the warning colour from 80% of the limit, and `today: spend record unreadable` when the record cannot be read (tasks then pause before their next turn). When something needs you, a second row says what and names the key: a decision waiting for you (`a` approve, `x` reject, `i` amend and instruct), a pause and why (`r` resume), a failed task (`R` retry), an agent that owes you an answer, or that every agent is a scripted mock.
- **Tasks (left).** Every task, persisted across restarts. A task whose run waits for a free slot (`maxConcurrentTasks`) is marked `queued`; pausing or cancelling it withdraws the run.
- **Centre.** `tab` / `shift+tab` (or `>` / `<`, or a click) switch between **conversation** (one entry per message, plus the events that change what you should do), **activity** (each agent's raw live output), **events** (the task's audit log), **decisions** (every decision record, superseded ones included), **team** and **settings**.
- **Right column.** **Agents** (one line each, numbered for `1`-`9`), **Decision** (the latest; yellow while it waits on you) and the **Inspector**, which shows whatever you last selected in full, messages rendered as markdown. `enter` moves focus to it; `v` zooms it to the whole screen; `v` or `esc` returns.

The layout drops columns rather than squeezing them: below 130 columns the task list becomes a centre tab, below 96 the agents and the inspector do too. Identity is **sigil plus colour** (`[C] opus`, `[O] codex`), never colour alone; set `NO_COLOR` or `RITAM_ASCII=1` for a plain-ASCII, monochrome screen. Tokens and cost are what the CLIs report, and read `--` until they report something; Codex reports tokens but not cost.

### Keys

| Key | Does |
|---|---|
| `tab` `shift+tab` `>` `<` | switch the centre view |
| `]` `[` | move keyboard focus between panes: tasks, centre, agents, decision, inspector |
| `j` `k` `↑` `↓` | move the selection in a list; scroll text |
| `PgUp` `PgDn` · `g`/`Home` `G`/`End` | page · jump to the oldest / newest |
| `enter` | open the selection in the inspector; in the inspector, zoom |
| `v` · `esc` | zoom the inspector · back out of zoom or a focused agent |
| `n` | new task (the form: title, description, Recruit, Participants, authority, Builder) |
| `p` · `r` | pause · resume |
| `c` | cancel the task for good; asks first |
| `R` | retry a `FAILED` task: same agents, their sessions kept |
| `a` | approve the decision waiting on you; asks for an optional note |
| `x` | reject the decision waiting on you; asks for a note, which is required |
| `A` | the tool calls waiting for your approval: `y` allow, `n` refuse, `esc` later |
| `i` | instruct an agent; on a completed task, a follow-up; on a held decision, your answer |
| `{` `}` | previous / next task |
| `d` `e` | jump to the decisions / events view |
| `J` `K` | move the agent selection from any pane |
| `1`-`9` · `0` | expand that roster agent's live output over the centre · back |
| `f` `t` `h` | follow the newest entry · show/hide thinking · help bar |
| `:` `/` `?` | command line · filter by agent · help |
| `q` `ctrl+c` | quit; asks first while tasks are running |
| mouse | click a tab, task, agent, message or decision; the wheel scrolls what is under the pointer (hold Shift to select text) |

**Confirmations.** Cancel, and quit with tasks running, ask first: `y` or `enter` confirms, `n` or `esc` keeps things as they are. `a` and `x` do nothing (and say so) unless a decision is waiting on you.

**Pasted text never runs as keys.** The native Windows console delivers a paste as one key per character. Keys arriving less than 15 ms apart are taken as a paste: in a text field `enter` and `tab` type a space instead of submitting, a confirmation is not answered by them, and quit, cancel, approve and reject are ignored, as they are for 150 ms after a text field closes.

### Commands (`:`)

```
instruct <agent> "..."           send an agent a message (no text: opens the prompt)
pause [reason]                   stop the task's agents mid-turn; r resumes
resume                           continue the task
retry                            bring a FAILED task back (R)
raise <messages N | budget $N>   allow N more messages over the task's life, or $N more spending
cancel [reason]                  end the task for good (asks first)
approve [note]                   accept the decision waiting on you
reject [note]                    refuse it, saying what must change (no note: asks for one)
task new "..."                   create and start a task with every team agent; authority: the
                                 selected task's, else the first agent
filter <agent|all>               show only messages from or to one agent
follow <on|off>                  pin the conversation to its newest entry
thinking <on|off>                show or hide agent reasoning
tab <name>                       switch the centre view
inspect <agent>                  point the inspector at an agent
authority <agent>                hand the task's authority to another participant
models                           list every model you can recruit (the team tab)
recruit <provider> <model> [name] [role] [writes]
                                 add an agent to the team; read-only unless "writes" is given
dismiss <agent>                  remove a recruited agent
check [provider model]           check a model runs on your account (one short paid call)
review [auto|manual]             who approves the agents' tool calls (see the review of tool calls)
settings                         the workspace's access level and command list
access <0-3>                     set the access level (raising it asks you to type yes)
allow | disallow <command>       add a command to the list · take one off
export jsonl [path]              write the events this UI has seen to a file
help                             the key and command reference
quit                             leave ritam (asks first while tasks are running)
```

`:raise` raises only: `messages N` sets the task's `maxTotalMessages` ceiling to N above the larger of the current ceiling and what it has used; `budget $N` does the same for its cost limit. It does not resume the task; press `r`. A task with no such limit has nothing to raise.

Text fields (the instruction prompt, the command line, the new-task form) show everything you type, wrapped. `ctrl+a` selects the whole text (typing replaces it, backspace clears it), `ctrl+u` clears the field and `ctrl+w` deletes the last word.

`export jsonl` writes the TUI's own buffer, capped at 500 events, not the durable log; the full history is in `.ritam/ritam.db`.

### Agent sessions and follow-ups

Each agent keeps its own Claude Code / Codex conversation per task and resumes it on every turn (`claude --resume <id>`, `codex exec resume <id>`), so it remembers the deliberation instead of being re-briefed from scratch.

- **Which session is whose.** One session per (task, agent) is recorded in `.ritam/sessions.json`. Claude sessions get an id ritam chooses (`--session-id`); Codex announces its thread id on the first turn. The CLIs' "most recent session" shortcuts are never used. An empty or corrupt `sessions.json` is kept aside as `sessions.json.bad-<time>`, start-up says so, and the agents start new sessions.
- **Same place, same permissions.** Every turn runs where its session was created, and a session made outside the workspace is never resumed. Read-only permissions are passed again on every turn; for Codex, whose `exec resume` has no `--sandbox` flag, with `-c sandbox_mode=...`.
- **Compaction.** Every resumed turn carries a short standing brief (who the agent is, its role, the task statement, who it may address, the rules and the reply format), so a CLI summary that dropped them cannot break its replies.
- **Lost sessions.** If a CLI no longer has a session, a new one is started and the agent is re-briefed with a transcript of the task so far.
- **Follow-ups.** Press `i` on a completed task: it is reopened, the agents resume their sessions, and your message is delivered. A new `FINAL_DECISION` supersedes the old one, which stays on record. A follow-up that only asks something completes again once it is answered; one in which the authority hands out new work (`TASK_ASSIGNMENT` or `IMPLEMENT`) ends with a `FINAL_DECISION`, and the authority is told so. Instructing a paused task asks you to resume it first.

### Where ritam keeps its data

- **The app home**, one per user: `%LOCALAPPDATA%\ritam` on Windows, `~/.ritam` elsewhere, or `$RITAM_HOME`. It holds `ritam.yaml` (the global settings), `logs/` (one log per project), `projects.json` (the known projects and the access levels you approved), `backups/<project>-<id>/`, `archives/`, `models.json` (what was learned about models on your account), `spend.json` (each day's spending, over every project), `canary/` (sandbox checks), `rejected/` (agents' versions of protected files that were put back) and `workspace/` (where agents work after a launch at a global location).
- **The project's hidden folder**, `<project>/.ritam`: `ritam.db` (the database), `team.json` (the project's team), `sessions.json`, `ritam.yaml` (the project's settings), `project.json` (the project's id, so a moved project keeps its backups) and `tmp/` (the agents' scratch files).

**Former name.** ritam was once called `orch`. At start, an `orch` app home or `.orch` project folder is renamed to the ritam name only if it holds this program's files (a project list; a `project.json` or SQLite database), and each project is checked once, which the project list records. A project with both folders gets a warning naming them, and ritam starts. `ritam rename -from <old>` does the same for any other former name.

### Settings

Settings are read in layers, each overriding the one before: the defaults, the app home's `ritam.yaml`, then the project's (`ritam.yaml` in the project folder, then `.ritam/ritam.yaml`). A single setting overrides just that setting; a list (`access.commands`, `allowedCommands`, `args`) is replaced whole; a provider entry is merged field by field; an `agents:` entry replaces the whole agent. `ritam.example.yaml` documents every key.

- **Read strictly.** A key ritam does not know stops it starting, naming the file, the line and the nearest valid key (`maxDailyCostUsd` -> "did you mean maxDailyCostUSD?"). Values are validated together and every problem is reported: negative limits, timeouts or costs, `backups.keep` below 1, a `backups.interval` under 1 minute other than 0 (0 turns periodic snapshots off), `deleteAfterDays` below 1 with `whenProjectDeleted: delete`, an unknown log level or format.
- **`ritam config check`** lists the settings files in the order they are read, every setting in force, where each non-default value came from (the global file, a project file, a path set at start, the real-agent idle floor), and what was held back from the project's files.
- **A project's files cannot decide what runs on your machine.** They travel with a clone or copy, so they may tighten anything but may not: choose the programs ritam runs (an `executable`) or add `args`; give an agent or a provider write access the global settings do not give; switch `tui.useMockAgents` off (only `-real` or your global settings can); add commands your global settings do not list; point the workspace, log, database or sessions outside the project, turn `confineReads` off or allow a global workspace; loosen a limit (spending, messages, duration, response timeouts, `requireHumanApproval`); set `access.allowUnsandboxed`; or change `backups` or the `proxy`. Each is put back to the global value and named in a notice at start.

Commonly changed settings (`orchestrator:` section unless noted; defaults in brackets): `maxConcurrentTasks` [1], `maxDeliberationRounds` [10, checkpoint interval], `maxMessages` [200], `maxTaskDuration` [30m], `stallMessages` [8], `maxTotalMessages` [1000], `maxTaskCostUSD` [10], `maxTaskTokens` [5000000], `maxDailyCostUSD` [50], `maxDailyTokens` [25000000], `requireHumanApproval` [false], `idleTimeout` [60s; at least 10m with real agents], `maxRateLimitWait` [2m]; `providers.<claude-code|codex>.responseTimeout` [5m], `.writeResponseTimeout` [20m], `.allowWrites` [unset, meaning yes], `.allowedCommands`; `storage.backend` [sqlite]; `access.level` [0]; `review.mode` [auto], `review.askTimeout` [5m]; `workspace.confineReads` [false]; `tui.useMockAgents` [true]; `backups.*`.

### Storage and restarts

- **One SQLite file per project**, `.ritam/ritam.db` (pure Go, no server): tasks, messages, events, decisions, disagreements, knowledge, the delivery queue and the record of every agent turn. `storage.backend: memory` keeps nothing.
- **Writes that belong together are one transaction**: a message and its deliveries, a decision and the one it supersedes. Each task row has a version, and an update written from a stale copy is refused and redone on a fresh read, so the run loop and the UI cannot undo each other's changes.
- **Queued messages survive.** A message waiting for a busy agent is a queued delivery in the database. It is marked sent with the turn that carries it, acknowledged when that turn ends well, and queued again when the turn is killed or fails. A run starts by refilling the inboxes from the queue, so a pause, a killed turn or a restart loses nothing; an agent whose earlier turn was cut off is told it may have seen part of the batch.
- **Every turn is recorded**: which messages it carried, when it started and ended, how it ended (completed, failed and why, killed) and what it cost (`Service.TaskTurns`; the UI does not show it yet).
- **Tasks running when ritam exited come back `PAUSED`** with the reason `INTERRUPTED`. Resume (`r`): queued messages are delivered, and each agent that owes a reply is asked for it again; if nobody owes one, the authority is told to carry on.
- **One ritam per database.** A second ritam on the same file refuses to start and names the pid holding it. The lock is an OS lock, so a crashed ritam never leaves it stuck.
- **Inspecting it.** Messages and events have ordinary columns, so any SQLite tool can read the history; other records are JSON in a `data` column. The schema version is `PRAGMA user_version`; a build refuses a database newer than it understands.

**Backups.** Each project's database is snapshotted into the app home when a task finishes, when ritam exits, every `backups.interval` (10 minutes) while work is going on, and before a schema upgrade. Every command that opens the database (the TUI, `-headless`, `backups now`, `restore`, `archive`) takes that pre-upgrade backup when backups are on. A snapshot is skipped when nothing changed; the newest `backups.keep` (5) are kept, and the newest is never pruned. Snapshots use SQLite's `VACUUM INTO`, so they are consistent while ritam runs.

**Deleted projects.** If a project's hidden folder is deleted, ritam finds the project again by its path, keeps its id, and offers to restore the latest backup. If a project folder is deleted, ritam says so at start (`backups.whenProjectDeleted: ask`) and keeps its backups until you choose, or keeps or deletes them automatically (`keep`, or `delete` after `backups.deleteAfterDays`). A project on a drive or share that is not mounted is not taken for deleted.

**Commands** (run in the project folder; those that change data need ritam closed there):

| Command | Does |
|---|---|
| `ritam where` | shows the app home, the settings files, the project, the database, the log and the backups folder |
| `ritam config check` | shows the settings in force and where each came from |
| `ritam backups` / `backups now` | lists this project's backups / takes one now |
| `ritam backups projects` | lists every known project, its backups, and whether it still exists |
| `ritam backups delete <project>` / `keep <project>` | deletes a project's backups / keeps a deleted project's backups without asking again |
| `ritam restore [latest\|<file>]` | restores this project from a backup or an archive; what it replaces is backed up first |
| `ritam archive [-clear]` | zips this project's tasks, team and sessions into the app home; `-clear` then starts the project fresh |
| `ritam rename -from <old>` | renames the app home and known projects' hidden folders made under an older name |
| `ritam hook pretool` | run by the agent CLIs before each tool call, with the call on stdin (see [the tool hook](#9-how-claude-code-and-codex-plug-in)); not for people |

### Long tasks, limits and spending

- **Supporting agents can consult each other** with a `QUESTION`; the answer comes back to the asker. The conversation and the inspector show who waits on whom.
- **Whoever can answer you, does.** An agent handed part of your question is told it is yours and may answer you directly. When everyone goes quiet with a request open, the agent that owes the reply gets one reminder (starting from the end of the chain). An agent still silent after it has that request closed as unanswered and recorded; if your own question stays unanswered, the task pauses naming it.
- **Verdicts, not approvals.** A supporting agent records agreement or objection with `REVIEW verdict=approve|changes|reject`. Only the authority's `APPROVE`, `REJECT` and `FINAL_DECISION` change a task.
- **Only real answers close a request.** The orchestrator's own notes never answer or open a request, and a clarifying `QUESTION` or a `STATUS_UPDATE` back does not close a `DELEGATE`.
- **Agents work until done.** The limits meter autonomous agent work: your input (an instruction, a follow-up, a resume, a rejection) starts a fresh allowance, and messages to or from you never count. Every `maxDeliberationRounds` authority-led rounds a **checkpoint** asks the authority for a progress `STATUS_UPDATE` and notifies you; work continues. What pauses a task:
  - a **stall**: `stallMessages` agent messages in a row with no progress (repeats, remarks that answer and produce nothing; unreadable replies count too);
  - long **unattended** work: `maxMessages` or `maxTaskDuration` since your last input;
  - the **budget**: `maxTaskCostUSD` or `maxTaskTokens`, from the usage the CLIs report;
  - the **day's spending**: `maxDailyCostUSD` or `maxDailyTokens` over every task and project on this machine;
  - the `maxTotalMessages` ceiling over the task's life, which only `:raise messages` lifts;
  - an agent failure the authority cannot get past (below), a run that ends on an error, or agents that cannot be started.
- **Idle.** `idleTimeout` pauses a task only when no agent is working and no retry is waiting, and nothing has been said or streamed for that long. A working agent is bounded by its turn's time limit instead.
- **Every turn is billed**, the one that ends the run and those killed by a pause, a cancel, a completion or quitting included. Codex reports tokens but not cost.
- **Spending is shared out before each turn.** When the task has a cost limit or the day has one, each Claude Code turn gets an allowance when it is handed over: what is left of the task's budget and of the day's, minus what co-workers' turns in progress hold, shared among the agents that could start at the same time, at least $0.05. It is passed as `--max-budget-usd`. With less than that left, the turn waits for co-workers' turns to finish, or the task pauses. A retried turn takes its own share. *Unverified:* this assumes `--max-budget-usd` counts only the turn's spending on a resumed session; if the CLI counts the session's running total, resumed turns would stop at their first call (see the comment above `budgetArgs` in `internal/agents/claude/adapter.go`). Codex turns get no dollar cap.
- **The day's record** (`spend.json` in the app home) uses local calendar days and an OS file lock, so several ritam instances add to it without losing anything. A record that cannot be read or written fails closed: the task pauses saying why, and a corrupt file is kept aside as `spend.json.bad-<time>`.
- **Raising limits.** A task paused at its message ceiling or budget can be given more with `:raise messages N` or `:raise budget $N`, then resumed.

### When an agent's turn fails

Adapters report what kind of failure a turn had; the orchestrator acts on the kind, never on the error's wording.

| Failure | What happens |
|---|---|
| unreadable reply (no header, or no block that can be delivered) | the agent is shown what it sent and the exact header to write, with this task's ids, and asked again. A second unreadable reply in a row: see "give up" |
| part of a reply unreadable | the readable blocks are delivered (an unknown type as `INFORMATION`, a block with no recipient dropped) and the agent is told what to fix |
| the turn ran past its time limit | sent again at once, the agent told it was cut off; a second timeout: give up |
| crash, overloaded or server error, lost session | sent again after 10 s, 30 s and 90 s; then give up |
| usage limit that resets within `maxRateLimitWait` (2m) | waited out and sent again, up to three times; then give up |
| usage limit that resets later | the task pauses, saying when; resume after then (nothing resumes it for you) |
| usage limit with no reset time | for the authority, the task pauses; a supporting agent is dropped |
| not logged in, out of credits, model not in the plan, unknown model | a supporting agent is dropped; for the authority, the task pauses. Credits, plan and unknown-model failures mark the model unavailable in `models.json`, so the form will not offer it |
| the CLI stopped the turn at its spending cap | the task pauses for its budget |

To **give up** on a supporting agent is to **drop** it from the task: it receives nothing more, the requests it owed are closed, and the authority is told once to carry on and reassign its work. To give up on the authority pauses the task with the reason. Dropped agents are recorded on the task, survive a pause, and are put back when you resume (or retry) it. A task never fails over one agent.

### Decisions that wait for you

A `FINAL_DECISION` is **held for you**, with the task at `AWAITING_DECISION`, when `requireHumanApproval` is on or when an agent is waiting for your answer (to "run the build and send me the output", say). The run ends while it waits, so it does not hold a slot. Then:

- **Approve** (`a`, `:approve`): the decision is accepted and the task completes. It works on a task paused while its decision waited too.
- **Reject** (`x`, `:reject`, a note required in the UI): the decision is marked rejected with your note, the disagreements it settled are reopened, the task goes back to `DELIBERATING`, and the authority is sent your note as a `REJECT`. A run is started to deliver it.
- **Answer** (`i`): your message goes to the agent you choose, and the authority is told its decision still waits and to confirm it or send a revised one; a new `FINAL_DECISION` supersedes the held one. A run is started to deliver them.

A held decision survives a pause and a restart: the decision is stored as proposed, and a restart leaves `AWAITING_DECISION` tasks as they are. Resuming a task that only waits on you starts nothing.

Two other gates keep a decision from cutting work short. A `FINAL_DECISION` sent while another participant is in the middle of a turn, about to retry one, or still owes a report on an `IMPLEMENT` or `REQUEST_CHANGE` is **not taken up**: it is recorded, delivered to nobody, and the authority is told what is outstanding and to decide again. An open `DELEGATE` or `QUESTION` does not hold it; it adds a caveat to the decision's evidence instead. And a decision accepted while you still have a question open keeps the task open until that question is answered (or, after a reminder, records that it was not).

### Delivery: one turn for everything waiting, and the team in view

Each agent turn is a CLI call of a minute or more, so messages wait in the recipient's inbox and are handed over when it is free:

- **Combined.** Everything waiting reaches the agent as one turn, oldest first, your messages marked to be answered first.
- **Held while the agent waits on a co-worker.** An agent that asked another for something, while that one still works on it, is not woken by the chatter in between. Your messages, and requests addressed to it, always go straight through, and a hold never lasts more than four minutes.
- **The team in view.** Every delivery opens with a roster (each agent's id, provider and model, role, whether it may write files and build, its state and its spending, columns that are the same for everyone dropped) and the board: who is working, how many messages each has unread, who waits on whom. Every agent can read it; only the authority may act on it.
- **Durable.** See [Storage and restarts](#storage-and-restarts).

### The team: recruiting agents and choosing models

The **team** tab lists the team and every model the providers offer: Claude Code's `/model` tiers, and Codex's from `codex debug models` (its cache as fallback). Select a model, press `enter`, name the recruit: it joins the team **read-only**. `:recruit claude-code opus opus-2 reviewer writes` recruits one that may write.

- **Choose the team while creating a task.** The new-task form (`n`) starts with nobody on it. **Recruit** lists every model: up/down to move, space to tick, left/right or a digit for how many agents of it. **Participants** is built from that, every agent named after its model (`opus-1`, `opus-2`, `sonnet-1`); an agent of that name already on the team for the same model is used again. You tick the **authority**; nobody is pre-selected, and a task needs an authority and at least two participants. On create, the agents not yet on the team are recruited **able to write** (within the workspace) unless their provider says `allowWrites: false` or ritam was launched at a global location.
- **Models your account cannot run** are recognised (see the failure table) and, for credits, plan or an unknown model, marked unavailable in the app home's `models.json`; the form will not let you pick them. `c` in the form's Recruit field, or `:check` in the team tab, makes one short paid call to check a model. In mock mode a check calls nothing and records nothing.
- **The same agent, many tasks.** Each task gets its own instance of each of its agents, with its own CLI processes and session.
- **Recruits are kept** in `.ritam/team.json`. `:dismiss` removes a recruit, not while it works on a running task. The names `human` and `orchestrator` are reserved.
- **Changing the authority.** `:authority codex` hands the selected task's authority to another participant; the agents are told.

### Watching the agents work

An agent working on its turn has a `thinking…` entry in the conversation; the message it posts replaces it. Select one to see its live output in the inspector. Both adapters stream while they work: Claude token deltas (thinking blocks and tool use included), Codex item-level events (reasoning steps, commands, the final message). This output is presentation only: never persisted, never routed, never acted on.

**Which agents am I running?** The roster marks scripted agents `mock`, the inspector names each agent's CLI, and the header shows `MOCK AGENTS` when every agent is scripted. Mock replies are a fixed script that ignores your task. `tui.useMockAgents` defaults to `true`, so a fresh start never reaches a real CLI; `-real` or `useMockAgents: false` in your global settings changes that, and a project's settings cannot.

### ritam.yaml says how to run the providers, not who the agents are

```yaml
providers:
  claude-code:
    executable: claude         # on PATH, or an absolute path
    responseTimeout: 5m        # one turn of a read-only agent
    writeResponseTimeout: 20m  # one turn of an agent that may write or run commands
    allowWrites: true          # false: this provider's agents are read-only
    allowedCommands: []        # extra commands, besides access.commands
  codex:
    executable: codex
```

Both entries are optional. The team is recruited in the TUI and kept in `.ritam/team.json`; an older `agents:` section still loads. No API key is read from the settings, the environment or anywhere else by ritam: the CLIs use their own logins.

### Safety: what a write-enabled agent may do

**The default.** Agents recruited from the new-task form, and providers that do not set `allowWrites`, **may edit files**. What protects you:

- the scripted mock agents are the default, and a project's settings can neither switch them off nor grant write access;
- writes are confined to the **workspace**: the folder holding `ritam.yaml` (or `workspace.root`). Claude Code edits with `--permission-mode acceptEdits` and no allow rule for its edit tools, so an edit anywhere else would need a permission nobody is there to give, and is refused; Codex runs in its `workspace-write` sandbox with temp folders and the network turned off. A `workingDir` must lie inside the workspace, links and junctions resolved first;
- launched at a global location (a drive root, your home folder, a system folder, AppData, temp), agents work read-only in the app home's `workspace/` folder; `workspace.allowGlobal: true` lets them write there;
- the **access level** (below) decides what commands they may run; level 0, the default, runs none for Claude Code;
- **protected files** are guarded (below).

Every tool call is checked by [the review](#the-review-of-tool-calls) before it runs, but nothing captures what each turn changed, so keep write-enabled real agents in a run you supervise, and use a version-controlled project folder if you want to see or undo their changes.

### The review of tool calls

With real agents, every tool call goes through ritam's hook to the review (`internal/review`) before the CLI runs it. The call is parsed, never run: bash with `mvdan.cc/sh`, PowerShell and cmd with parsers of ritam's own, file tools, Codex's argument lists and `apply_patch`. Wrappers are seen through (`sudo`, `env`, `xargs`, `find -exec`, `npx`, `bash -c`, `powershell -EncodedCommand`, `cmd /c`, `ssh host cmd`), as are quoting tricks (`r''m`, `X=rm; $X`, ``R`e`move-Item``) and `echo … | base64 -d | sh`. Each path the call touches is placed in a zone: the agent's build folder, the workspace, a protected file, ritam's own files, temp, home, the system, elsewhere, a share, a device, the registry, or unknown. The facts are checked against ritam's rules (`internal/review/rules/harmful-commands.yaml`, embedded, with every rule's own examples as tests) and the turn's history (a refusal tried another way, a script written then run, three refusals). The worst finding decides:

| Finding | What happens |
|---|---|
| critical | refused, and the task is paused (its turn is stopped) |
| harmful | refused; the agent is told the rule's reason |
| suspicious | put to you |
| info, none | runs in auto mode; in manual mode a shell command is put to you |

The **review mode** is set in the settings tab (`:review auto`, `:review manual`) and saved to the project's settings (`review.mode`). **Auto** (the default) lets ritam's rules decide: rules cannot foresee everything, so a destructive command they miss still runs. **Manual** also puts every command to you. The mode does not widen the access level: a command you allow still has to be one the level lets the agent run. A project's settings may choose manual, never auto over your global manual.

A call put to you opens a prompt at the bottom of the screen (when you are not typing): who wants to run what, and why it is asked. `y` allows it, `n` refuses it, `esc` leaves it for later and `A` brings it back. Nobody answering within `review.askTimeout` [5m] refuses it, and so does a `-headless` run, where nobody is there to ask. The agent is told why a call was refused, and refused calls show as failures in the activity stream.

**Read-only agents** (`allowWrites: false`, or a recruit without writes): Claude Code gets `--allowedTools Read,Grep,Glob --disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit --permission-mode dontAsk`; Codex gets `--sandbox read-only`. `workspace.confineReads: true` keeps Claude Code's file tools reading inside the agent's working folder; off, agents may read anywhere you can. Codex's sandbox reads everywhere.

**Access levels**, chosen in the settings tab (`:settings`, or `:access <0-3>`) and saved to `.ritam/ritam.yaml`:

| Level | | What agents may run |
|---|---|---|
| 0 | Files only (default) | edit files, run read-only commands; a build or test is asked of you |
| 1 | Sandboxed builder | only agents whose CLI passed the sandbox check run the listed commands; the others ask them |
| 2 | Build and test | every agent runs the listed commands (`:allow cmake`, `:disallow cmake`) |
| 3 | Any command | anything inside the workspace except the always-blocked list |

Raising the level asks you to type `yes`; lowering applies at once, from each agent's next turn. A level a project's own settings ask for is granted only once you approved it on this machine for that folder (recorded in the app home's `projects.json`); a clone, a copy or a moved project starts at 0. A global launch stays at 0.

Claude Code's sandbox exists on macOS, Linux and WSL2, and ritam turns it on there (no network, nothing retried outside it). On native Windows it has none, so by default **an agent with no verified sandbox runs no commands at any level**: at levels 2 and 3, as at level 1, only agents whose sandbox passed the check build and test. `access.allowUnsandboxed: true` in your global settings lets them run commands directly on this machine. Codex always runs its commands in its own sandbox, at level 0 too.

**Builders.** The new-task form's **Builder** field says who runs builds and tests: **Auto** (follow the access level), **Manual** (you), or one participant. The choice narrows the level, never widens it. Every agent that builds uses its own `build/<agent>/` folder; when several build, the authority hands out the work.

**The sandbox check.** Ritam checks an adapter's claim to run commands in a sandbox: the agent is asked to write one file inside the workspace and one in a folder of its own under the app home's `canary/`, and ritam judges the files, not the reply: the inside one must exist, the outside one must not. Each check uses its own folder, so another ritam starting meanwhile cannot affect it; a check whose folder vanished proves nothing; a start removes only canary folders older than an hour. The result is kept per model in `models.json` with the CLI's version, and runs when a task that could use the model as a builder starts, or with `:check`.

**What a write-enabled Claude Code agent may run.** No shell at level 0 (and at level 1 without a sandbox), the listed commands plus its `allowedCommands` as `Bash(<command>:*)` rules at level 2, any `Bash` command at level 3. Refused at every level: deleting, disks and system settings, scheduled tasks, permissions, killing processes, the network and downloaders, rewriting git history and git's settings, installing software, interpreters and shells given code on their command line (`python -c`, `node -e`, `bash -c`, `cmd /c`, `powershell -Command`), `find -delete`/`-exec`, `xargs`, `git -C` and similar, and the PowerShell tool; a deny rule wins over an allow rule. The configured command lists are also filtered through wrappers (`env`, `nohup`, `timeout`), a program's folder and extension, and every command in a chain, and a bare interpreter or shell is refused. None of this sees inside a script or a build file, which is why a sandbox is what makes running commands safe.

**What configuration cannot change.** ritam's own flags come after configured `args`, and only allowlisted flags pass: for Claude Code `--model`, `--effort`, `--fallback-model`, `--append-system-prompt`, `--betas`; for Codex `--model`/`-m`, `--enable`/`--disable`, `--color`, `--image`/`-i` and `-c` overrides of the model and its reasoning only. Claude Code is started with `--setting-sources ""` and `--strict-mcp-config`, Codex with `-c mcp_servers={}`, so your own CLI settings, hooks and MCP servers do not apply.

**Protected files.** Claude Code's file tools are denied ritam's own files (`.ritam/`, the workspace's `ritam.yaml`, the app home) and the workspace's files that run code later: `.git` (settings and hooks), `.github/workflows`, `.husky`, `.vscode/tasks.json`, `settings.json` and `launch.json`, `.claude/settings*.json`, `.mcp.json`, `.codex/config.toml`, `.envrc`, `CLAUDE.md`, `CLAUDE.local.md`, `AGENTS.md`. Shell commands and Codex's sandbox have no per-path rule, so after every agent turn ritam compares those files with what it last read or wrote: a changed file is put back, a new file in a guarded folder (a git hook) is taken away, the agent's version is kept in the app home's `rejected/` folder, and the task's events say so. The app home's shared files (`projects.json`, `models.json`, the global settings) are read again whenever agent turns in this process go from none to one, so another ritam's changes between turns are kept and only a change made during a turn is put back. The database is not guarded this way.

**What agents can say.** In what the next agent reads, a line that looks like the orchestrator's own framing (`=== ... ===`, `--- ... ---`) or the `[HUMAN]` mark is quoted after `> `. In the TUI, control characters and escape sequences in anything shown are dropped.

### Headless run

`go run ./cmd/ritam -headless` with mock agents prints this sequence: task created; `CREATED -> PLANNING`; one `TASK_ASSIGNMENT` to the authority only; `demo-lead -> demo-helper DELEGATE`; `-> INVESTIGATING`; `ANSWER`; `-> DELIBERATING`; `IMPLEMENT`; `-> IMPLEMENTING`; `IMPLEMENTATION_RESULT`; `-> REVIEWING`; `REQUEST_CHANGE`; `-> IMPLEMENTING`; `IMPLEMENTATION_RESULT`; `-> REVIEWING`; `demo-lead -> * FINAL_DECISION`; `-> AWAITING_DECISION`; `D-001` created and accepted; `-> COMPLETED`.

### Running tests

```powershell
go test ./...
```

The tests use mock agents, a scripted fake agent that fails the way the real CLIs do (timeouts, crashes, rate limits, malformed and empty replies, budget stops), stored samples of real-looking CLI output, and both storage backends held to the same conformance tests. None calls a real CLI or the network.

On some Windows machines an Application Control policy intermittently blocks a freshly built test binary; if a package fails to run for that reason, run `go test ./...` again.

## Directory structure

```
cmd/ritam/                entry point: flags, data commands, headless runner
internal/domain/          core types: Task, Message, Decision, Agent, Event, Turn, deliveries
internal/ports/           interfaces between the domain, storage, agents, proxy and the TUI
internal/orchestrator/    EventBus, AuthorityManager, TaskManager, MessageBus, inboxes,
                          failure policy, turn budgets, Orchestrator
internal/agents/core/     shared agent plumbing: registry, manager, one-shot runner, sessions,
                          reply parser, prompts, access policy, file guard
internal/agents/claude/   Claude Code adapter (@@ORCH)
internal/agents/codex/    Codex adapter (##CODEX)
internal/agents/mock/     in-process scripted agents (the default, tests, -headless)
internal/agents/fake/     scripted fake agent that fails like the real CLIs (tests)
internal/knowledge/       shared-knowledge store
internal/decisions/       decision lifecycle (propose, accept, reject, supersede)
internal/storage/         SQLite (sqlite*.go) and in-memory (memory*.go) repositories
internal/backup/          database snapshots, restore and archives
internal/layout/          app home, project folders, project registry, former-name migration
internal/fsutil/          file locking and durable writes
internal/brand/           the app's name and the names derived from it
internal/proxy/           seam for the future local LLM proxy (not implemented)
internal/app/             application service (ports.Service), team, wiring (wire.go)
internal/tui/             Bubble Tea terminal UI
internal/config/          settings loading, strict decoding, validation, project restrictions
internal/logging/         structured logging with credential redaction
docs/                     detailed documentation
test/, cpp_ledger/        sample C++ workspaces for trying agents on
ritam.example.yaml        example settings
```

## Constraints

- **Git is optional.** ritam neither needs nor runs git; this repository is kept in git, a project ritam works on need not be.
- **Local-first.** No cloud services are used or assumed, other than the ones the agent CLIs themselves call. Settings are local YAML files; there is no remote config source.
- **No database server.** SQLite, embedded and file-based (pure Go, no cgo).
- **No credentials in configuration.** Claude Code and Codex authenticate through their own CLI logins; neither the settings nor the proxy carry API keys.
- **Windows is the primary target.** Executables are resolved on `PATH` (Windows commonly uses shims), prompts go on stdin because `.cmd` shims re-parse the command line, and a turn's whole process tree is killed when it is stopped.
