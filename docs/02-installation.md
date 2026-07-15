### 2. Installation

#### Binary install (recommended)

One command on macOS or Linux, no Go toolchain needed:

```bash
curl -fsSL https://raw.githubusercontent.com/provectus/slack-mcp-server/master/scripts/install.sh | bash
```

This installs the latest fork release binary to `~/.local/bin/slack-mcp-server` after sha256 verification, and (by default) the `slack-mcp-update` updater next to it. See the [Install section in the README](../README.md#install) for the flags (`--version`, `--prefix`, `--with-updater` / `--no-updater`, `--with-service`) and the update workflow. Windows binaries are attached to each [release](https://github.com/provectus/slack-mcp-server/releases) for manual download.

#### Build from source (developers)

```bash
git clone https://github.com/provectus/slack-mcp-server.git
cd slack-mcp-server
make build   # binary lands at build/slack-mcp-server
```

See next: [Configuration and Usage](03-configuration-and-usage.md)
