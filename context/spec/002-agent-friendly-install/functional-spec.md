# Functional Specification: Agent-Friendly Install & Update

- **Roadmap Item:** Phase 3 — Frictionless Onboarding (install/update without building from source)
- **Status:** Completed
- **Author:** Aleksandr Makarov

---

## 1. Overview and Rationale (The "Why")

Today the only way to run this project's own edition of the server is to build it from source, which requires a developer toolchain (Go) that many users don't have. Updating means pulling the repository and rebuilding. There is also no working way to publish ready-to-run versions: the release process inherited from the original project points at the original project's distribution channels and cannot be used by this project.

This change gives users — and AI agents acting for them (e.g. Claude Code) — a fully scriptable way to install and update the server from ready-to-run programs, with no compiler and no manual visits to a downloads page. Success is measured by time-to-first-tool-call: a new user (or their AI agent) goes from nothing to a running server with a small number of copy-pasteable commands, and keeping the server current is one command.

---

## 2. Functional Requirements (The "What")

**R1 — Published ready-to-run versions.** The project publishes versioned releases containing ready-to-run programs for macOS, Linux, and Windows (both common processor types). A release can be marked as "changes configuration expectations."

- **Acceptance Criteria:**
  - [x] Given the maintainer publishes a new version, when a user views the project's releases, then ready-to-run programs for each supported platform are attached to it.
  - [x] The maintainer can mark a release as configuration-changing, with a note describing what the user must review.
  - [x] Publishing a release does not attempt to use the original project's distribution channels.

**R2 — One-command install.** A user or agent installs the server with a single copy-pasteable command that downloads the latest release for their platform and puts a working program in place. No compiler needed.

- **Acceptance Criteria:**
  - [x] Given a macOS or Linux machine with no developer tools, when the user runs the documented single install command, then the server program is installed and responds to a version query.
  - [x] When run interactively, the installer asks one question — whether to also install the update helper; either answer completes the install. Command options let the user (or an agent) pre-answer this question so no prompt appears.
  - [x] If the platform is unsupported or the download fails, the user sees a clear error saying what went wrong and what to do.

**R3 — Optional background-service setup (macOS and Linux).** The installer can, on request, also configure the always-on background service (using the existing token-file setup), replacing today's manual configuration-file editing.

- **Acceptance Criteria:**
  - [x] Given a macOS or Linux user with a prepared token file, when they run the install command with the service option, then the server runs as a background service that survives restarts.
  - [x] Given the token file is missing, when they request service setup, then they get a message pointing to the token setup guide, and the program itself is still installed.
  - [x] Without the service option, install changes nothing about services.

**R4 — Version check & update script.** A separate script — the running server is not involved and never self-updates — checks the latest published version and, by default, updates in place when newer. Calling it is the user's (or their agent's) responsibility.

- **Acceptance Criteria:**
  - [x] Given an installed server, when the user runs the update script in check-only mode, then it reports the installed version, the latest available version, and whether an update exists — changing nothing.
  - [x] Given a newer version exists, when the user runs the update script, then the program is replaced with the new version and the reported version afterwards matches the latest release.
  - [x] Given the server runs as the background service, when an update is applied, then the service is restarted onto the new version.
  - [x] Given the installed version is already the latest, running the update script changes nothing and says so.
  - [x] Output is agent-friendly: non-interactive, and the outcome (up-to-date / updated / update available / error) is unambiguous from the output and exit status.

**R5 — Configuration-change warning.** When updating across any release marked configuration-changing, the update script prominently warns the user and shows the release's note about what to review.

- **Acceptance Criteria:**
  - [x] Given the installed→latest range includes a configuration-changing release, when the user updates (or runs check-only), then a clearly visible warning appears with that release's review note.
  - [x] Given no such release in the range, no warning appears.

---

## 3. Scope and Boundaries

### In-Scope

- Publishing ready-to-run programs from this project's own releases (binaries for all supported platforms attached to each release).
- Install script: macOS and Linux; optional background-service setup on both.
- Separate check/update script: macOS and Linux.
- Documentation updates describing the new install/update path as the primary one.

### Out-of-Scope

- Windows install/update scripting (binaries are published; setup stays manual).
- Auto-update or scheduled update checks by the server itself.
- Publishing to the original project's distribution channels (npm package, desktop-extension registry, Docker Hub).
- Other Phase 3 roadmap items (Group-DM support, guided token extraction, setup diagnostics).

---

## Change Log

_Dated amendments made after the spec was first written. Leave empty until the first amendment._

- 2026-07-14 — tech-spec review feedback — Background-service setup extended from macOS-only to macOS **and Linux** (user decision during tech review). Installer now asks whether to also install the update helper, with options to pre-answer non-interactively (was: fully silent install).
