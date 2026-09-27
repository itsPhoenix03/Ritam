# Real Claude Code / Codex integration: what is implemented, what remains

See also: [../README.md](../README.md), [architecture.md](architecture.md), [agent-protocol.md](agent-protocol.md).

This document used to describe how real integration *would* work. It no longer needs to: the Claude Code and Codex adapters now drive the real CLIs. This document describes exactly what that means, why the transport and wire formats are shaped the way they are, and — in a clearly separated section at the end — what still does not work.

## 1. The transport: one CLI process per protocol message

Both adapters are built on `core.OneShotRunner` (`internal/agents/core/oneshot.go`). It resolves the executable on `PATH` via `exec.LookPath` (never a hard-coded install path — both CLIs are commonly `.cmd` shims on Windows), then for every protocol message runs the CLI once with `exec.CommandContext`, waits for it to exit, and captures stdout/stderr separately.

**Why one process per message, not a long-lived session.** Claude Code and Codex are built for a human sitting at a terminal. A long-lived session can block forever on an interactive permission prompt that nobody is there to answer, and a wedged session looks exactly like a slow one from the outside — there is no way to tell "still thinking" from "stuck" without a timeout that kills and restarts it anyway. A process that must exit before its reply counts gives three things a session cannot:

- a hard timeout (`OneShotRunner.Timeout`, default 5 minutes, `ErrTimeout` on expiry);
- a clean retry boundary — each message is independent, so retrying means launching a fresh process, not untangling partial session state;
- a crash that costs one message, not the whole task.

The cost is real and is paid deliberately: conversational context does not survive between calls, so it has to be re-supplied every time. That is what the knowledge-injection mechanism in section 3 is for.

**Arguments are always a slice, never a shell string.** `OneShotRunner.Run` builds `exec.CommandContext(ctx, path, args...)` from a `[]string`; nothing is ever concatenated into a string and handed to a shell. Prompt text — which is agent-controlled, model-generated, and therefore untrusted — reaches the CLI as inert argv data. It cannot be reinterpreted as a shell command no matter what characters it contains.

## 2. Claude Code adapter (`internal/agents/claude`)

### Invocation

```
claude -p <prompt> --output-format json
```

plus, when the agent is read-only (the default — `AgentConfig.AllowWrites == false`):

```
--allowedTools Read,Grep,Glob --disallowedTools Bash,Edit,Write,MultiEdit,NotebookEdit --permission-mode dontAsk
```

or, when `allowWrites` is set:

```
--permission-mode acceptEdits
```

`--model` is appended when `AgentConfig.Model` is set.

Why each flag:

- `--output-format json` — the CLI returns exactly one JSON object. `extractResult` (`internal/agents/claude/adapter.go`) unmarshals it into a small envelope (`type`, `subtype`, `is_error`, `result`, `session_id`) and reads `.result`, checking `.is_error`/`.subtype == "error"` first. Non-JSON stdout — a login prompt, a crash banner, a CLI-upgrade notice — fails `json.Unmarshal` and is returned as a hard error. It is never treated as if it were the agent's considered reply; a malformed or unexpected response must surface as a failure, not silently become part of the deliberation.
- `--allowedTools`/`--disallowedTools`/`--permission-mode dontAsk` (read-only case) — the allowlist and the explicit denylist say the same thing twice on purpose: in headless mode, a tool that is merely "not on the allowlist" can still surface as an interactive permission prompt with nobody present to answer it, so the deny list closes that gap explicitly rather than relying on the allowlist alone.
- `--permission-mode acceptEdits` (write-enabled case) — the only mode change needed once writes are allowed; no separate patch-review step exists yet (see section 4).

### Reply format

The header the orchestrator asks Claude Code to emit — `@@ORCH type=<TYPE> to=<agent-id>` — is this project's convention, not a Claude Code feature. `parseOutput` (`internal/agents/claude/protocol.go`) scans the result text for lines starting with `@@ORCH` (tolerating leading fence/bullet decoration such as `` ` ``, `>`, `-`, `*`), and everything up to the next header (or end of input) becomes that message's content. An unknown `type=` value is a hard parse error; a missing `to=` field is a hard parse error. Text before the first header, or between recognized blocks, is treated as prose and dropped.

