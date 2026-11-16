# Driving Claude Code and Codex: what exists, what was run live, what remains

See also: [../README.md](../README.md), [architecture.md](architecture.md), [agent-protocol.md](agent-protocol.md).

The Claude Code and Codex adapters drive the real CLIs. This document describes how, what has been checked against the live CLIs, and, at the end, what is not built.

## 1. The transport: one process per turn, one session per agent

Both adapters run through `core.OneShotRunner` (`internal/agents/core/oneshot.go`). Each agent turn is one CLI process: the executable is resolved on `PATH` (both CLIs are often `.cmd` shims on Windows), the arguments are a `[]string` (never a shell string), the prompt is written to stdin, output is read as it streams, and the process must exit before its reply counts. A turn has a time limit (`responseTimeout`, 5 minutes, for a read-only agent; `writeResponseTimeout`, 20 minutes, for one that may write or run commands); on that limit, a cancel or a stop, the whole process tree is killed. An agent's turns run one at a time (`core.InvocationQueue`), and messages that arrive meanwhile go over together in its next turn.

**Why a process per turn.** These CLIs are built for a person at a terminal. A long-lived process can block on a permission prompt nobody is there to answer, and a wedged one looks like a slow one. A process that must exit gives a hard time limit, a clean retry boundary, and a crash that costs one turn.

**Memory across turns** comes from the CLIs' own sessions. Each (task, agent) has one session, recorded in `.ritam/sessions.json` and resumed on every turn: Claude with an id ritam chooses (`--session-id` first, `--resume` after), Codex with the thread id it announces (`thread.started`), resumed with `codex exec resume`. A resumed turn sends a short standing brief instead of the full one; a lost session is replaced and the agent re-briefed with a transcript of the task. Shared knowledge is attached to every delivery as well (see [agent-protocol.md](agent-protocol.md#7-what-a-delivery-carries)).

## 2. Claude Code (`internal/agents/claude`)

```
claude [configured args] --verbose -p --output-format stream-json --include-partial-messages
       --setting-sources "" --strict-mcp-config [--settings <file>]
       <permission flags> [--model M] (--session-id <id> -n "<name>" | --resume <id>)
       [--max-budget-usd <allowance>]
```

- **Output.** `stream-json` events give live activity (token deltas, thinking, tool starts); the reply is the final `result` event. Usage comes from that event: the token counts are the turn's own; `total_cost_usd` is the session's running total, so the turn is charged how far it grew. A turn killed before its result reports its tokens with no cost.
- **Permissions.** Read-only: `--allowedTools Read,Grep,Glob --disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit --permission-mode dontAsk`. Write-enabled: `--permission-mode acceptEdits`, an allow list of the read tools plus what the access level grants (`Bash(<cmd>:*)` rules, or `Bash` at level 3), and a deny list of refused commands, the PowerShell tool, `git -C` and similar, and `Edit(...)` rules for ritam's protected paths. There is no allow rule for the edit tools, so an edit outside the working folder would need a prompt and is refused.
- **`--settings`.** A file in the project's `tmp/` folder turning Claude Code's sandbox on where the platform has one (macOS, Linux, WSL2: no network, no unsandboxed retries), and `blockReadsOutsideWorkingDirectories` when `workspace.confineReads` is on. `--setting-sources ""` and `--strict-mcp-config` keep your own settings files, hooks and MCP servers out.
- **Budget.** `--max-budget-usd` carries the turn's allowance from the orchestrator (see [architecture.md](architecture.md#6-spending)); without one, what is left of the task's budget; with no cost limit, nothing. **Unverified:** this assumes the CLI counts only this invocation's spending on a resumed session. If it counts the session's running total, every resumed turn would stop at its first API call; the fix would be to pass the session's cost so far plus the allowance. A turn the CLI stops at its cap pauses the task for its budget.
- **Failures** are read from the result event (`subtype`, `is_error`, the error text), with stderr as the last resort, and reported by kind.
- **Configured `args`** pass only `--model`, `--effort`, `--fallback-model`, `--append-system-prompt` and `--betas`; everything else, short flags and stray positional arguments are dropped with a warning.

## 3. Codex (`internal/agents/codex`)

```
codex exec [configured args] [--model M] --json --sandbox <read-only|workspace-write>
           --skip-git-repo-check --color never --output-last-message <file>
           [write confinement] -c mcp_servers={} [--cd DIR] -
codex exec resume [configured -c/--enable/--disable] [--model M] --json --skip-git-repo-check
           -c sandbox_mode="<mode>" [write confinement] -c mcp_servers={}
           --output-last-message <file> <thread-id> -
```

