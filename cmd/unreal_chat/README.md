# unreal_chat

A local coding chat with an editable terminal prompt and a plain piped interface,
using the Unreal Agent harness. Requires
Go 1.27 and a Unix-like host (tested on macOS). It does not invoke the Codex CLI.

```sh
make build
./bin/unreal_chat                         # current workspace
./bin/unreal_chat ./my-project
./bin/unreal_chat -session SESSION_ID ./my-project
./bin/unreal_chat -h
# Or, from this repository:
go run ./cmd/unreal_chat -provider openai-codex -model gpt-6-astra -reasoning-effort xhigh
```

Flags precede the optional workspace. The defaults for this experiment are
**openai-codex / gpt-6-astra / xhigh**; all three and the session ID (or unsaved state)/workspace are
shown at startup. An explicit provider is never replaced with Codex.

## Authentication and configuration

Reuse an existing ChatGPT-authenticated Codex login: the adapter reads
`$CODEX_HOME/auth.json`, or `$HOME/.codex/auth.json` by default. Alternatively set
`OPENAI_CODEX_AUTH_FILE` to an existing auth file, **or** supply
`OPENAI_CODEX_ACCESS_TOKEN` and `OPENAI_CODEX_ACCOUNT_ID`. Do not combine these
sources. A subscription access token is required; an OpenAI API key is not a
Codex subscription token. Obtain/renew credentials outside this application;
there is no login or refresh flow here. Missing or expired credentials require
external renewal and restarting the chat.

Other existing adapters are available:

```sh
# Set OPENAI_API_KEY securely in your environment first.
unreal_chat -provider openai -model gpt-6-astra ./my-project
unreal_chat -provider ollama -model YOUR_LOCAL_MODEL ./my-project
```

OpenAI, OpenRouter, and Fireworks use `OPENAI_API_KEY`, `OPENROUTER_API_KEY`, and
`FIREWORKS_API_KEY`, respectively. `UNREAL_HARNESS_LLM_API_KEY` overrides these,
but is not used by Codex. Ollama does not need a key.

| Flag | Environment fallback / default |
| --- | --- |
| `-provider` | `UNREAL_HARNESS_LLM_PROVIDER` / `openai-codex` |
| `-model` | `UNREAL_HARNESS_LLM_MODEL` / `gpt-6-astra` |
| `-reasoning-effort` | `UNREAL_HARNESS_LLM_REASONING_EFFORT` / `xhigh` |
| `-base-url` | `UNREAL_HARNESS_LLM_BASE_URL` / adapter default |
| `-max-attempts` | `UNREAL_HARNESS_LLM_MAX_ATTEMPTS` / **1** |
| `-session ID` | Resume an existing session; missing IDs are errors |
| `-session-directory DIR` | `<workspace>/.harness/sessions`; relative paths are workspace-relative |

Flags override environment values. Reasoning effort supports `low`, `medium`,
`high`, `xhigh`, and `max` (provider support varies). Unlike the one-shot runner,
chat does **not** load `.env` files or apply sandbox proxy configuration. Export
settings in the launching shell. Never paste credentials into the conversation.

## Using the chat

Type a complete line and press Enter. You can send another line while the model
or tools are working: this steers the model without canceling pending tools.
Assistant messages display on completion, not token by token. Tool notices show
command descriptions, not raw tool output or JSONL.
On capable terminals replies use a neutral foreground and spaced conversation
blocks. CommonMark headings, bold/italic emphasis, lists, quotes, visible links,
and code blocks are rendered using the goldmark parser. Prose wraps at words
(to at most 100 columns). Replies start at the left edge, without artificial
leading spaces. Code rows have no decorative left border or list/quote prefix,
so selecting them copies only the code's own indentation and text. Language
headings and boundaries use ordinary Markdown fences: `` ```lang ``, code, then
`` ``` ``. Fences are lengthened when needed for code that contains backticks.
Replies have one blank separator line, and text selection/copying belongs to the
terminal: no mouse capture, Copy buttons, or clipboard integration. Code keeps its
indentation and line breaks, with tabs expanded for display only. No syntax
highlighter, HTML execution, external image fetching, or hidden OSC links are used. Canonical history/model context is unchanged.

An animated vertical list shows model/input-pending activity plus one row per
running/canceling operation, with a short ID, command label, state, and elapsed
time. A quiet model/effort/workspace rule separates this area from `you> `, whose
accent never colors the draft. All decorations share the editor's viewport and
cursor ownership, disappear before submission, and hide when space is needed
for the draft. The chooser temporarily replaces the input area.

