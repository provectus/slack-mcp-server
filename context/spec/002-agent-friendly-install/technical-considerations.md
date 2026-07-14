# Technical Specification: Agent-Friendly Install & Update

- **Functional Specification:** [functional-spec.md](./functional-spec.md)
- **Status:** Approved
- **Author(s):** Aleksandr Makarov (with release-infra specialist)

---

## 1. High-Level Technical Approach

Publish fork-native GitHub Releases on `provectus/slack-mcp-server` (prebuilt binaries + checksums, no npm/DXT/Docker), triggered by fork-prefixed tags `pv-v*`. Add a `--version` flag to the binary so installed versions are introspectable. Ship two shell scripts: `scripts/install.sh` (curl-able one-command install, macOS/Linux, optional always-on service setup — LaunchAgent on macOS, systemd user unit on Linux) and `scripts/update.sh` (installed alongside the binary as `slack-mcp-update`; check-only and check+apply modes; config-change warnings scanned from release notes; safe atomic binary swap; service restart). The existing source-build + LaunchAgent flow keeps working; `run-with-tokens.sh` gains a binary-resolution order so both flows share the service machinery.

Affected systems: GitHub Actions release pipeline, Makefile, `cmd/slack-mcp-server` + `pkg/version`, `run-with-tokens.sh`, docs.

---

## 2. Proposed Solution & Implementation Plan (The "How")

### 2.1 Release pipeline (R1)

- **Tag scheme (confirmed):** fork releases tagged `pv-vX.Y.Z`, starting `pv-v1.0.0`. Upstream's `v*` tags in history/future syncs can never trigger the fork workflow. Version comparison strips the `pv-v` prefix.
- **`.github/workflows/release.yaml`:**
  - Trigger: `on.push.tags: ['pv-v*']`.
  - Runner: `ubuntu-latest` (pure-Go cross-compile — the existing 6-target matrix already builds without CGO).
  - Steps: `make build-all-platforms` → generate `build/checksums.txt` via `shasum -a 256 slack-mcp-server-*` → upload release assets: 6 binaries, `checksums.txt`, `LICENSE` (`generate_release_notes: true`, `make_latest: true`).
  - Removed: `make build-dxt`, `make npm-publish`, `.env.dist`, `docker-compose.yml` assets.
- **`.github/workflows/release-image.yaml`:** deleted (pushes to `ghcr.io/korotovsky/...`; container publishing out of scope).
- **Makefile:** `release` target pushes the tag to the `fork` remote (currently pushes to `origin` = upstream — bug) and validates the `pv-v` format. `npm-publish`, `npm-copy-binaries`, and `build-dxt` targets are **deleted as dead code**, along with the `npm/` directory and `manifest-dxt.json` (confirmed — upstream-merge compatibility is not a concern).
- **Config-change marking (confirmed):** a release is marked configuration-changing by a line in its GitHub release notes body beginning `CONFIG-CHANGE: <review note>` (multiple lines allowed). Editable post-publish in the GitHub UI; greppable via the releases API.

### 2.2 Version introspection (R4 prerequisite)

- `pkg/version`: add pure function `String() string` returning:

  ```
  slack-mcp-server pv-v1.0.0
  commit: <hash>
  built:  <time>
  ```

- `cmd/slack-mcp-server/main.go`: add `--version` bool flag (stdlib `flag`, long form only); right after `flag.Parse()`, print `version.String()` to stdout and exit 0 — before any logger/provider/token validation, so it works on a machine with no tokens configured. Scripts parse line 1, field 2.

### 2.3 Install script (R2, R3)