## 3. Codex adapter (`internal/agents/codex`)

### Invocation

```
codex exec --sandbox <read-only|workspace-write> --skip-git-repo-check --color never --output-last-message <tmpfile> [--model M] [--cd DIR] <prompt>
```

Why each flag:

- `--sandbox read-only` / `--sandbox workspace-write` — selects Codex's own sandbox mode, matching `AgentConfig.AllowWrites` exactly the way `--allowedTools`/`--permission-mode` does for Claude.
- `--skip-git-repo-check` — this orchestrator deliberately does not require its working directory to be a git repository (see the README's constraints section), and Codex's default behavior is to refuse to run outside one. This flag is required, not optional, given that constraint.
- `--color never` — keeps ANSI escape codes out of stdout, which matters because stdout is also where banner text, reasoning summaries and progress output land.
- `--output-last-message <tmpfile>` — the agent's final message is read from this file (`newLastMessageFile` creates a per-invocation temp file, cleaned up after the run), never scraped out of stdout. Stdout carries the CLI banner, reasoning traces and progress output interleaved with whatever the model said; the `-o` file is the one place that is exactly the final message and nothing else.
- `--model` — appended when `AgentConfig.Model` is set.
- `--cd <dir>` — appended when `AgentConfig.WorkingDir` is set.
- The prompt is the **last positional argument**, appended after every flag. This is deliberate, not incidental: because it is positional and last, prompt text can never be parsed as a flag, however it is generated or whatever it happens to start with.

### Reply format

`##CODEX TYPE=<TYPE> TO=<agent-id>` — again, this project's convention, not a Codex feature. Parsing (`internal/agents/codex/protocol.go`) is structurally identical to the Claude adapter's, with one deliberate difference: header keys are matched case-insensitively (`type=`/`TYPE=` both work), whereas Claude's parser is case-sensitive. Everything else — fenced/bulleted header tolerance, hard error on unknown type, hard error on missing `to`/`TO` — is the same discipline applied independently.

### Why the two CLIs keep different wire formats on purpose

`@@ORCH` and `##CODEX` are visually distinct and use different case sensitivity rules. This is intentional, stated directly in both package doc comments: Claude Code and Codex are separate applications with separate flag sets, separate sandbox models and separate output shapes. Giving them a shared assumed wire protocol would be wrong the moment either CLI's actual behavior diverges from the other's, and it would hide that divergence behind a false abstraction. Each adapter package owns its own prompt-rendering and reply-parsing logic end to end; nothing about Claude's real behavior is assumed to say anything about Codex's, or vice versa.

## 4. Context across stateless calls: the knowledge-injection mechanism

Because every CLI invocation is a fresh, memory-less process (section 1), something else has to carry context between messages. That something is `MessageBus.Enrich` (`internal/orchestrator/bus.go`), wired at construction time (`orchestrator.New`) to `Orchestrator.attachKnowledge`.

Mechanism, exactly:

1. Just before a message is handed to an adapter — inside `MessageBus.deliver`, on the **delivered copy** of the message, after cloning it — `Enrich(ctx, t, delivered)` runs.
2. `attachKnowledge` queries `knowledge.Store.ContextFor(ctx, taskID, query, knowledgeContextLimit)`, where `query` is the message's own content (falling back to the task title if empty) and `knowledgeContextLimit = 6` — at most six entries.
3. The hits are rendered as `- [<category>] <title>\n  <body>\n` lines and attached as `delivered.Metadata["knowledge"]`.
4. Both adapters' `renderPrompt` check `m.Metadata["knowledge"]` and, if non-empty, render a `RELEVANT PROJECT KNOWLEDGE (established earlier in this task):` section into the prompt ahead of the incoming message.

Two things matter about where this happens:

- It only ever touches the **delivered copy**. The recorded message — what `MessageBus.Route` appends to the message log via `messages.Append` — is untouched; `Enrich` runs strictly after `Route` has already recorded the un-enriched message. The message log stays a faithful record of what was actually sent; the knowledge block is presentation added purely for the recipient's one-shot invocation.
- It is the direct replacement for conversation memory. A one-shot CLI process has no way to recall an earlier turn on its own; the only thing that gives it continuity is what the orchestrator puts in the prompt. This is why shared knowledge — not any individual agent's transcript — is the source of truth for what "the task knows so far." `Orchestrator.recordKnowledge` is the other half of this loop: it promotes `ANSWER`/`CHALLENGE` content into `INVESTIGATION` knowledge entries and `TEST_RESULT`/`IMPLEMENTATION_RESULT` content into `TEST_RESULT` entries as they are routed, so later rounds have something to retrieve.

## 5. The prompt contract (both adapters, same shape)

`renderPrompt` in each package builds a prompt containing, in order:

1. The agent's own id (`YOUR AGENT ID: <id>`).
2. Its role: `AUTHORITY` (holds final decision-making responsibility, may delegate/decide/challenge) or `SUPPORTING AGENT` (names the authority agent explicitly, states it may investigate/propose/implement/review/**challenge** but may **not** issue `FINAL_DECISION` — "the orchestrator will reject it").
3. The task id, title and description.
4. The exact list of agent ids it may address: every other participant (marking which one is the authority), plus the literal `human` and `*` (broadcast).
5. The `RELEVANT PROJECT KNOWLEDGE` section, when `Metadata["knowledge"]` is non-empty (section 4).
6. The incoming message, delimited `--- INCOMING <TYPE> FROM <agent> ---` / `--- END ---`.
7. The read-only/write-access constraint in force for this call — an explicit sentence telling the agent whether it may edit files/run commands, or must describe a change instead of making it.
8. The mandatory reply header format, with a worked example, and the full list of valid `MessageType` values (`domain.AllMessageTypes()`), so the model always sees the complete, current vocabulary rather than a hard-coded subset that could drift from `internal/domain`.

Nothing about this prompt assumes the previous call happened at all — every field needed to act correctly is re-derived and re-supplied on every single invocation, consistent with the one-shot transport in section 1.

## 6. Running against the real CLIs

```powershell
go run ./cmd/orch -real                                   # real Claude Code + Codex, TUI
go run ./cmd/orch -real -headless -task "your question"   # one real task, headless, prints the event log
```

`-real` sets `cfg.TUI.UseMockAgents = false` and raises `cfg.Orchestrator.IdleTimeout` to at least 10 minutes if it is currently lower — real CLI calls routinely take minutes, and the idle timeout must not fire mid-answer. Without `-real`, `orch` always uses the in-process mock adapters regardless of `orch.yaml`'s `agents.*.type`.

Auth is each CLI's own: `claude /login`, `codex login`. Nothing in this project ever reads or accepts an API key from `orch.yaml`, environment variables set by these packages, or any other config source — the CLI processes are launched and expected to already be authenticated out of band.

Write access is off by default (`AgentConfig.AllowWrites` defaults false, the zero value, deliberately the safe one) and is config-gated per agent. It should stay off for any agent whose `IMPLEMENT` results are not being reviewed by a human before being trusted, since there is no patch-review step yet (section 7).

## 7. A verified real run

On 2026-09-15, a real task ran to completion against two live Claude Code processes: `opus` (AUTHORITY) and a sonnet-backed `scout` acting as IMPLEMENTER. The task: "Can a task ever go straight from CREATED to COMPLETED?" It completed in 82 seconds. Flow:

```
orchestrator --TASK_ASSIGNMENT--> opus, scout
scout  --ANSWER----------------> opus
opus   --DELEGATE (verification round)--> scout
scout  --ANSWER----------------> opus
opus   --FINAL_DECISION--------> * ("No" — citing task.go:39 and taskmanager.go:120)
orchestrator: D-001 ACCEPTED -> task COMPLETED
```

This confirms, against the real CLI and not just adapter unit tests: executable resolution, the one-shot invocation shape, the `--output-format json` envelope parsing, the read-only tool policy, prompt rendering including the knowledge section, and `@@ORCH` reply parsing, end to end through the orchestrator's `RunTask` loop.

## 8. Still deferred — be precise about this

The Codex adapter **has now completed live round trips** through the orchestrator: `opus -> codex DELEGATE`, `codex -> opus ANSWER`, parsed by `parseOutput`, routed, and accepted into a `FINAL_DECISION`. A three-agent run put a Claude authority, a Claude implementer and a Codex implementer in the same deliberation; it ran ten rounds and then paused on `maxDeliberationRounds` for a human, which is loop protection working, not a failure.

Getting there exposed two real defects, both now fixed and covered by tests:

**1. The prompt was truncated at its first newline.** On Windows `codex` on PATH resolves to `codex.cmd`, a batch shim, so Windows runs it through `cmd.exe`, which re-parses the command line. A multi-line prompt passed as an argument ends at the first newline. The agent received only:

```
You are a coding agent participating in a multi-agent deliberation.
An orchestrator routes every message; you never talk to another agent directly.
```

and replied *"Understood. Send the coding task and any specific roles or deliberation protocol you want me to follow."* — which the adapter correctly reported as a missing `##CODEX` header, an accurate error pointing at entirely the wrong cause. The prompt now goes in on **stdin** via the `-` positional argument, which sidesteps shell parsing, quoting and the 32 KB Windows command-line limit. The Claude adapter is unaffected: `claude` resolves to a real `.exe`, so no shell re-parse occurs.

**2. Configured `args` could defeat the read-only sandbox.** They were appended *after* `--sandbox read-only`, and for Codex a later flag wins, so a config could append `--dangerously-bypass-approvals-and-sandbox` and a nominally read-only agent would run unsandboxed. `safeExtraArgs` now rejects sandbox-, working-root- and output-affecting flags from config, the orchestrator's own flags are appended last so they win any remaining conflict, and bare positional arguments are dropped — Codex reads the first positional as the prompt, so a stray token (including the orphaned value of a rejected flag) would silently replace the agent's brief.

This second defect was found by a Codex agent auditing `buildArgs` through the orchestrator, which is the system doing the job it was built for.

Everything else still missing, honestly listed rather than implied to be "coming soon":

- **Session continuity.** Neither `claude --resume` nor `codex exec resume` is used. Every call is stateless; the knowledge-injection mechanism (section 4) is the entire substitute, not a supplement to native session memory.
- **Streaming.** Neither `--output-format stream-json` (Claude) nor `--json`/JSONL event output (Codex) is implemented. The TUI only updates when a CLI process exits — there is no mid-turn progress.
- **Structured Codex replies via `--output-schema`.** Not implemented. This is noted directly in `internal/agents/codex/adapter.go`'s package doc as the most promising next step for that adapter specifically: it would remove the need to parse a free-text `##CODEX` header out of the final message at all.
- **Write access has no patch-review step.** `AllowWrites` is config-gated and off by default; turning it on lets `IMPLEMENT` actually run `acceptEdits`/`workspace-write`, but nothing reviews the result before it's trusted. It should stay off until a review step exists.
- **Token/cost accounting.** The Claude JSON envelope carries `usage` and `total_cost_usd` fields; they are logged (`a.log.ForAgent(a.id).Debug(...)`) but never aggregated per task or exposed anywhere.
- **Agent crash-recovery / restart-backoff policy.** `Orchestrator.handleOutbound` still fails the whole task immediately on any adapter error (`TODO(phase-next)` in the code); nothing retries.
- **`RequireHumanApproval` run-loop continuation.** A human `REJECT` after `FINAL_DECISION` ends the run; a fresh `RunTask` call is needed to resume deliberation.
- **SQLite persistence.** `internal/storage/sqlite.go` remains a schema sketch with no compiled logic; everything is in-memory and lost on process exit.
- **The LLM proxy HTTP server.** `internal/proxy` is a seam only; `Start` returns "not implemented" whenever the proxy is enabled.
- **TUI panes for knowledge search and disagreements.** `Store.ContextFor` and `Service.TaskDisagreements` exist and are tested at the service layer, but neither has a terminal UI surface yet.

None of the above blocks the Claude Code path from working end to end as verified in section 7; they are the next things to build, not prerequisites of what already runs.
