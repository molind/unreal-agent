# Read-only report viewer

Keep context accounting and storage unchanged. On capable TTYs, /status, /help
and /sessions open a read-only snapshot in the terminal alternate screen.
Esc/q/Ctrl-C dismiss it, restoring primary transcript, draft and cursor. Arrows,
PageUp/PageDown, Home/End and Space navigate; commands/paste are not submitted.
Plain pipes and TERM=dumb retain normal text output. /resume remains a chooser.

Use the existing line-editor reader and render lock, never another stdin reader
or an external pager racing with it. Clear only the editable primary-screen tail
before entry. Resize rewraps the snapshot and preserves an approximate content
anchor. Live status continues updating in memory. Asynchronous conversation output
is queued and flushed to the primary transcript on close; if the bounded queue
fills, close the viewer and deliver all output instead of losing it. Reports have
an explicit size bound/truncation notice. Never interpret report text as ANSI.

Viewer close events are distinct from user messages and session selections.
Compaction approval menus defer while a viewer is open and may open after it is
closed. EOF, output errors and application shutdown must leave alternate mode and
restore terminal settings. Tests cover scrolling, tiny sizes, drafts/cursor,
paste/key isolation, async output/order/overflow, menu exclusion, redaction,
plain-output compatibility and real PTY primary/alternate-screen restoration.
