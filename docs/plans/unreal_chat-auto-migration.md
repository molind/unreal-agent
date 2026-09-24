# Automatic legacy migration and cleanup

User request: migrate sessions automatically and remove .harness afterwards.

SQLite startup checks workspace .harness/sessions and an explicit storage
directory. Import idle sources automatically, verify exact source bytes and the
canonical imported event prefix, then remove only recognized migrated files.
Prune .harness only if empty; preserve skills, experiments and all unknown files.
Never RemoveAll a user directory or follow project-local storage symlinks.

Keep raw originals as compressed database artifacts (including torn JSONL tails).
A durable versioned cleanup manifest survives partial deletion and makes retries
idempotent. Verify every archive before any deletion; changed sources, corrupt
archives and failed imports leave sources untouched. Already imported histories
may have newer SQLite events: compare the original prefix, never overwrite them.

Hold a source-directory lock shared by new legacy hosts and exclusive for
migration. Inspect old processes' open descriptors too (Linux /proc; macOS lsof).
Fail closed if inspection cannot be completed. Older runner binaries without
persistent descriptors require a conservative executable-name check. Advisory
locking cannot protect against arbitrary third-party writers ignoring the lock.

Before deleting an interrupted shell's old spools, materialize them privately in
the SQLite operation directory and append a rebased recovery checkpoint. Do not
rewrite historical events/tool text/compaction hashes or repeat unknown commands.
In-place migration gives unfinished operations fresh .migrated spool paths and
leaves the SQLite database itself in place. File receipts remain in SQLite namespaces.

Test: startup/resume, idempotence, partial cleanup, already imported/newer history,
metadata/diff/output retention, torn tails, corrupt archives, changed files,
symlinks/unknown/skills preservation, active new and pre-lock processes, pending
shell recovery, in-place destination preservation, CLI keep-source compatibility.
Do not migrate this currently running development session during implementation.
