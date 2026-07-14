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

#### Upstream-only channels

The upstream project (`korotovsky/slack-mcp-server`) also distributes through channels this fork does not publish to — the methods below install the **upstream** edition, not this fork's releases:

- [DXT Extension](03-configuration-and-usage.md#Using-DXT)
- [Cursor Installer](03-configuration-and-usage.md#Using-Cursor-Installer)
- [npx](03-configuration-and-usage.md#Using-npx)
- [Docker](03-configuration-and-usage.md#Using-Docker)

See next: [Configuration and Usage](03-configuration-and-usage.md)
