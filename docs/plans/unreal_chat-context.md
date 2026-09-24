# Rolling context management

Implemented as opt-in coordinator recovery, enabled by the interactive chat.

## Boundaries

- `contextbuilder` is I/O-free: estimate requests, select an answered prefix,
  produce a bounded tool-free summarization request, validate/apply checkpoints.
- `coordinator` owns scheduling, stop/steer handling, input-delivery accounting,
  append-only checkpoint persistence and status observation. A maintenance turn
  never acknowledges ordinary pending input or schedules summarizer tools.
- `session.Turn.Compaction` is an optional, versioned prefix descriptor. A matching
  successful model response supplies the summary. Hashes exclude current system
  instructions, include the exact historical prefix, and are checked on replay.
  Old sessions without descriptors remain readable. Checkpoint histories must be
  resumed with a compaction-capable builder; unknown versions fail explicitly.
- The original file stays append-only. Applying a checkpoint replaces only the
  model-facing prefix. No transcript/capture deletion, hidden retry, or silent
  clipping of pending input. Previous summaries participate in later prefixes.
- Latest two response blocks, unanswered input, and entire crossing/active
  call/result spans remain intact. Existing tools continue during summarization.
- Runtime remains responsible for cancel/join and the durable stop barrier after
  context errors. Only pure recoverable context failures keep the chat open;
  additional storage, logging, or output failures must still surface as fatal.

## Trigger and approval lifecycle

No guessed context window or proactive threshold. Requests go to the provider
regardless of the informational size estimate. The first explicit overflow
persists a `context_overflow` control and authorizes one maintenance attempt.
Permission is consumed at the compaction turn, not on summary success. A second
overflow (ordinary or maintenance) creates a durable request-scoped approval gate.
Only a matching `approve_compaction` control can grant one further attempt.
The shared chooser submits that control on Yes; default No uses stop-and-discard.
Escape defers, `/compact` reopens; plain mode uses `/compact yes|no`. Scoped
selection identities prevent stale results and session/approval confusion. The
menu does not displace an active resume picker or destroy the draft. Ordinary
text, tool completion, stale approvals and process restarts do not grant consent.

Only a successful ordinary response ends the episode. Stops clear outstanding
permission but do not reset the automatic-attempt allowance; new user input can
resume ordinary work but cannot bypass the next approval. Pending approval blocks
model scheduling, not operation updates. New input during maintenance is queued,
not used to restart it. A failed summary request can use a strictly smaller safe
prefix only after approval. Lack of a safe prefix stops with guidance.

The prefix is roughly the oldest half by estimated size with whole response and
call/result boundaries. Latest two blocks and unanswered input remain exact.
Summary size bounds are output handoff limits, not a guessed provider window.
Completed checkpoint descriptors and response formats remain compatible with
already saved sessions. Context controls are not added to the model conversation.

## Validation

Tests cover large accepted requests, real overflow triggers, active operations,
queued steering and stop/late responses, durable checkpoint replay, one-shot
request-scoped approvals, duplicate/stale approval rejection, restart and decline,
summary overflow and smaller-prefix retry, failure recovery and /new. CLI/PTY
checks verify default No, Yes, Escape, stop, reopening and draft/cursor preservation. Full race and terminal
regression suites remain enabled. No test depends on paid APIs or credential files.