Each finished operation leaves one compact notice: a green **✓** for success,
red **✗** for failure (including nonzero shell exits), or yellow **–** for
cancellation. Commands stay neutral; duration and diagnostic metadata are muted.
Long descriptions are clipped to the current width. Multiline scripts show their
first line and an additional-line count rather than a flattened wall of code.
Failures/cancellations retain an explicit state and short ID; **`/status` keeps
the full IDs and descriptions**, including commands needed for `/cancel ID`.
Live rows and repeated Working notices never enter the transcript. Command logs
are unchanged. Plain/piped output retains the existing full-ID lifecycle notices
and raw Markdown, without animation or terminal styling.

### Editing and colors

On a TTY with `TERM` set (not `dumb`), a locally extended `golang.org/x/term` line editor
(see `internal/lineeditor`) handles asynchronous redraws, wrapping, resize, and Cyrillic/Belarusian input.

- Left/Right or Ctrl-B/F: move; Home/End or Ctrl-A/E: start/end.
- Backspace/Delete: delete; Ctrl-U/K: clear before/after the cursor;
  Ctrl-W: delete the preceding word; **Option/Alt-Left/Right: move by word**
  (to the previous/next space-separated word start). Meta-b/f (`ESC b` /
  `ESC f` sequences) and Alt/Meta-modified CSI arrows are recognized, including
  the usual macOS terminal mappings. Terminal shortcuts must be configured to send these
  keys to the application rather than intercept them locally.
- Option+Return (or Ctrl-J) inserts a real newline without submitting. The next
  line starts at column zero, without padding to align it after `you> `. Enter
  submits all lines as one message; multiline slash text remains user content.
- Up/Down move within multiline drafts; Ctrl-P/N always recall history. In a
  single-line draft Up/Down recall history as before. Home/End (Ctrl-A/E) move to
  the beginning/end of the whole draft. History holds the last 100 submissions
  in this process and preserves multiline text; it is not a separate input log.
  Tall multiline drafts scroll within the editor viewport. Async output, resize,
  editing and Ctrl-C retain their existing behavior.
- Ctrl-C follows the current state, not a timed double-press counter:
  1. A nonempty draft is cleared, including inline text, folded paste, and an
     over-limit/rejected draft. No model/tool is stopped and nothing is submitted
     or added to history; previously submitted history is retained.
  2. With an empty draft, active model/tool work is stopped and joined. The chat
     remains open for another message.
  3. With an empty draft and no work pending, exit the chat.
  Rapid consecutive keys are handled in order; the exit step waits for stop/join.
  An OS SIGINT (including plain/piped input) is not an editor key and follows
  stop-when-busy / exit-when-idle. `/stop` always stops without exiting.
- Ctrl-D: EOF on an empty draft; otherwise delete at the cursor.
- Short, single-line bracketed paste (up to **160 printable Unicode characters**)
  inserts normal visible text: paths, IDs and short phrases can be edited one
  character at a time, mixed with typing, and recalled from history.
- Longer, multiline, or control-bearing paste folds into one **▣ attachment cell**
  and announces its byte/line counts. Enter sends the **exact text**, preserving
  newlines, tabs, indentation, and trailing content, as one user message. Paste
  never submits automatically. Even a short copied line ending in a newline
  remains a block so its original bytes are retained.
- **Only the supported slash commands listed below are interpreted.** Absolute
  paths such as `/Users/name/project/file.go` and `/tmp`, or any other unrecognized
  slash-leading text, are user messages rather than unknown-command errors.
  Known single-line commands run only after an explicit Enter outside paste
  framing. Copied commands/IDs may include surrounding blank whitespace or a
  trailing newline: `/resume ` plus a copied ID still works. Blocks with multiple
  content lines, including ones beginning `/exit`, remain user text. User-message
  whitespace is never trimmed or flattened.
- Move across a folded block with the arrows; Backspace/Delete removes the
  whole block. You can edit typed text before/after it and mix multiple blocks.
  To change a block’s contents, delete it and paste the replacement. History
  recall retains the complete blocks, not just their visible markers. Ctrl-C
  discards the current draft's blocks without deleting submitted history.
  Folded contents are not echoed; short printable pastes are visible like typing.
  Neither kind is fed to the terminal key parser or executed as escape sequences.
- A complete message (typed text plus all blocks) supports **up to 1 MiB of
  UTF-8 text**. The visible editor supports 4096 cells; each folded block uses
  only one. Exceeding either limit explicitly rejects the **whole draft**;
  press Enter to start a fresh draft. No partial prefix is sent or added
  to history. Invalid UTF-8 and exhausted attachment storage are also rejected.

Color is reserved for the input accent, inline emphasis and result markers;
ordinary assistant text uses the terminal's default foreground.
Live command labels use ASCII escapes for non-ASCII characters to avoid width
ambiguities; full Unicode labels remain in notices and `/status`. As in upstream
x/term, wide/combining draft characters and drafts taller than the screen have
limited support; use folded paste for large text. Status hides when the draft
needs the viewport. Set `NO_COLOR=1` to suppress colors without losing editing.
Redirected output,
an unset `TERM`, and `TERM=dumb` remain plain; no spinners or raw input mode
are used when either input or output is not a capable terminal.

