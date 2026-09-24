# unreal_chat: one-shot implementation experiment

## Objective

Implement a usable first version of `unreal_chat`, an interactive terminal coding
chat built on the existing Unreal Agent harness. The experiment is to have the
existing one-shot `unreal-agent-runner` implement this plan, then independently
verify and review its result.

The executable and its source directory are named `unreal_chat` and
`cmd/unreal_chat`. Keep `unreal-agent-runner` working as before.

## First-version scope

1. `go run ./cmd/unreal_chat` opens a chat in the current directory.
   Accept one optional workspace argument, e.g. `unreal_chat ./my-project`.
   Provide `-h` with all supported flags and commands.
2. Keep one coordinator alive between ordinary user messages. Reading terminal
   input must remain possible while the model or tools are running. Submit
   complete user lines as new `InputExternal` events with fresh IDs.
3. Print readable assistant text and tool start/completion/failure/cancellation
   notices, including an operation identifier and useful command description.
   Do not print raw JSONL, private reasoning, credentials, or duplicate replayed
   tool events to the chat terminal. Preserve canonical session history on disk.
4. Support new and resumed sessions, with a visible session ID and workspace.
   Default storage belongs to the selected workspace's `.harness/sessions`.
   A startup `-session ID` must resume an existing session and report a missing
   ID as an error rather than silently creating an unintended session.
   Replay enough of the saved user/assistant transcript to make resume useful.
5. Support `/help`, `/status`, `/sessions`, `/new`, `/resume ID`, `/cancel ID`,
   `/stop`, and `/exit` (with `/quit` as an alias if convenient).
   Unknown commands and invalid arguments should be readable errors and leave
   the chat usable. Session switching must not leave old jobs or an old
   coordinator running; reject switching while busy with clear guidance to
   `/stop`, or cleanly stop and join the old runtime before switching.
6. `/cancel ID` cancels only the selected operation through the existing
   operation manager. `/stop` and Ctrl-C stop the current model/work and return
   control to the prompt without exiting the application or deleting history.
   They must not automatically restart the interrupted request or retry tools.
   The next user message must work. Ctrl-C at idle is harmless; `/exit` and EOF
   close the application and clean up active work. A normal assistant response
   must never close the chat.
7. Reuse existing provider adapters and authentication. Support provider, model,
   and reasoning-effort selection using flags and existing environment names.
   For this experiment use `openai-codex`, `gpt-6-astra`, and `xhigh`; make the
   chosen values visible. Do not silently force Codex when another provider is
   explicitly selected. Explain missing/expired credentials without exposing
   their contents. No new login or token-refresh implementation is required.
8. Use a chat-appropriate system prompt: explanation/discussion is not permission
   to edit; explicit requests to implement authorize relevant edits and tests.
   Include the workspace-root `AGENTS.md` when present. Do not inherit the
   one-shot claim that the process is inside an isolated sandbox container.
   Document that tools execute with the local process's permissions and this
   MVP does not enforce a read-only discussion mode.
9. Update build instructions/Makefile and add a short user-facing README with
   examples, commands, authentication setup, stop semantics, and limitations.

## Deliberately deferred

- Token-by-token streaming (display completed assistant messages for now).
- Automatic context compaction and unlimited-length conversations.
- Enforced `/discuss` and `/work` permission modes or approval dialogs.
- OAuth login/refresh, multiple agents, web UI, full-screen TUI, session forks,
  IDE integrations, and background jobs that outlive the chat process.

These are follow-up work, not reasons to leave the first version unfinished.

## Implementation guidance

- Inspect `cmd/internal/agentrunner/run.go` and `providers.go` for wiring,
  `harness/coordinator/loop.go` for scheduling, and existing tests for steering,
  stopping, recovery, and tool grace periods before designing changes.