- **The reply** is read from the `--output-last-message` file (created in the project's `tmp/` folder), never scraped from stdout. `--json` events on stdout give live activity, the thread id and usage.
- **Sandbox.** `read-only`, or `workspace-write` for a write-enabled agent, with `-c` overrides that exclude temp folders and turn network access off. `exec resume` has no `--sandbox` flag, so the mode is forced with `-c sandbox_mode=...` on every resumed turn. `-c mcp_servers={}` keeps your MCP servers out. `--skip-git-repo-check` lets Codex run in a folder that is not a git repository.
- **Usage.** Codex reports running token totals for the thread in `turn.completed`; each turn is charged the difference. It reports no cost, so Codex turns get no dollar cap and only the token limits bound them.
- **Failures** come from its `error` and `turn.failed` events, stderr as the last resort.
- **Configured `args`** pass only `-c`/`--config` (overrides of the keys in `allowedConfigKeys`: the model, its reasoning and output settings, and context limits; not the sandbox, approval policy, model provider, `notify`, shell environment or instructions), `--enable`/`--disable`, `--color`, `-m`/`--model` and `-i`/`--image`.

## 4. Running against the real CLIs

```powershell
go run ./cmd/ritam -real                                   # real Claude Code and Codex, TUI
go run ./cmd/ritam -real -headless -task "your question"   # one task with the recruited team, headless
```

`-real` sets `tui.useMockAgents` to false for the run; with real agents the idle timeout is at least 10 minutes. A project's own settings cannot turn the mock agents off. Each CLI uses its own login (`claude /login`, `codex login`); ritam never reads an API key. Agents are recruited in the TUI; whether they may write, and what they may run, is set by the provider settings, the recruit and the access level (README, "Safety").

## 5. What has been run live

These were observed against the live CLIs with earlier versions of ritam:

- a task completed against two live Claude Code processes (an Opus authority and a Sonnet-backed implementer) in 82 seconds, through `TASK_ASSIGNMENT -> ANSWER -> DELEGATE -> ANSWER -> FINAL_DECISION -> COMPLETED`;
- a three-agent run (Claude authority, Claude implementer, Codex implementer) had Codex replies parsed, routed and accepted into a decision;
- one three-agent task produced 417 live activity events while the agents worked;
- a resumed Codex turn asked to write a file could not, with the `sandbox_mode` override in place.

Running them found two defects, both fixed and covered by tests: a multi-line prompt passed as an argument to Windows' `codex.cmd` shim was cut at its first newline (the prompt now goes on stdin), and configured `args` placed after `--sandbox read-only` could override it (only an allowlist of flags passes now, and ritam's own flags come last).

Since then, the adapters and the orchestrator have been changed substantially (typed failures, retries, the delivery queue, per-turn budgets, the shared reply parser, the prompts). Those changes are tested with the mock agents, a scripted fake agent that fails the way the CLIs do, and stored samples of CLI output (`testdata/` in each adapter); this repository records no live run of the current version.

## 6. Not built

- **A verification gate**: nothing runs the project's tests or build before a decision is accepted.
- **An enforced independent reviewer**: role labels are not enforced; any participant's `REVIEW` is advice to the authority.
- **Review of file edits before they land**, and **per-turn capture of what changed** (a `Turn` holds message ids and usage, no diff).
- **A task board with dependencies** between tasks.
- **Git worktrees per agent**: every agent works in the same project folder.
- **Long-lived sessions with interrupt**: `Interrupt` is a no-op; a turn can only be killed, which is why a decision is deferred rather than cutting work short.
- **Structured-output replies** (`codex exec --output-schema`, a JSON schema for Claude): replies are free text with a header line.
- **Codex dollar pricing**: Codex reports tokens only, and no price table converts them.
- **Automatic resume when a rate limit resets**: a limit that resets later than `maxRateLimitWait` pauses the task until the human resumes it.
- **The local LLM proxy**: `internal/proxy` is a seam; `Start` returns `ErrNotImplemented` when enabled.
- **Full-text or vector knowledge search**: `KnowledgeRepository.Search` is a substring scan.
- **UI for disagreements and turn records**: both are stored and readable through the service, not shown.
- **`-demo` / `tui.demoOnStart`**: the task it creates has only one participant and is refused.
