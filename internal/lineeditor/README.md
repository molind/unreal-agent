# x/term line editor with transient status rows

`terminal.go` is copied from `golang.org/x/term v0.46.0/terminal.go` under
its included BSD license. We continue to use upstream x/term for terminal mode
and size APIs. This local extension is necessary because upstream exposes only
a single-line `SetPrompt`, not an atomic multiline live area API. Do not replace
it with newline-containing prompts or a competing cursor renderer.

Local changes: `SetStatus` (`status.go`) runs under the existing editor lock;
status rows share its cursor origin, disappear before Enter, and are repainted
around asynchronous Write. `SetPromptInfo` adds a clipped context/rule row in
that same region, with optional dim decorations and an accented `you> ` prompt.
The prompt's text/visible width do not change. ANSI is queued separately from
prompt glyphs so narrow wrapping cannot split an escape sequence. Context is
hidden in selection mode and removed with status before Enter. Rows are bounded by height,
clipped by width, and an overflow row reserves access to `/status`. Growing drafts
shrink/hide status before it can scroll into history. Erase clears the entire
editable tail (the prior chat transport did this substitution). Resize uses the
physical origin of explicit-CRLF rows instead of the upstream doubled-row shrink
heuristic. Editing, paste callback, history, and serialization remain upstream-owned.
Key decoding additionally recognizes Meta-b/f and Meta-modified CSI arrows
alongside upstream's Alt arrows, all routed to the existing word-motion handlers. Compare against the pinned module source when updating x/term.

As upstream, editable Unicode uses one cell per rune: Cyrillic works; wide and
combining input is not fully supported. Status callers supply printable ASCII.
Drafts taller than the terminal retain the upstream limited scrolling behavior;
folded paste is recommended for large text. No alternate screen is used.

`OpenSelection(title, choices)` (`selection.go`) is a transient input mode under the same lock,
not another reader or renderer. `ReadLine` returns a typed `Selection` result
with an opaque value or explicit cancellation, without submitting/remembering keys; draft/cursor and live status are retained.
The chat input transport frames a timed-out standalone Escape as ESC ESC, leaving
partial arrow sequences buffered. Application handoff acknowledges each line
before the reader proceeds, so immediately queued arrows belong to the chooser.
Rows are viewport-bounded and removed before output/confirmation/cancel. Selected
indices are retained during resize; opaque values preserve exact session identity.
ASCII/Cyrillic chooser text is single-cell, with controls/other Unicode escaped.

`ClearDraft` is called between `ReadLine` calls after the chat transport reports
an interruption. It resets the draft, partial key bytes and pending history
navigation under the render lock, preserving submitted history, live status and
an open chooser. Interrupt keys use the same acknowledged application handoff as
submitted lines, so a later key cannot race ahead of a stop/join decision.

Multiline drafts use a source-rune layout and a height-bounded viewport under the
same editor lock. Option+Return / Ctrl-J insert LF at column zero; Up/Down navigate
text columns while Ctrl-P/N retain explicit history navigation. Single-line
editing keeps the upstream incremental fast path. No mouse reporting, copy
hitboxes, clipboard controls, or alternate screen are used.
