# x/term line editor with transient status rows

`terminal.go` is copied from `golang.org/x/term v0.46.0/terminal.go` under
its included BSD license. We continue to use upstream x/term for terminal mode
and size APIs. This local extension is necessary because upstream exposes only
a single-line `SetPrompt`, not an atomic multiline live area API. Do not replace
it with newline-containing prompts or a competing cursor renderer.

Local changes: `SetStatus` (`status.go`) runs under the existing editor lock;
status rows share its cursor origin, disappear before Enter, and are repainted
around asynchronous Write. Prompt remains `you> `. Rows are bounded by height,
clipped by width, and an overflow row reserves access to `/status`. Growing drafts
shrink/hide status before it can scroll into history. Erase clears the entire
editable tail (the prior chat transport did this substitution). Resize uses the
physical origin of explicit-CRLF rows instead of the upstream doubled-row shrink
heuristic. Editing, key parsing, paste callback, history, and serialization remain
upstream-owned. Compare against the pinned module source when updating x/term.

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
