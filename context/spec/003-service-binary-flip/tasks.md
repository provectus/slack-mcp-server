<!-- retrofit: implementation shipped on feat/service-binary-flip (517ab30, PR #18) before this task list was written; items are checked to reflect the work as actually done -->

# Tasks: Service Binary Flip (Development ↔ Release)

- [x] **Slice 1: Pin-aware binary resolution**

  > The launcher honors the `current` pin symlink; no pin = old behavior.
  - [x] Task: Add the pin lookup to `resolve_binary` in `run-with-tokens.sh` — after `$SLACK_MCP_BIN`, before `~/.local/bin`; hard error with remediation text when the pin exists but is not executable. **[Agent: release-infra]**
  - [x] Verify: shellcheck with CI flags (`-e SC2046,SC2163`); manual resolution check (pin present → pinned binary; pin absent → prior order). **[Agent: release-infra]**

- [x] **Slice 2: Installer creates the pin, service files stop pinning via env**

  > `install.sh --with-service` becomes pin-based; custom `--prefix` service installs start working.
  - [x] Task: Create `ln -sfn <installed binary> <share-dir>/current` in `setup_service`; drop `SLACK_MCP_BIN` from the rendered plist and systemd unit; simplify `setup_service_darwin`/`setup_service_linux` signatures. **[Agent: release-infra]**
  - [x] Task: Update `scripts/test/test_install_service.sh` — `assert_pin` helper, pin assertions in the macOS/Linux/whitespace-prefix render cases, `SLACK_MCP_BIN` absence assertions. **[Agent: testing-expert]**
  - [x] Verify: `bash scripts/test/run.sh` green (41/41); rendered unit still passes `systemd-analyze --user verify` where available. **[Agent: release-infra]**

- [x] **Slice 3: Makefile flip targets**

  > One-command flip in each direction plus status; old habit preserved.
  - [x] Task: Add `service-local`, `service-release`, `service-status`, internal `service-restart` (Darwin/Linux), and `reinstall-service` as deprecated alias. **[Agent: release-infra]**
  - [x] Task: Document the pin and the flip targets in README's background-service section; update `context/product/architecture.md`'s resolution-order line. **[Agent: release-infra]**
  - [x] Verify: `make service-status` runs on a machine without a pin (auto-detect message) and with one (target + version shown); artifacts: none produced. **[Agent: release-infra]**

- [x] **Slice 4: Feature Testing & Regression**

  > Verifies the whole feature end-to-end against functional-spec.md, run after all implementation slices are complete.
  - [x] Acceptance coverage note (retrofit): install-side criteria are covered by the committed shell harness (`scripts/test/test_install_service.sh` pin/render cases — the harness has no annotation mechanism, so `@spec`/`@regression` tags are not applicable); launcher resolution and the flip flow were verified live on macOS: installer → pin → release `pv-v1.0.0`, SSE `HTTP 200`, `slack-mcp-update --check` up-to-date, `make service-local` → pin → repo build, SSE `HTTP 200`. **[Agent: testing-expert]**
  - [x] Run all tests: `bash scripts/test/run.sh` — 41/41 pass. **[Agent: testing-expert]**

## Known gaps

- `run-with-tokens.sh` has no automated test harness (pre-existing project gap); its resolution logic is verified manually. Candidate for a future harness addition.
