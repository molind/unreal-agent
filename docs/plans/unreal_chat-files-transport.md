# Structured file tools and incremental transport

Implemented independently of context compaction; both continue to use the
existing coordinator, append-only sessions and cancellation/operation lifecycle.

## File operations

Read/Edit/Write are real tool definitions with pure translators and a versioned
`file` operation. Bounded I/O runs in a joined blocking worker, not in translation
or the coordinator. Read supplies a random 16-hex revision reference. Its private
binding contains the canonical path and full SHA-256. Thus wire cost is small
without truncating the actual integrity comparison. Edit checks an exact unique
match (or explicit replace_all); Write creates only when missing unless supplied
with a valid revision for the target.

Existing mutations use advisory inode locks, full-digest/identity rechecks and
atomic replacement. Creation uses an atomic link from a prepared temporary file.
Directory handles pin parent resolution. Reject nonregular, symlink, binary,
oversized, read-only and hard-linked mutation targets. Full safety against other
programs ignoring locks is not claimed. Ordinary modes/newlines are preserved,
not ACLs/xattrs/inode identity. Cancellation cannot undo a committed file.

Private prepared/completed receipts prevent blind repeated mutations after
recovery. Completed results replay without writes; prepared uncertain outcomes
fail rather than guessing. Full diffs are captured; bounded sanitized previews
are shown once after durable completion. Lifecycle logs never copy source text.

## Transport

Responses WS v2: `response.create`, `store:false`, session cache affinity,
`OpenAI-Beta: responses_websockets=2026-02-06`. First-party defaults auto; other
providers/custom HTTP compatibility endpoints are not assumed WS-capable.

A per-adapter serialized, cancellable connection owns continuation state. Exact
serialized prefix + previous normalized output and matching request properties
are required for delta input + previous_response_id. Mutations, model/settings/
session changes, compaction, cancellation or failure invalidate it. Full history
remains local. No provider response ID is assumed durable across processes.

A bounded reader handles control frames while tools run, and is joined on reset.
Explicit missing-reference/expired-connection errors before generation allow one
full resync. No retry of partial/ambiguous work. Auto HTTP fallback only follows
unsupported handshake status, not auth/quota/policy/context errors. Credentials
are never forwarded across redirects. No token refresh, automatic paid API smoke
call, or claim of reduced model context/billing.

/status and diagnostic counters expose actual full/delta request sizes, without
logging request bodies. Tests include a real CLI Read/Edit flow over loopback WS,
file conflicts/recovery, wire prefix behavior, pings, cancellation, resync,
redirects and HTTP fallback. Public Codex reference protocol was inspected at
https://github.com/openai/codex/tree/main/codex-rs/codex-api/src/endpoint
without reading local credential files.
