# SQLite compaction replay regression

Reproduced with synthetic nonalphabetically ordered opaque provider JSON. The
format-1 JSON artifact writer decoded to map[string]any and marshaled again.
It preserved semantic JSON values but changed Raw item representation. Prefix
hashing includes those raw bytes, so a saved compaction could not replay.

Fix:
- JSON artifact format 2 is a byte-exact sequence of literal spans and referenced
  large encoded string tokens. Keep the format-1 reader; reject unknown versions.
- SQL record writers strip only their JSONL delimiter, not JSON representation.
- Read old imported history through fingerprint-verified raw journal archives,
  checking semantic equality before substituting original record bytes. Preserve
  newer SQLite events and never rewrite original event rows or checkpoint hashes.
- If there is no recoverable original, chat explicitly warns and ignores a stale
  derived summary during replay, keeping the full canonical transcript. Do not
  apply an unverified summary. Live validation and non-projection failures remain
  strict. Later matching checkpoints must still apply.
- Stop giving blanket credential-renewal advice for unrelated runtime failures.

Tests use only synthetic provider state, never user conversations or credentials.
Cover exact bytes/escapes/numbers/order, old reader compatibility, original-archive
read recovery, changed/corrupt archive rejection, stale/no-original recovery,
strict live validation, diagnostic failures and valid later checkpoints.
The reported user database must not be edited to replace hashes or delete history.