| Command | Effect |
| --- | --- |
| `/help` | Show commands |
| `/status` | Session, selected settings, model activity, and operation states |
| `/sessions` | IDs, update times, first user prompt/topic, empty transcripts, and `*` current |
| `/new` | Stop/join current work and open a fresh unsaved chat |
| `/resume` | Open a recent-first cursor chooser; Enter confirms, Esc cancels |
| `/resume ID` | Stop/join current work and load an existing conversation |
| `/cancel ID` | Cancel just that operation; other work continues |
| `/stop` | Stop model generation and all tools, retaining history and the chat |
| Ctrl-C | Clear draft → stop active work → exit when idle |
| `/exit`, `/quit`, or EOF | Stop/join work and close the application |

A normal assistant answer never exits. Ctrl-C at an empty, idle prompt exits.
Idle is observed from the coordinator, including tool grace and pending result
delivery, not inferred from a spinner or the presence of a runtime. Stop records
a durable interruption: resuming does **not** retry the stopped request/tools or
invent an assistant answer. The next user message starts new work in the same
conversation. Cancellation cannot undo edits or external side effects already
performed. Shell cancellation can take a few seconds while process groups exit.
A canceled individual tool may produce a model follow-up; `/stop` never does.
Switching sessions always stops and joins the old runtime first. A missing resume
ID leaves the old session selected and usable, with its work stopped.

### Selecting a conversation

Startup always opens a **fresh, unsaved chat** unless `-session ID` is given.
The first actual user message creates and saves exactly one conversation. Local
commands, `/new`, opening/canceling the chooser, resume, and exit do not create
empty conversations. Old saved sessions, including old empty ones, are retained.
Diagnostic logs can still be created before the first message; these are not
conversations. `/status` identifies an unsaved chat explicitly.

Use **`/resume`** to choose without copying IDs. In a capable terminal:

- Up/Down (or Ctrl-P/N) moves the visible `>` and terminal cursor; Enter resumes
  that exact session; **Esc** dismisses without switching or stopping work.
- Sessions are a recent-first snapshot (ties sort by exact ID), with first-prompt
  text, UTC update time, a `*` current marker, unique row numbers, and a selected
  ID detail row. Repeated topics never substitute one ID for another.
- The list scrolls through all saved sessions and retains selection during
  resize. It shares the editor live area, not terminal scrollback or an alternate
  screen. Async output and task status continue while choosing. Small windows
  prioritize the selected row over live rows, decorations and details; narrow
  rows are clipped. Enlarge the window for full IDs/titles, or use `/sessions`.
- Belarusian/Cyrillic titles remain readable. Controls, bidi formatting, and
  other potentially multi-cell Unicode are escaped in chooser rows. No saved
  prompt can execute terminal controls. Selector text/paste is not a model input.
- With no draft, Ctrl-C stops active model/tools and leaves the chooser open;
  when already idle it exits the chat. Esc cancels only the chooser; Ctrl-D exits
  with cleanup. A standalone Esc is recognized after a short (100 ms) key-sequence
  timeout. Confirming alone invokes the existing stop/join/resume/replay path.

Without a capable TTY, `/resume` prints an explanation and the saved session IDs;
use `/resume ID` to select explicitly. It never waits for invisible arrow input.
`/sessions` and `/resume ID` remain available in all modes. Missing/corrupt
selections report the failure and retain the current session (work stopped on
confirmation), never silently choose another. An empty store reports no saved
sessions. Listing does not select or stop work.

Resume confirms the exact ID and topic and replays its saved messages. An old
empty transcript is labeled explicitly. If the first prompt is literally
`continue`, it is a different conversation with no earlier task. There is no ID
substitution, merging, or implicit startup resume.

History and operation captures are canonical files under `.harness/sessions`.
Resume displays saved user/assistant messages once and does not replay old tool
notices. The model receives the full saved context, including tool results and
provider reasoning state. Terminal output excludes reasoning and raw HTTP
exchanges, preserves useful error causes, redacts configured environment credentials and common token patterns,
and removes terminal control characters. **History/capture files are not
redacted**; protect them like source code and other sensitive workspace data.

## Local logs and diagnostics

Startup and `/status` show both locations, relative to the selected session
storage (so `-session-directory` also relocates logs):

- `<session-directory>/logs/commands/<session-ID>/<operation-ID>.jsonl`:
  command lifecycle records, UTC timestamp, session/operation identity, command,
  working directory, first-observed start, final duration, exit code when known,
  and stdout/stderr capture paths when available. A nonzero exit is a failure.
  Output bytes remain in the canonical capture files, not duplicated here.