- **File:** `scripts/install.sh`. Documented command: `curl -fsSL https://raw.githubusercontent.com/provectus/slack-mcp-server/master/scripts/install.sh | bash`. *(Assumption: raw-from-master so the documented command never goes stale; `--version` flag pins a release.)*
- **Platform detection:** `uname -s`/`uname -m` → `darwin|linux` × `amd64|arm64`; unsupported → clear error, exit 2.
- **Destination:** `~/.local/bin/slack-mcp-server` (no sudo). *(Assumption.)*
- **Updater install is asked, not silent (confirmed):** installer prompts "also install the updater (`slack-mcp-update`)?" reading from `/dev/tty` when interactive. Flags `--with-updater` / `--no-updater` skip the prompt (for agents/automation). Non-interactive run without either flag defaults to **installing** the updater, with a notice line. *(Assumption: non-TTY default = yes.)*
- **PATH:** never edits shell rc files; if the bin dir is not on `$PATH`, prints the exact `export PATH=…` line for the user to add. *(Assumption.)*
- **Flags:** `--version <pv-vX.Y.Z>` (pin, default latest), `--prefix <dir>`, `--with-updater` / `--no-updater`, `--with-service` (macOS and Linux).
- **Flow:** resolve release via GitHub API → download binary + `checksums.txt` to temp → sha256 verify → `chmod +x` → move into place → probe `slack-mcp-server --version` → report installed version.
- **Service mode without a git checkout (confirmed: macOS + Linux):** `--with-service` downloads `run-with-tokens.sh` (same raw URL base) to `~/.local/share/slack-mcp-server/`, then:
  - **macOS:** **renders** `~/Library/LaunchAgents/com.slack-mcp-server.plist` with real paths (never copies the placeholder template), `launchctl bootstrap` + `kickstart -k`.
  - **Linux:** renders a systemd **user unit** `~/.config/systemd/user/slack-mcp-server.service` (ExecStart = `run-with-tokens.sh`, `Restart=on-failure`), `systemctl --user daemon-reload && systemctl --user enable --now slack-mcp-server`. Prints a note that `loginctl enable-linger $USER` is required for start-at-boot without an active login session.
  - Missing `~/.ssh/slack_tokens` (both OSes) → binary and scripts still installed, warning points to `SLACK_TOKENS_SETUP.md`, exit 0. `run-with-tokens.sh` is plain bash and already OS-agnostic.
- **Exit codes:** 0 ok (incl. token-file warning), 2 unsupported platform/bad flags, 3 download/API failure, 4 checksum mismatch, 5 post-install probe failure.

### 2.4 Update script (R4, R5)

- **File:** `scripts/update.sh`, installed as `slack-mcp-update`. Binary discovery: sibling `slack-mcp-server` next to the script's real path; `--bin <path>` override. Installed version read from `--version` output.
- **Modes:** default check+apply; `--check` reports only, changes nothing.
- **GitHub API usage (one check per invocation):** `GET /repos/provectus/slack-mcp-server/releases/latest` resolves the newest version to compare against the installed one; when an update range exists, one more call (`releases?per_page=100`) fetches the notes of the in-between releases for the `CONFIG-CHANGE:` scan (filter tags in `(installed, latest]` after prefix strip). The repo is **public** (verified via `gh repo view`: visibility PUBLIC), so unauthenticated access works; `$GITHUB_TOKEN` is honored when set (higher rate limit). API and download URLs are **hardcoded** in the scripts — no base-URL override knobs (confirmed).
- **Output contract:** human summary plus machine-readable lines:

  ```
  INSTALLED=pv-v1.0.0
  LATEST=pv-v1.2.0
  RESULT=up-to-date|updated|update-available|error
  CONFIG_CHANGES=<n>
  ```

  Each config-change note printed as a prominent `WARNING (pv-vX.Y.Z): <note>` block in both modes.
- **Exit codes:** 0 up-to-date/updated, 10 update-available (`--check` only), 1 error. Config warnings do not alter exit codes — `CONFIG_CHANGES=` is the signal. *(Assumption.)*
- **Swap safety:** download to `<bindir>/.slack-mcp-server.new` (same filesystem → atomic rename) → sha256 verify against release `checksums.txt` → probe `.new --version` → rename current to `.bak`, `.new` into place, re-probe, delete `.bak`. Any failure → restore `.bak`, `RESULT=error`, exit 1.
- **Service restart:** macOS — if `launchctl print gui/$(id -u)/com.slack-mcp-server` succeeds, `kickstart -k` after the swap. Linux — if `systemctl --user is-enabled slack-mcp-server` succeeds, `systemctl --user restart slack-mcp-server`.

