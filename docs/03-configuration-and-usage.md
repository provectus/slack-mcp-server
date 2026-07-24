## 3. Configuration and Usage

You can configure the MCP server using command line arguments and environment variables. This fork is installed as a prebuilt binary (see [Installation](02-installation.md)) and connected to your agentic tool manually — via `stdio` for a client-launched server, or via `sse` for the background service.

### Connecting from MCP clients (stdio)

Point your MCP-capable tool at the installed binary. For [Claude Desktop](https://claude.ai/download), open `claude_desktop_config.json` and add the server to `mcpServers` (use the absolute path to the binary — `~` is not expanded):

```json
{
  "mcpServers": {
    "slack": {
      "command": "/Users/you/.local/bin/slack-mcp-server",
      "args": ["--transport", "stdio"],
      "env": {
        "SLACK_MCP_XOXP_TOKEN": "xoxp-..."
      }
    }
  }
}
```

Token alternatives for the `env` block (see [Authentication Setup](01-authentication-setup.md)):

- `SLACK_MCP_XOXP_TOKEN` — user OAuth token
- `SLACK_MCP_XOXB_TOKEN` — bot token (limited access: invited channels only, no search)
- `SLACK_MCP_XOXC_TOKEN` + `SLACK_MCP_XOXD_TOKEN` — browser session token + cookie

For Claude Code:

```bash
claude mcp add slack -e SLACK_MCP_XOXP_TOKEN=xoxp-... -- ~/.local/bin/slack-mcp-server --transport stdio
```

The same `command`/`args`/`env` shape works in any tool that supports MCP servers (Cursor's `~/.cursor/mcp.json`, Windsurf, etc.).

> [!WARNING]
> If you are using Enterprise Slack, you may set `SLACK_MCP_USER_AGENT` environment variable to match your browser's User-Agent string from where you extracted `xoxc` and `xoxd` and enable `SLACK_MCP_CUSTOM_TLS` to enable custom TLS-handshakes to start to look like a real browser. This is required for the server to work properly in some environments with higher security policies.

### Connecting over SSE

If you installed with `--with-service`, the server already runs in the background (launchd on macOS, systemd user unit on Linux) with the `sse` transport on `127.0.0.1:13080`. Connect clients through the `mcp-remote` wrapper:

```json
{
  "mcpServers": {
    "slack": {
      "command": "npx",
      "args": [
        "-y",
        "mcp-remote",
        "http://127.0.0.1:13080/sse",
        "--header",
        "Authorization: Bearer ${SLACK_MCP_API_KEY}"
      ],
      "env": {
        "SLACK_MCP_API_KEY": "my-$$e-$ecret"
      }
    }
  }
}
```

<details>
<summary>Or, sse transport for Windows.</summary>

```json
{
  "mcpServers": {
    "slack": {
      "command": "C:\\Progra~1\\nodejs\\npx.cmd",
      "args": [
        "-y",
        "mcp-remote",
        "http://127.0.0.1:13080/sse",
        "--header",
        "Authorization: Bearer ${SLACK_MCP_API_KEY}"
      ],
      "env": {
        "SLACK_MCP_API_KEY": "my-$$e-$ecret"
      }
    }
  }
}
```
</details>

### TLS and Exposing to the Internet

There are several reasons why you might need to setup HTTPS for your SSE.
- `mcp-remote` can handle only https schemes for non-localhost endpoints;
- it is generally a good practice to use TLS for any service exposed to the internet;

You could use `ngrok`:

```bash
ngrok http 13080
```

and then use the endpoint `https://903d-xxx-xxxx-xxxx-10b4.ngrok-free.app` for your `mcp-remote` argument.

### Console Arguments

| Argument                    | Required ? | Description                                                                                                                                                                                                         |
|-----------------------------|------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--transport` or `-t`       | Yes        | Select transport for the MCP Server, possible values are: `stdio`, `sse`                                                                                                                                            |
| `--enabled-tools` or `-e`   | No         | Comma-separated list of tools to register. If not set, read-only tools and usergroups tools are registered by default; write tools require their tool-specific env var (e.g., `SLACK_MCP_ADD_MESSAGE_TOOL`) or explicit listing in `SLACK_MCP_ENABLED_TOOLS`. Available tools: `conversations_history`, `conversations_replies`, `conversations_add_message`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_search_messages`, `conversations_unreads`, `conversations_mark`, `conversations_draft_message`, `conversations_join`, `conversations_leave`, `channels_list`, `channels_me`, `usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `users_search`, `saved_list`, `saved_update`, `saved_clear_completed`. |

### Environment Variables

| Variable                          | Required? | Default                   | Description                                                                                                                                                                                                                                                                               |
|-----------------------------------|-----------|---------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `SLACK_MCP_XOXC_TOKEN`            | Yes*      | `nil`                     | Slack browser token (`xoxc-...`)                                                                                                                                                                                                                                                          |
| `SLACK_MCP_XOXD_TOKEN`            | Yes*      | `nil`                     | Slack browser cookie `d` (`xoxd-...`)                                                                                                                                                                                                                                                     |
| `SLACK_MCP_XOXP_TOKEN`            | Yes*      | `nil`                     | User OAuth token (`xoxp-...`) — alternative to xoxc/xoxd                                                                                                                                                                                                                                  |
| `SLACK_MCP_PORT`                  | No        | `13080`                   | Port for the MCP server to listen on                                                                                                                                                                                                                                                      |
| `SLACK_MCP_HOST`                  | No        | `127.0.0.1`               | Host for the MCP server to listen on                                                                                                                                                                                                                                                      |
| `SLACK_MCP_API_KEY`           | No        | `nil`                     | Bearer token for SSE and HTTP transports                                                                                                                                                                                                                                                            |
| `SLACK_MCP_PROXY`                 | No        | `nil`                     | Proxy URL for outgoing requests                                                                                                                                                                                                                                                           |
| `SLACK_MCP_USER_AGENT`            | No        | `nil`                     | Custom User-Agent (for Enterprise Slack environments)                                                                                                                                                                                                                                     |
| `SLACK_MCP_CUSTOM_TLS`            | No        | `nil`                     | Send custom TLS-handshake to Slack servers based on `SLACK_MCP_USER_AGENT` or default User-Agent. (for Enterprise Slack environments)                                                                                                                                                     |
| `SLACK_MCP_SERVER_CA`             | No        | `nil`                     | Path to CA certificate                                                                                                                                                                                                                                                                    |
| `SLACK_MCP_SERVER_CA_TOOLKIT`     | No        | `nil`                     | Inject HTTPToolkit CA certificate to root trust-store for MitM debugging                                                                                                                                                                                                                  |
| `SLACK_MCP_SERVER_CA_INSECURE`    | No        | `false`                   | Trust all insecure requests (NOT RECOMMENDED)                                                                                                                                                                                                                                             |
| `SLACK_MCP_ADD_MESSAGE_TOOL`      | No        | `nil`                     | Enable message posting via `conversations_add_message` by setting it to `true` for all channels, a comma-separated list of channel IDs to whitelist specific channels, or use `!` before a channel ID to allow all except specified ones. If empty, the tool is only registered when explicitly listed in `SLACK_MCP_ENABLED_TOOLS`. |
| `SLACK_MCP_ADD_MESSAGE_MARK`      | No        | `nil`                     | When `conversations_add_message` is enabled (via `SLACK_MCP_ADD_MESSAGE_TOOL` or `SLACK_MCP_ENABLED_TOOLS`), setting this to `true` will automatically mark sent messages as read.                                                                                                        |
| `SLACK_MCP_ADD_MESSAGE_UNFURLING` | No        | `nil`                     | Enable to let Slack unfurl posted links or set comma-separated list of domains e.g. `github.com,slack.com` to whitelist unfurling only for them. If text contains whitelisted and unknown domain unfurling will be disabled for security reasons.                                         |
| `SLACK_MCP_DRAFT_MESSAGE_TOOL`    | No        | `nil`                     | Enable native draft create/update via `conversations_draft_message`: `true` for all channels, a comma-separated channel-ID whitelist, or `!Cxxxx` negation. Non-destructive by default: an existing draft at the destination is returned, not overwritten, unless the caller passes `overwrite: true`. Requires a session token (`xoxc`/`xoxd`); not available with bot tokens. Drafts are saved to the user's Drafts and never auto-sent. |
| `SLACK_MCP_USERS_CACHE`           | No        | `.users_cache.json`       | Path to the users cache file. Used to cache Slack user information to avoid repeated API calls on startup.                                                                                                                                                                                |
| `SLACK_MCP_CHANNELS_CACHE`        | No        | `.channels_cache_v2.json` | Path to the channels cache file. Used to cache Slack channel information to avoid repeated API calls on startup.                                                                                                                                                                          |
| `SLACK_MCP_CACHE_TTL`             | No        | `24h`                     | Cache time-to-live. Supports duration format (`24h`, `30m`) or seconds (`3600`). Set to `0` to disable TTL (cache forever). When the cache expires, stale data is served immediately while a background refresh fetches fresh data.                                                       |
| `SLACK_MCP_MIN_REFRESH_INTERVAL`  | No        | `30s`                     | Minimum interval between forced cache refreshes. Prevents API abuse from repeated force-refresh requests. Supports duration format (`30s`, `1m`) or seconds (`60`). Set to `0` to disable rate limiting.                                                                                  |
| `SLACK_MCP_LOG_LEVEL`             | No        | `info`                    | Log-level for stdout or stderr. Valid values are: `debug`, `info`, `warn`, `error`, `panic` and `fatal`                                                                                                                                                                                   |
| `SLACK_MCP_ENABLED_TOOLS`         | No        | `nil`                     | Comma-separated list of tools to register. If empty, all read-only tools and usergroups tools are registered; write tools (`conversations_add_message`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_mark`, `conversations_draft_message`) require their specific env var to be set OR must be explicitly listed here. When a write tool is listed here, it's enabled without channel restrictions. Available tools: `conversations_history`, `conversations_replies`, `conversations_add_message`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_search_messages`, `conversations_unreads`, `conversations_mark`, `conversations_draft_message`, `conversations_join`, `conversations_leave`, `channels_list`, `channels_me`, `usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`, `users_search`, `saved_list`, `saved_update`, `saved_clear_completed`. |

### Tool Registration and Permissions

#### Overview

Tools are controlled at two levels:
- **Registration** (`SLACK_MCP_ENABLED_TOOLS`) — determines which tools are visible to MCP clients
- **Runtime permissions** (tool-specific env vars like `SLACK_MCP_ADD_MESSAGE_TOOL`) — channel restrictions for write tools

Write tools (`conversations_add_message`, `reactions_add`, `reactions_remove`, `attachment_get_data`, `conversations_mark`, `conversations_draft_message`) are **not registered by default** to prevent accidental exposure. To enable them, you must either:
1. Set their specific environment variable (e.g., `SLACK_MCP_ADD_MESSAGE_TOOL`), or
2. Explicitly list them in `SLACK_MCP_ENABLED_TOOLS`

`conversations_draft_message` additionally requires a session token (`xoxc`/`xoxd`) — it is not registered for bot tokens because it relies on Slack's edge API. Drafts it creates or updates appear in Slack's **Drafts** list; open them from there. Due to a Slack desktop-client limitation, an API-created draft is not auto-attached to a channel's message composer (the composer only auto-shows drafts that client composed locally), even though the draft is saved and kept up to date.

Usergroups tools (`usergroups_list`, `usergroups_me`, `usergroups_create`, `usergroups_update`, `usergroups_users_update`) are **registered by default**. They require appropriate OAuth scopes (`usergroups:read` for read operations, `usergroups:write` for write operations).

#### Examples

**Example 1: Read-only mode (default)**

By default, only read-only tools are available. No write tools are registered.

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-..."
  }
}
```

**Example 2: Enable messaging to specific channels**

Use `SLACK_MCP_ADD_MESSAGE_TOOL` to enable messaging with channel restrictions:

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-...",
    "SLACK_MCP_ADD_MESSAGE_TOOL": "C123456789,C987654321"
  }
}
```

**Example 3: Enable messaging without channel restrictions**

Use `SLACK_MCP_ENABLED_TOOLS` to register write tools without restrictions:

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-...",
    "SLACK_MCP_ENABLED_TOOLS": "conversations_history,conversations_add_message,reactions_add"
  }
}
```

**Example 4: Minimal read-only setup**

Expose only specific tools:

```json
{
  "env": {
    "SLACK_MCP_XOXP_TOKEN": "xoxp-...",
    "SLACK_MCP_ENABLED_TOOLS": "channels_list,conversations_history"
  }
}
```

#### Behavior Matrix

| `ENABLED_TOOLS` | Tool-specific env var | Write tool registered? | Channel restrictions |
|-----------------|----------------------|------------------------|---------------------|
| empty/not set   | not set              | No                     | N/A                 |
| empty/not set   | `true`               | Yes                    | None                |
| empty/not set   | `C123,C456`          | Yes                    | Only listed channels |
| includes tool   | not set              | Yes                    | None                |
| includes tool   | `C123,C456`          | Yes                    | Only listed channels |
| excludes tool   | any                  | No                     | N/A                 |