- Reuse the existing inbox, context builder, coordinator, operation manager,
  and session store. Do not implement a second agent loop or call an external
  Codex CLI to answer messages. Extract small shared setup pieces only where
  actual reuse warrants it; avoid a broad runner rewrite.
- The stock runner always submits `StopWhenIdle`; the chat must not do this
  after each user message. Existing external input already interrupts an
  in-flight LLM request while preserving running operations.
- Design interruption using the existing lifecycle primitives where possible.
  A narrowly scoped harness control/API change is acceptable when necessary
  for `/stop`; preserve replay semantics and pending-input accounting. Simply
  cancelling and immediately resuming the same session can replay an unfinished
  user request, so test this case. Do not fabricate model responses to mark
  cancelled work complete.
- A store observer only sees persisted items, not every operation update.
  Do not consume the operation manager's Updates channel twice. Use a small
  forwarding adapter or a suitable existing observation boundary for status.
- Store methods are not serialized for the same session, and observer mutation
  is not concurrency-safe. Keep ownership explicit, and do not read mutable
  coordinator state from the input goroutine. Avoid blocking I/O in translators.
- Terminal input and asynchronous output must coexist without losing input.
  A simple line-oriented interface is enough; avoid new dependencies unless
  they are clearly necessary. Non-TTY/pipe operation must be deterministic too.
- Avoid copying the entire runner or introducing generic event buses/frameworks.
  Keep this a focused addition that can be understood and reviewed.

## Acceptance checks

Use deterministic fake/model HTTP transports and real temporary local files or
short-lived subprocesses as appropriate. Automated tests must not need a paid
model, real credentials, or an external service.

- Two sequential messages reach the same conversation, and replying does not
  terminate the application.
- A new user message while a tool is pending reaches the model before that tool
  finishes; the pending tool is not accidentally cancelled or launched twice.
- An in-flight model request can be steered without losing accepted user input.
- Tool notices and `/status` reflect running and terminal states with stable IDs.
- Cancelling one job leaves unrelated jobs and the chat alive.
- `/stop` during model generation and during a shell command preserves the
  session, stops the work, does not automatically restart it, and accepts the
  next user message. Test interruption/replay as well as the live lifecycle.
- Session resume includes the old conversation exactly once. New/switch/resume
  failures preserve the usable current session where feasible.
- EOF, exit, output errors, and provider failures clean up goroutines and child
  processes; no hangs, data races, or indefinite retry loops.
- Existing one-shot runner behavior remains covered by its original tests.

Run focused tests during development, then `make test check build` (extend the
build target for `unreal_chat`) and `git diff --check`. Exercise the CLI with a
scripted local model or equivalent integration test. Report what actually ran
and any remaining limitations; do not claim a real-provider test unless run.

## Working boundaries for the one-shot agent

- Implement the code, tests, and documentation in this repository. Preserve this
  plan and any pre-existing/unrelated changes. Do not commit, push, reset, clean,
  or otherwise discard changes.
- Do not invoke SSH, SCP, rsync, server APIs, tunnels, or remote diagnostics.
  Provider access is managed by the configured harness adapter. Use local
  mock servers for tests. Do not read or print credential files or environment
  secrets through Bash tools, and do not install system-wide software.
- Write project configuration inside the project, never `/tmp` or `/private/tmp`.
  Ordinary test temporary files are fine. Use platform-default local commands
  before introducing custom hosts, ports, users, or wrappers.
- Once a coherent implementation draft exists and relevant checks have run,
  read and apply `/Users/evgen/.codex/skills/guru-code-review/SKILL.md`.
  Do not load it during initial exploration or implementation. Apply its
  general Go/concurrency/architecture guidance where relevant and fix concrete
  findings, then rerun affected checks. Do not spawn additional agents.
- Before writing or finalizing a bug fix, read and apply
  `/Users/evgen/.agents/skills/fix-review/SKILL.md`.
- End with a concise report of changed files, behavior, verification, and known
  limitations. Complete the implementation rather than stopping after a plan.