### 2.5 Shared service machinery

- `run-with-tokens.sh` binary resolution order: `$SLACK_MCP_BIN` env → `~/.local/bin/slack-mcp-server` → `<repo>/build/slack-mcp-server` (script-relative, current behavior). One file serves both the curl-install and source-build flows.
- `com.slack-mcp-server.plist` stays a placeholder template for the source flow; the installer renders its own.
- **Precedence caveat:** a source-flow user who also curl-installed gets the `~/.local/bin` binary. `make reinstall-service` documentation notes this; a source user can pin via `SLACK_MCP_BIN` in the plist env.

### 2.6 Docs

- `README.md` — new primary "Install" section (curl command, update command, service option); rewrite "Running as a macOS background service" into a section covering both install flows and both OS service managers (launchd / systemd user unit).
- `docs/02-installation.md` — binary install first, source build demoted.
- `SLACK_TOKENS_SETUP.md` — note service setup re-runnable via `install.sh --with-service`.
- `context/product/architecture.md` — distribution/CI section updated.

---

## 3. Impact and Risk Analysis

- **System Dependencies:** GitHub Releases API availability (update/install fail closed with exit 3/1 and clear messages); `softprops/action-gh-release`; existing Makefile cross-compile matrix; launchd (macOS service mode) / systemd user session (Linux service mode).
- **Risks & Mitigations:**
  - *Upstream tag collision / accidental workflow firing* → `pv-v*` trigger guard; upstream `v*` tags inert.
  - *Corrupt/partial download bricks the install* → checksum verify + temp-file atomic rename + `--version` probe + `.bak` rollback.
  - *Unauthenticated API rate limits (CI, shared IPs)* → only 2 calls per run; `$GITHUB_TOKEN` honored.
  - *`curl | bash` trust* → HTTPS raw URL from the repo itself; checksums for binaries; script is reviewable in-repo.
  - *LaunchAgent drift between the two flows* → single `run-with-tokens.sh` with explicit resolution order; installer-rendered plist never shares the template file.
  - *Update while server running* → atomic rename is safe (running process keeps its inode); `kickstart -k` restarts onto the new binary.

---

## 4. Testing Strategy

- **Go unit:** `pkg/version.String()` format; smoke of the built binary's `--version` (exit 0, line-1 shape) via Makefile/test helper.
- **Shell:** plain-sh test harness under `scripts/test/` *(assumption: no bats dependency)*. URLs stay hardcoded in the scripts (confirmed); tests mock the network via a **curl PATH shim** (a stub `curl` earlier in `$PATH` serving fixture responses: releases JSON with/without `CONFIG-CHANGE:` lines, binary assets, checksums). Platform-detect and semver-compare are sourceable functions tested directly.
- **Mid-update failure & rollback (confirmed, required):** tests that inject an error at each stage of the swap (download truncation, checksum mismatch, failed `--version` probe of the new binary) and assert the old binary is restored intact, `RESULT=error` is emitted, and the error is visible in the updater output.
- **End-to-end with mocked GitHub API (confirmed, required):** a full updater run against the curl shim playing a complete release sequence — installed old version, newer release published, config-changing release in between — asserting: check-only output, apply path, binary actually replaced (probe reports new version), `CONFIG_CHANGES` warning shown, exit codes correct.
- **CI:** `shellcheck scripts/*.sh` + the sh harness on ubuntu and macos runners (PR job). Optional post-release smoke on `pv-v*`: curl the raw installer, pin to the just-published tag, assert `--version` on both runners.
- **Manual:** LaunchAgent / systemd-user-unit bootstrap end-to-end (CI runners lack a GUI launchd / user systemd session); real update crossing a config-changing release.
