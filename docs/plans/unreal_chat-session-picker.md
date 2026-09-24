# Session picker and unsaved empty chats

## User request

`/resume` without an ID should open a cursor-based session chooser, eliminating
the `/sessions`, copy ID, `/resume ID` workflow. Do not retain the empty session
created just by starting the application before resuming another conversation.
The user explicitly permits avoiding creation or removing that empty session;
prefer creating a session only when the first actual user message is accepted.

## Behavior

- In a capable interactive terminal, `/resume` opens an inline chooser with a
  visible selected row. Up/Down moves, Enter resumes that exact session, Esc
  dismisses it and returns to the same chat. Show recent sessions first, useful
  first-prompt/topic text, update time and a current-session marker. Keep identity
  unambiguous even when topics repeat. Titles must be readable for Belarusian
  conversations and must not execute embedded terminal controls.
- Bound the chooser to the viewport, scroll through all sessions, retain the
  selected item during resize, and avoid leaving menu rows in scrollback.
- Opening or canceling the chooser must not stop ongoing work or select a
  session. On confirmation, reuse existing stop/join/exact-ID resume/replay logic.
  Async output and task status must remain functional while choosing. Preserve
  the documented Ctrl-C stop-work behavior; EOF still exits with cleanup.
- Keep `/resume ID` and `/sessions`. Without a capable TTY, `/resume` must provide
  a usable plain fallback and explanation rather than wait for invisible arrows.
  A numbered selection fallback is acceptable; exact-ID resume remains available.
- Empty/no saved sessions, unreadable/missing selections, and storage errors need
  useful behavior without crashing or selecting some other session.
- A newly opened or `/new` chat stays unsaved until its first actual user message.
  Startup, local commands, opening/canceling a picker, resume, and exit should not
  create durable empty conversations. Preserve existing history and nonempty
  sessions; do not indiscriminately remove old sessions just to hide them.
- Default startup remains a fresh UNSAVED chat; the chooser is requested by
  `/resume`. Explicit `-session ID` still resumes that exact ID at startup.

## Implementation boundaries

The user additionally requested a compact reusable menu for other questions.
Keep session enumeration/resumption outside the editor component. The chooser
must receive its title and choices from its caller and return an opaque value
or cancellation. No hard-coded `Resume` title or session command in the reusable
component. Verify another title/choice set works with the same API. Keep this a
small reusable primitive; do not build a generic UI framework or speculative
question/workflow engine.

The actual unreal-agent-runner implements; parent independently reviews. Preserve
all existing uncommitted work. No commits, pushes, extra agents, server access,
credential inspection, or edits to the original GLMap workspace/sessions.

Read fix-review and current library/storage contracts before changes. Reuse the
existing single input reader, editor render lock/cursor ownership, store APIs,
and application event owner. Avoid a second terminal input loop or duplicate
session/operation state machine. Lazy creation belongs at the existing session
lifecycle boundary; do not spread invented readiness flags through consumers.
Preserve live task rows, colored completion notices, absence of repeated Working,
exact long/multiline paste, history, stdio backpressure, logging/error stacks,
session context restoration, process-group cancellation and cleanup.

## Verification and review

Use focused tests for session lifetime and selection plus a real PTY test that
shows a visible cursor moving through multiple seeded sessions and confirms
the selected session's history and actual subsequent model context. Cover
cancel/no selection, recent ordering, duplicate titles, enough rows to scroll,
small/resize windows, empty store, missing/corrupt selection, ongoing tools while
the menu is open, no selector keystrokes sent to the model, explicit-ID resume,
plain output, Ctrl-C/EOF and terminal restoration. Assert durable session counts:
launch/exit, launch/resume, and `/new`/resume leave no new empty session, while
the first real message creates exactly one durable conversation.

Review a checked draft with guru-code-review, fix findings, and run focused
checks followed by `make test check build`, `git diff --check` and both existing
independent PTY scripts. Do not weaken their behavioral assertions. The live-list
script is `.harness/experiments/unreal-chat-live-status/independent_live_status.py`;
the earlier terminal script is
`.harness/experiments/unreal-chat-terminal/independent_pty_smoke.py`.
If an old assertion explicitly expects the now-obsolete empty-session creation,
explain it rather than pretending it passed. Update help and documentation and
build `bin/unreal_chat`. Report actual checks and remaining limitations.
