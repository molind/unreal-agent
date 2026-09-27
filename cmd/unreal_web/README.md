# unreal_web

A private, mobile-friendly web UI for the same local agent and SQLite sessions
used by `unreal_chat`. One Go binary includes the HTML, CSS, JavaScript and API;
there is no Node service, CDN or frontend dependency installation.

```sh
make build
./bin/unreal_web /absolute/path/to/project
```

Open **http://127.0.0.1:8097**. The startup output gives the path to a private
`access-token` file. Enter its contents in the sign-in form. The token stays in an
HttpOnly, SameSite=Strict cookie, not a URL or browser local storage. The default
web state directory is `$XDG_STATE_HOME/unreal-agent/web`, or
`$HOME/.local/state/unreal-agent/web`; override it with `-state-directory`.
Only one server may own a web state directory.

## Tailscale access

Run Tailscale on both the host and phone, connected to the same tailnet. To
listen directly on the host's Tailscale IP, pass it with the port:

```sh
./bin/unreal_web -listen 100.124.20.20:8097
```

Replace the example IP with your host's Tailscale IP. Open
**http://100.124.20.20:8097** on the phone and sign in with the access token.
No Tailscale Serve setup or `-public-url` is needed for this mode. Traffic between
the devices travels over Tailscale's encrypted connection. Browsers treat this
as HTTP, so code blocks may need manual text selection to copy.

`-listen` accepts a specific IPv4 or IPv6 address and port, such as
`192.168.1.20:8097` or `[fd7a:115c:a1e0::1]:8097`. The address must belong to the
host; wildcard addresses (`0.0.0.0`, `::`) are rejected. The server accepts the
chosen IP origin. Without `-listen`, it listens on `127.0.0.1:8097`.

For HTTPS, keep the default loopback listener, enable HTTPS certificates in the
tailnet and run **Tailscale Serve** in a separate terminal:

```sh
tailscale serve http://127.0.0.1:8097
```

Use the HTTPS origin printed by Serve when starting Unreal, for example:

```sh
./bin/unreal_web \
  -public-url https://your-machine.your-tailnet.ts.net \
  /absolute/path/to/project
```

The example hostname must be replaced with your actual Serve hostname. Both the
loopback origin and configured HTTPS origin are accepted. Unknown Host headers,
cross-origin requests and non-JSON mutations are rejected. Serve is private to
the tailnet; its access rules still apply. Do not expose this port through Funnel.

By default, sign in with the access token on each browser. Optionally use
`-tailscale-user your-exact-login@example.com` together with `-public-url` to trust
that specific `Tailscale-User-Login` header from the local Serve proxy. Other
tailnet users still need the token. Tagged devices do not have user identity
headers. This mode trusts other processes on the host, which can reach loopback;
`-tailscale-user` is refused with a non-loopback listener. Direct IP access always
requires the token and does not trust identity headers. Never put an untrusted
proxy in front of the service.

