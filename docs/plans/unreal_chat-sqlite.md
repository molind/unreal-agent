# Workspace SQLite storage

Implement the agreed storage design in dev. Keep the legacy JSONL reader and
its replay validation, and add an indexed SQLite backend. Default chat state is
XDG_STATE_HOME/unreal-agent/workspaces/<hash of canonical workspace>/state.sqlite3.
An explicit session directory remains supported. One harness writer per workspace;
readers/exports remain possible. Workspace identity is checked before resume.

Store immutable events, latest operation projections, diagnostics, file revision
bindings and edit receipts in SQLite. Large JSON strings are content-addressed
without changing their decoded values; artifact streams use 128 KiB independently
compressed/checksummed blocks. JSON numbers must round-trip without float loss.

Shells keep regular durable spool files while active. Import before recording a
terminal checkpoint, and unlink only after the checkpoint commits. Retry import
and cleanup idempotently; do not touch captures of possibly surviving processes.
Old captures are retained as migration backups. File mutation intent and result
remain separate durable commits around atomic filesystem replacement; never claim
that a SQLite transaction makes external file edits atomic.

Expose artifact/capture references to tools, bounded artifact reading, and a CLI
for read/export, diagnostics, integrity checking, consistent backup and explicit
legacy import. Migration must validate replay, be transactional for each history,
detect changed legacy sources, and leave source files untouched. Do not migrate
this currently running development session automatically as part of testing.

Tests: backend parity/reopen/fork, failed transactions and cancellation, paging,
number/JSON round-trip, dedup/compression, chunk corruption, interrupted capture
import/cleanup, revision and receipt recovery, XDG resolution and workspace
isolation, migration retries/conflicts, CLI exports, end-to-end chat and runner.
