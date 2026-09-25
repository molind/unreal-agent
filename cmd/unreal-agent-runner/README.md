# unreal-agent-runner

Run an AI agent from a prompt or JSON request. It writes events to stdout as
JSONL and exits when the task finishes.

Install with Go 1.27+:

```sh
go install github.com/unreallabsai/unreal-agent/cmd/unreal-agent-runner@latest
```

Set an OpenAI API key and run a prompt in the current directory:

```sh
export OPENAI_API_KEY="..."
unreal-agent-runner -p 'Inspect this project and explain how to run its tests.'
```

Or run from source at the repository root:

```sh
go run ./cmd/unreal-agent-runner -p 'Inspect this project and explain how to run its tests.'
```

Choose a workspace and save the output:

```sh
unreal-agent-runner -workspace ./my-project -p 'Summarize this project.' > run.jsonl
```

Sessions default to legacy JSONL at
`${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/sessions`
(override with `-session-directory`). This compatibility default is unchanged for
benchmark/script consumers. Select `-storage-format sqlite` to use the same XDG
workspace database, compressed artifacts, and file revision/receipt storage as
`unreal_chat`. With SQLite and no directory override, the path is
`${XDG_STATE_HOME:-$HOME/.local/state}/unreal-agent/workspaces/<workspace-id>`.
SQLite runners and chats may work concurrently in one workspace, with one owner
per session. Resuming an already-owned session fails before starting operations.
Migration remains exclusive; close older exclusive-writer binaries after upgrading.
Shared workspace files are not isolated (use git worktrees for independent Bash,
build or git activity). Standard output remains JSONL regardless of the storage
backend. See
[storage and migration](../unreal_chat/README.md#sqlite-storage-and-migration) for
automatic verified legacy migration/cleanup, `unreal-storage` inspection,
artifact export and backup. SQLite startup migrates workspace `.harness/sessions`
and legacy files in the chosen directory. The old shared runner-default directory
is not auto-imported: it may contain sessions from several workspaces. Migrate such
stores explicitly only after checking their workspace ownership.

You can also pass a JSON request as an argument or through stdin:

```sh
unreal-agent-runner '{"prompt":"Summarize this project."}'
unreal-agent-runner < request.json
```

OpenAI is the default provider. Set `UNREAL_HARNESS_LLM_PROVIDER` to `openai`,
`openai-codex`, `openrouter`, `fireworks`, or `ollama`, and
`UNREAL_HARNESS_LLM_MODEL` to choose a model.

Run `unreal-agent-runner -h` for options and the JSON request fields.

## Docker

The `unrea1labs/unreal-agent` image supports Linux on AMD64 and ARM64. Run it
with a project mounted as the workspace:

```sh
docker run --rm -i --user "$(id -u):$(id -g)" \
  -e OPENAI_API_KEY -v "$PWD:/workspace" \
  -v unreal-agent-state:/state \
  unrea1labs/unreal-agent:latest -p 'Summarize this project.'
```

Each release also publishes its Git tag (for example, `v0.1.0`) for version pinning.

### Structured files and transport

The default local tools also include Read, Edit and Write. File mutations use
short session-scoped revision references backed by full file digests, exact
replacement checks and durable receipts; see the [chat documentation](../unreal_chat/README.md#structured-file-operations).
They can be disabled by name in `disallowed_tools`.
For first-party OpenAI/Codex the Responses adapter defaults to automatic
incremental WebSockets. Set `UNREAL_HARNESS_LLM_TRANSPORT=http`, `websocket`, or
`auto` to override for those providers. Custom endpoints default to HTTP.
The response's optional Transport metadata reports actual full/delta payloads.
