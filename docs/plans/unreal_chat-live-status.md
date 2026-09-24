# Live task status for unreal_chat

## Requested behavior

Keep a transient, animated vertical list of active work immediately above the
input line. The prompt stays `you> `. Show the agent's pending/model activity as
a status row, and each running or canceling operation as its own row with a
spinner, useful command label, short ID, elapsed time, and state. A simple ASCII
animation using `*` and similar characters is sufficient.

When an operation finishes, remove its live row and append exactly one durable
transcript entry above the live area. Use green for success, red for failure
(including nonzero shell exit), and yellow for cancellation, with readable text
and proper color resets. Preserve full operation IDs in completion notices and
the existing separate command logs. Honor NO_COLOR.

The repeated `Working — waiting for model response. Ctrl-C or /stop to stop.`
lines currently pollute the transcript. In an interactive terminal replace them
with the transient agent status, including across model/tool turns. Do not add
running notices or animation frames to permanent history. Plain/piped output
keeps deterministic, useful lifecycle text without cursor sequences.

## Implementation boundaries

- Delegate implementation to the local unreal-agent-runner; parent reviews it.
- Preserve all existing uncommitted work, terminal flag restoration, backpressure
  handling, error/stack diagnostics, exact multiline and long paste, history,
  live steering, cancellation, resume, and logging. No commits or pushes.
- Change presentation and terminal integration only as needed. Do not change
  coordinator scheduling, provider behavior, or session semantics.
- Read the maintained line editor's implementation before choosing the rendering
  boundary. A newline inside a single-line SetPrompt must not be assumed safe.
  Reuse library ownership of editing/cursor/history and serialization; avoid a
  second editor or duplicate operation state machine.
- Render within the available terminal height and width. With more tasks than
  fit, show a bounded list and a readable overflow count; /status retains all.
- Async messages and completions appear above the live area. Preserve drafts,
  cursor position, folded paste, and readable scrollback when rows are added,
  removed, resized, or cleared on stop/new/resume/exit.

## Verification and review

Use focused display tests and real PTY tests with visible-screen/cursor checks,
not just input-payload checks. Cover concurrent tasks finishing out of order,
animation, success/failure/cancel colors, no duplicate Working/terminal entries,
idle clearing, resize and a narrow/short terminal, overflow, and a Cyrillic draft
with the cursor in its middle. Keep existing paste/backpressure/resume/cleanup
regressions passing.

Read fix-review before fixes and guru-code-review after a coherent checked draft.
Run focused checks, then `make test check build`, `git diff --check`, and the
existing independent PTY smoke script. Do not weaken independent checks to pass.
Report exact behavior, tests, and any material limitations for parent review.