- `<session-directory>/logs/diagnostic-*.jsonl`: one file per chat process,
  recording startup, provider requests/completion/cancellation, runtime start/
  stop, operation lifecycle, session-selection failures, and shutdown/failures.
  Failures include component/stage, identifiers, redacted causes, and a stack
  captured at an application/coordinator/provider boundary. No frame logs,
  model requests/responses, private reasoning, or authentication objects.

Files are created private (`0600`), directories `0700`. Command logs follow
**durably saved live operation events**, not transcript replay: resuming does
not re-log old commands. An ongoing operation keeps its operation ID and logged
start across runs. For sessions predating these logs, timing begins when this
chat first observes the operation, not at an invented historical start. UI timers
measure activity observed in this run. Logging failures are reported rather than
silently dropping the audit trail. Logs are local and not automatically rotated;
remove/archive old logs when no chat process is using them.

Report a failure with its diagnostic path and relevant operation ID. Provider
errors keep their actual cause (and error chain), not only a generic login hint.
Panics are captured at the synchronous application boundary, the coordinator
run boundary, and within the provider `Respond` call; this is **not** a global
panic handler for arbitrary provider/harness goroutines. Argument parsing and
failures before log storage can be opened may only have stderr diagnostics.

## Safety and limitations

- Tools run **locally with all permissions of this process**. The prompt says
  discussion is not authorization to edit; explicit implementation requests
  authorize relevant edits/tests. Workspace-root `AGENTS.md` is included.
  This is **not** an enforced read-only mode, sandbox, or approval system.
- Bash (`/bin/sh`) and ViewImage are available. No remote tools, skill discovery,
  background jobs surviving exit, or multiple agents are added by this MVP.
- Use only one process per session; there is no cross-process session lock.
  Forced termination/power loss cannot guarantee child cleanup or rollback.
  Existing unfinished sessions without a durable stop retain normal harness
  recovery behavior. Keep the same workspace when resuming custom storage.
- This is not a full-screen UI or a browser. The Markdown view supports core
  CommonMark, not extensions such as pipe tables or interactive links. Code is
  not reflowed or syntax highlighted. The editor supports
  single-cell Unicode such as Belarusian; complex combining/emoji/wide-glyph
  cursor widths and terminal-specific reflow are limited by `x/term`.
  Messages/piped lines are limited to 1 MiB; the editable view to 4096 cells,
  with explicit rejection rather than truncation. Small pasted text counts
  toward the same editor limit as typing. Folded paste supports long
  code blocks without spanning the screen. Very long typed drafts/activity
  prompts spanning the whole screen are best avoided.
  History navigation belongs to the current process, not the selected session.
- Pipes are supported: commands/lines are processed in order. EOF stops work,
  **not** waits for an answer; scripted callers must keep stdin open until the
  desired answer, then send `/exit`. For a single prompt with wait-until-idle
  JSONL output, use `unreal-agent-runner` instead.
- Provider failures stop work and exit with a sanitized diagnostic rather than
  retrying indefinitely. Restart with `-session ID` after correcting settings.
  Token-shaped redaction is defense in depth, not a secret-detection guarantee.
- No automatic compaction, unlimited context, streaming text, OAuth refresh,
  forks, IDE integration, or permission modes. Start `/new` before context grows
  beyond your chosen model limits.

## Verification

`make test check build` includes race tests and a real CLI subprocess exercised
against a loopback fake Responses API, including a real local shell operation,
steering, Ctrl-C, and exit. Shared-descriptor PTY subprocess tests exercise
slow-drained long output, editing across async events/resize, paste, colors,
EOF, failures and terminal/file-flag restoration. A regression replays the
provided local read-only session fixture when installed (otherwise skipped);
the synthetic long-output PTY test always runs. Log tests cover lifecycle,
resume deduplication, redaction, error chains, boundary stacks and permissions.
Seeded-session PTY tests check a visible moving chooser cursor, scrolling/resize,
async tools and notices, cancel/confirm, missing/corrupt choices, Ctrl-C/EOF,
restored model context, and durable session counts. Unit tests cover recent
ordering, duplicate titles, safe Cyrillic rendering and plain fallback. Markdown
unit/fuzz tests cover redaction and terminal-control safety after entity decoding,
prose wrapping, literal code and plain fallback. Paste tests cover short inline
editing/history, both sides of the 160-character threshold, absolute paths,
control-bearing blocks and whole-draft limits. Interrupt tests cover draft
clearing without cancellation, model/tool stop, rapid clear/stop/exit sequences,
idle/chooser exit, attachment/history preservation, pending input and settled
coordinator idle notifications. Automated checks need neither real credentials
nor a paid model; they do not demonstrate real-provider model availability.