Official references: [Serve](https://tailscale.com/docs/features/tailscale-serve)
and [Serve CLI](https://tailscale.com/docs/reference/tailscale-cli/serve).

## Workspaces and conversations

- **Праекты** lists workspace folders, ordered by their latest conversation.
  Open a folder to see its conversations, newest first, and create a new one.
  **Апошняе** lists conversations across all folders with the project name below
  each title. Both views sort by the latest committed user message or assistant
  reply; tool checkpoints and compaction summaries do not change the order.
  Empty conversations use their creation time until the first message.
- Conversations in the standard `$XDG_STATE_HOME/unreal-agent/workspaces` SQLite
  catalog (or `$HOME/.local/state/unreal-agent/workspaces`) are discovered
  automatically, including projects used only from the CLI. Discovery repeats
  while the UI is open. Custom `-session-directory` databases outside that catalog
  are not searched. An unavailable project is shown with an error; other projects
  remain usable.
- Use **☆** beside a conversation to put it in **Pinned** and **★** to unpin it.
  Pins are shared across browsers and persisted in the web state directory.
  Pinned appears above both root tabs; it is hidden inside a project folder.
  Pinned conversations are omitted from the remaining Recent list, but still
  appear in their project. Pinning or browsing never starts agent work.
- Add existing folders by their **absolute path on the server machine**. Symlink
  aliases resolve to the same workspace. The sidebar lists conversations from
  the existing workspace SQLite database, including CLI-created sessions.
- New conversation creates an empty saved session. Opening its history is
  passive. Send a message or choose **Resume work** to start the agent. After a
  server restart, saved sessions require this explicit action; they never start
  merely because the browser reconnects.
- Sessions run independently. Switching conversations, closing a tab, locking a
  phone or losing a connection does not cancel work. The host must remain awake.
- Send during a running task to steer it. **Stop** cancels/joins model and tool
  work while retaining the session lease and history. **Release** also gives up
  ownership so `unreal_chat -session ID WORKSPACE` can take over. A session already
  owned by another CLI/server is refused, never stolen. Different sessions in
  the same folder share its files; use separate folders/worktrees for isolation.
- Markdown, tables, copyable code, tool results and file diffs are displayed.
  Raw HTML and external images are disabled. Output previews are bounded;
  canonical history and artifacts remain on disk.
- Compaction approval applies to the displayed request only. New messages do not
  grant compaction permission. You may cancel a single active operation.

Messages support up to 1 MiB of UTF-8 text. The browser saves drafts and pending
message IDs in local storage on that device. If sending is interrupted, **Retry**
uses the same ID. The server acknowledges only a committed input and checks
persisted IDs on restart. Reusing an ID for different text is rejected. Drafts
and pending messages therefore contain conversation text on the browser device;
use a trusted browser profile.

## Model configuration

The existing provider authentication and environment variables are used on the
server. Credentials never need to be entered on the phone. See the
[chat authentication documentation](../unreal_chat/README.md#authentication-and-configuration).

The flags `-provider`, `-model`, `-reasoning-effort`, `-transport`, `-base-url`, and
`-max-attempts` override the same `unreal_chat` settings. Defaults are
`openai-codex / gpt-6-astra / xhigh`. Current server settings apply when a saved
session is resumed, as with CLI flags. The first version does not have a per-chat
model editor. Renew expired Codex credentials externally and restart the server.
It does not load `.env` files. Tools have the launching user's local permissions;
selecting a workspace is not a filesystem sandbox.

## Manual start and stop

Start Unreal yourself when needed; it does not start at login:

```sh
./bin/unreal_web /absolute/path/to/project
```

Keep that process running while using the web UI. Closing a browser leaves agent
work running; Ctrl-C in the server's terminal stops and joins the active work.
Saved conversations remain available for the next manual start. The host must
remain awake.

For direct Tailscale IP access, include `-listen` as shown above. If using HTTPS,
keep the separate Tailscale Serve process running as well.

## Protocol and verification

`GET /api/events` is a coalescible SSE invalidation stream. It carries no history
or model output. On reconnect, the browser requests committed history pages
after its last sequence and refreshes the current session status. A five-second
notification also catches changes made by another local host. Slow subscribers
have bounded write deadlines and never block runtime event consumption.

The API has project registration/listing, idempotent session creation by UUID,
session listing/history, and explicit `send`, `resume`, `stop`, `release`,
`compact`, and `cancel` actions. Runtime lifetime belongs to the server, never
the HTTP request context. SIGINT/SIGTERM stop/join work before storage closes.

```sh
go test -race ./cmd/internal/chat ./cmd/internal/webchat ./cmd/unreal_web
go vet ./cmd/internal/chat ./cmd/internal/webchat ./cmd/unreal_web
node --check cmd/internal/webchat/static/app.js
```

Tests cover durable retries across restart, independent sessions, canonical
paths, CLI lease exclusion/handoff, authentication/Origin checks, safe Markdown,
SSE disconnect and shutdown, SQLite project discovery, message-based ordering,
and persistent pins across projects. Replies currently arrive on model completion;
token streaming, file uploads and push notifications are outside this version.
