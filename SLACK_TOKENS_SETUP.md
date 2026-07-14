# Creating `~/.ssh/slack_tokens`

The background service (macOS LaunchAgent or Linux systemd user unit) and `run-with-tokens.sh` read Slack MCP environment variables from **`~/.ssh/slack_tokens`**. This keeps tokens out of the service definition and in a single file you can secure with `chmod 600`.

## 1. Create the file

```bash
touch ~/.ssh/slack_tokens
chmod 600 ~/.ssh/slack_tokens
```

## 2. Choose one auth method and add variables

Use **one** of the following. Format: one `KEY=value` per line. No spaces around `=`. Values with special characters can be single-quoted.

### Option A: Browser tokens (xoxc / xoxd) — “spy” / stealth mode

You need both tokens from a logged-in Slack session in your browser.

- **SLACK_MCP_XOXC_TOKEN**: From DevTools → Console, run:
  ```js
  JSON.parse(localStorage.localConfig_v2).teams[document.location.pathname.match(/^\/client\/([A-Z0-9]+)/)[1]].token
  ```
  Copy the value (starts with `xoxc-`).

- **SLACK_MCP_XOXD_TOKEN**: From DevTools → Application → Cookies → select the cookie named `d`, copy its value.

Example `~/.ssh/slack_tokens`:

```
SLACK_MCP_XOXC_TOKEN=xoxc-1234567890-1234567890123-abcdef...
SLACK_MCP_XOXD_TOKEN=xoxd-...
```

### Option B: User OAuth token (xoxp)

From [api.slack.com/apps](https://api.slack.com/apps) → your app → OAuth & Permissions → copy “User OAuth Token”.

```
SLACK_MCP_XOXP_TOKEN=xoxp-...
```

### Option C: Bot token (xoxb)

From the same page, copy “Bot User OAuth Token”.

```
SLACK_MCP_XOXB_TOKEN=xoxb-...
```

## 3. Optional variables

You can add any supported env vars to the same file, for example:

```
# Optional: allow posting messages (true or comma-separated channel IDs)
# SLACK_MCP_ADD_MESSAGE_TOOL=true

# Optional: SSE API key if you use SLACK_MCP_API_KEY for client auth
# SLACK_MCP_API_KEY=your-secret-key

# Optional: port (default 13080)
# SLACK_MCP_PORT=13080
```

Comments (lines starting with `#`) and empty lines are ignored.

## 4. Install and start the background service

> **Tip:** once `~/.ssh/slack_tokens` exists, the installer can set up (or re-run) the background service for you — launchd on macOS, systemd user unit on Linux — with no repo clone or plist editing:
>
> ```bash
> curl -fsSL https://raw.githubusercontent.com/provectus/slack-mcp-server/master/scripts/install.sh | bash -s -- --with-service
> ```
>
> The manual LaunchAgent steps below are for the source-build flow on macOS.

```bash
# Copy plist into LaunchAgents (from your repo clone)
cp /path/to/slack-mcp-server/com.slack-mcp-server.plist ~/Library/LaunchAgents/

# Load and start
launchctl load ~/Library/LaunchAgents/com.slack-mcp-server.plist
```

**Important:** Before loading, edit the plist and replace `/path/to/slack-mcp-server` and `/Users/YOUR_USERNAME` with your actual repo path and home directory.

The server will listen on **http://127.0.0.1:13080/sse** (SSE) and start automatically at login. Logs: `~/Library/Logs/slack-mcp-server.log` and `slack-mcp-server.err.log`.

To stop:

```bash
launchctl unload ~/Library/LaunchAgents/com.slack-mcp-server.plist
```
