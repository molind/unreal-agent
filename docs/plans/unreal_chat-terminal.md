# unreal_chat: terminal reliability and usability

Implement this task in one shot using the existing unreal-agent harness. The
parent assistant will independently review the resulting changes. Preserve all
current uncommitted work; it is the accepted first chat implementation.

## Reported failures and investigation

The user ran unreal_chat in ~/git/glmap. A long assistant review was interrupted
mid-sentence by `unreal_chat: write /dev/stdout: resource temporarily unavailable`
and exit 1. Reproducer: start the chat in that workspace and run
`/resume 07b7620a-8299-4202-8d60-98db68726f80`. Replaying the long saved history
immediately crashes. A read-only copy of this exact session is available at
`.harness/experiments/unreal-chat-terminal/repro-sessions/` for local reproduction.
Do not change anything in ~/git/glmap or launch tools against that project.

Likely cause (verify): main duplicates stdin and sets O_NONBLOCK on it. dup
shares file-status flags; terminal stdin/stdout/stderr can share the same open
description. Existing os.Stdout/os.Stderr were created before this flag change,
so their Go poller state may not handle EAGAIN under output backpressure. Existing
CLI tests use separate pipes and missed this. Read the Go and OS contracts;
fix descriptor/poller ownership at its source, not with generic retry sleeps at
every writer. Preserve Darwin input.Close unblocking, broken-pipe cleanup, and
restoration of terminal flags on every normal/error exit. Test actual shared
TTY descriptors and long output, not only buffers and independent pipes.

The user also resumed `3f128877-13b0-4d8a-ad6f-af89dd1615cf`, then typed `continue`;
the model said there was no earlier task. Inspection established that this was
a different new session: its first external message was literally `continue`.
The original review is intact in `07b7620a...`. Improve session selection UX so
the first user prompt/topic, current session, and empty sessions are recognizable.
Keep genuine history/context resume covered by tests; do not merge conversations
or silently resume another ID. Do not auto-resume a session at startup unless
explicitly requested.

## Requested features

1. Fix the stdout EAGAIN crash, including the exact long-history replay path.
2. Add terminal colors for assistant/user/tool/status distinctions. Respect
   NO_COLOR and non-TTY/dumb terminal output; redirected output stays plain.
3. Show that the agent is working even before it has returned a message, and
   animate active commands individually with useful labels and elapsed time.
   Completion, failure and cancellation must leave readable final notices.
   Reuse durable operation events and existing state; do not add another agent
   loop or consume the operation update channel twice.
4. Improve terminal input: an editable prompt, useful standard editing/history
   keys, and async messages/status updates that preserve the text and cursor
   being edited. Handle wrapping/resizing, Unicode (Belarusian text), paste,
   Ctrl-C to stop work while retaining the chat, EOF and clean shutdown.
   A maintained terminal/readline library is acceptable when it substantially
   reduces custom code. Keep the ordinary piped/script interface deterministic.
   No need for a full-screen UI, Markdown engine, or token streaming.
5. Record executed commands separately from the conversation, in private local
   log files under the workspace .harness (or consistent with overridden session
   storage). Include timestamps, session/operation IDs, command, working
   directory, lifecycle status, duration/exit code, and captured output location
   where available. Avoid duplicate entries due to transcript replay; keep
   ongoing/resumed operation identity meaningful. Tell the user where logs are.
6. Add actionable diagnostic logging: timestamp and stage/component, session and
   operation identifiers when relevant, actual error causes, and stack traces
   captured at a useful failure boundary. Preserve error chains rather than
   replacing every error with a generic provider/configuration message. Include
   relevant lifecycle events so a failure can be placed in time. Logs should
   help diagnose output/runtime/provider/storage failures and panic paths. Do
   not claim to catch panics in arbitrary goroutines unless actually supported.
   Redact credentials, avoid private model reasoning and raw authentication
   contents, use private file permissions, and avoid massive per-frame logs.
7. Improve /sessions and /resume feedback as above. An empty transcript should
   be explicitly visible; retain exact requested-ID semantics.
8. Update the chat README/help for controls, colors, command/diagnostic logs,
   session selection and limitations.

## Verification and scope

- First reproduce the output failure with an automated regression before the
  fix when practical. Exercise a real executable in a PTY with slow output
  draining, shared stdin/stdout/stderr, and a multi-kilobyte assistant response
  or replay; assert complete output, successful next input, and clean exit.
- Verify that typing partially through a message while model/tool events and
  spinner ticks arrive preserves input and cursor, including Unicode and narrow
  window/resize. Verify raw/echo flags and file flags are restored after exit
  and failure, and no processes remain after cancellation or broken pipes.
- Test plain piped output, NO_COLOR, multiple active operations, success/failure/
  cancellation logs, error cause + stack + stage, redaction, and saved context
  in the actual model request after resume.
- Use local fake providers/HTTP mocks; no paid model calls from acceptance tests
  and no credential reads from Bash. Existing adapter authentication for this
  implementation run is already authorized.
- Run focused tests while developing; finish with `make test check build` and
  `git diff --check`. Do not edit sources while tests are reading them.
- Keep changes proportionate. The EAGAIN fix is a small boundary fix; richer
  terminal input and logging are separately requested features. Avoid unrelated
  harness rewrites, extra scheduling state, and broad generic frameworks.
- Before fixes, read/apply `/Users/evgen/.agents/skills/fix-review/SKILL.md`.
  Once a coherent implementation has passed relevant checks, read/apply
  `/Users/evgen/.codex/skills/guru-code-review/SKILL.md`, fix concrete findings,
  and rerun affected checks. Do not load the latter during initial implementation.
- Work only in this repo and ordinary test temp directories. No commits, pushes,
  destructive Git commands, SSH/SCP/rsync, remote diagnostics, server access,
  credential inspection, system-wide installs, or additional coding agents.
  Put project configuration in this repository, never /tmp.
- Finish with an honest concise report: cause, changes, tests actually run,
  dependencies, and limitations. Complete the work rather than stopping at a plan.
