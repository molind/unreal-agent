# Workspace storage

`storage` owns the SQLite handle, content-addressed artifacts and private file
metadata primitives. It does not depend on the coordinator, session, tools or
operation packages. The existing `sessionstore/localfile` validation/replay engine
supports two persistence backends: `New` (legacy JSONL) and `NewSQLite` (indexed
SQLite). Both implement the same session-store interface and observer contract.

## Layout and ownership

One `state.sqlite3` per canonical workspace. XDG_STATE_HOME must be absolute;
otherwise HOME/.local/state is used. The workspace directory name is SHA-256 of
its canonical absolute path. Symlink aliases resolve to the same identity;
moving a workspace requires an explicit migration/rebind design rather than
silently running old operations in another directory. Explicit directories are
bound to that same identity before a host resumes work.

SQLite is pure Go (`modernc.org/sqlite`); no sqlite executable or CGo is required.
Main database permissions are 0600, new storage directories 0700. SQLite inherits
that mode for WAL/SHM. Only local filesystems supporting locking are supported.
SQLite transactions use WAL, synchronous=FULL, foreign keys and a bounded busy
timeout. Version mismatches fail instead of guessing at migrations. A workspace
writer lock covers the whole host process, including external filesystem edits;
SQLite transaction locks alone cannot make those edits safe. Readers and backups
may run concurrently. This first version allows one harness writer per workspace,
not several simultaneous chat processes in different sessions of one workspace.

## Canonical history and artifacts

`events` is append-only and indexed by session/event number and logical item
sequence. `operations` contains the latest state, updated in the same transaction
as the event. Replay reads logical items and latest projections, not every
transient operation checkpoint. Canonical JSONL export includes all checkpoints.
A cached writer checks its high-water mark before appending; stale writers fail
without truncating a newer history. SQLite observers run only after commit.

JSON artifact format 2 preserves **every input byte**, including object order,
whitespace, number spelling and escape sequences. Its envelope contains exact
literal spans and references to large encoded string tokens (including encoded
image bytes). User JSON is never re-encoded as a map and cannot masquerade as an
internal reference. Deduplication and decompression are transparent to the
in-memory, I/O-free context builder. Bash's artifact-reference rendering mode is
persisted with each operation so replay never changes historical result text.

The initial format-1 writer normalized JSON object order. This was a bug: opaque
provider JSON participates in the compaction prefix hash. The reader remains
backward compatible. For old imports it restores the read view from an archived
original journal only after verifying the import fingerprint and semantic equality
of each affected record. Newer SQLite events are not replaced and no history rows
are rewritten. Without an original, the lost byte representation cannot be guessed.
Chat can then ignore only a stale derived compaction, retain the full canonical
transcript, and visibly warn. The summary is never applied without a matching hash;
malformed records, corrupted archives, storage and audit failures remain fatal.
Live compaction validation remains strict. Other hosts explicitly opt into replay
fallback with `RecoverStaleCompactions` and `OnCompactionSkipped`.
This is lossless storage compression, separate from conversation summarization.

Artifacts consist of immutable 128 KiB chunks, compressed with zstd only when
smaller, and checksummed with SHA-256. Range reads decompress only touched chunks.
`artifact:<sha256>` addresses content; `capture:<sha256-of-original-spool-path>` is
a stable alias that survives unlinking and database backup. Empty outputs do not
allocate chunk rows. Large artifacts are streamed rather than loaded in full.

## External effects and recovery

Bash still redirects to regular durable spool files. A terminal checkpoint first
imports closed captures, then commits the operation, then unlinks verified spools.
The import transaction and immutable alias make retries safe; changed spools are
never silently rebound or deleted. Captures of possibly surviving processes are
retained. Source bytes never disappear before the database owns a complete copy.
File-tool revisions and receipts use namespaced metadata rows; complete diffs are
artifacts. The per-operation receipt lock remains necessary (in-memory stripes
within a process, plus the host writer lock across processes). Filesystem edits
still use staged-file fsync, atomic rename/create, directory fsync, and separate
intent/completion receipts. An uncertain interrupted edit is NOT repeated.

## Automatic migration and source cleanup

SQLite hosts automatically migrate idle workspace `.harness/sessions` and legacy
files in an explicit destination. `MigrateLegacy` takes an exclusive directory
lease; JSONL hosts hold shared leases until all logging/tools are closed. Older
pre-lock binaries are checked via open process descriptors (Linux `/proc`, or
`lsof` plus executable names from `ps` on other Unix hosts). Inspection failures
fail closed. Old one-shot runners may hold no descriptor while waiting on the
provider, so their executable name triggers a conservative busy result. Custom
third-party writers that ignore advisory locks must be stopped by the user.

Recognized source files are archived exactly, including uncommitted JSONL tails,
and recorded in a durable `legacy-cleanup-v1` metadata manifest. Before any unlink,
all archives are checksum-verified and imported canonical event prefixes compared
against the source journals. Cleanup rechecks source hashes/identity and removes
only known files, then prunes empty directories. Skills, experiments, unknown
files and an in-place SQLite destination survive. A partial cleanup resumes from
the manifest even if the original journals are already gone. Raw backups remain
in compressed artifacts addressable through their original capture aliases.

Unfinished shell checkpoints are rebased only after new spool copies are synced;
old events/tool text stay unchanged. In-place migrations use fresh `.migrated`
spool paths so a resumed command cannot mutate an immutable source capture alias.
The lower-level `ImportLegacy` and CLI `-keep-source migrate` retain source files;
a later normal startup can still verify and clean that already-imported history.

## Maintenance

Use `unreal-storage backup` for a consistent standalone SQLite snapshot, including
committed WAL contents. Do not copy just the live `.sqlite3` file. A snapshot of an
active workspace does not include uncommitted external spool bytes or the workspace
files themselves: stop work first for a complete portable archive.

No history or artifact retention policy deletes data automatically. SQLite does
not shrink on row deletion without maintenance; garbage collection, cross-workspace
deduplication, FTS search and standalone single-session database export are not
part of this first version. `history SESSION` exports a session's JSONL transcript;
`backup FILE` includes the entire workspace database, including other sessions.
