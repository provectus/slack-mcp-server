# Functional Specification: Service Binary Flip (Development ↔ Release)

- **Roadmap Item:** None (developer-experience improvement; retrofit of shipped work, PR #18)
- **Status:** Approved
- **Author:** Aleksandr Makarov

---

## 1. Overview and Rationale (The "Why")

The Slack MCP server runs as an always-on background service on the user's machine. Two kinds of people run it: users who install the official released version with one command, and contributors who build the server from source to test their changes. Before this change these two setups conflicted: the background service was tied to one binary choice at setup time, and pointing it at the other version required hand-editing service configuration — error-prone, invisible, and different for every process that starts the server.

This change lets a contributor flip the running service between the **official released version** and their **own freshly built development version** with a single command, in either direction, at any time. Both modes reuse the same saved Slack credentials and service setup — nothing needs to be re-entered or reconfigured. Success measure: a contributor goes from "code change saved" to "background service running that change" with one command, and back to the official version with one command.

---

## 2. Functional Requirements (The "What")

- **As a** contributor, **I want** one command that rebuilds the server from my working copy and restarts the background service on that build, **so that** I can exercise my changes live immediately.
  - **Acceptance Criteria:**
    - [ ] Given the background service is set up, when I run `make service-local`, then the server is rebuilt from my working copy, the service restarts on that build, and a status readout confirms the development build is now active.

- **As a** contributor, **I want** one command that returns the service to the official released version, **so that** I can leave my machine in a known-good state.
  - **Acceptance Criteria:**
    - [ ] Given the official released version is present on the machine, when I run `make service-release`, then the service restarts on the official version and the status readout confirms it.
    - [ ] Given the official released version is **not** present, when I run `make service-release`, then it is downloaded and installed first, and the service then restarts on it.

- **As a** user, **I want** to see at a glance which version the service is running, **so that** I never have to guess.
  - **Acceptance Criteria:**
    - [ ] When I run `make service-status`, then I see which version is active (development build or official release), whether the service is running, and the exact version identifier.

- **As a** user, **I want** both modes to use my existing saved Slack credentials and service setup, **so that** flipping never requires reconfiguration.
  - **Acceptance Criteria:**
    - [ ] Given the assistant could reach Slack before a flip, when I flip in either direction, then it can still reach Slack afterwards with no credential or configuration changes.

- **As a** new user, **I want** the one-command installer (with the service option) to leave the service running the version it just installed, **so that** setup ends in a working state.
  - **Acceptance Criteria:**
    - [ ] When the installer finishes with the service option, then the service is running the just-installed official version, and the status readout confirms it.
    - [ ] Given the service is in official-release mode, when the built-in updater applies a newer official release, then the service keeps running the updated official version without any re-setup.

- **The choice of version must be explicit and fail loudly, never silent.**
  - **Acceptance Criteria:**
    - [ ] Given the selected version was deleted or is broken, when the service starts, then it stops with a clear error naming the problem and how to fix it — it does not silently run a different version.

- **Existing habits keep working.**
  - **Acceptance Criteria:**
    - [ ] When I run the older `make reinstall-service` command, then it behaves exactly like `make service-local`.

---

## 3. Scope and Boundaries

### In-Scope

- Flipping the background service between development build and official release (macOS and Linux).
- Version/status visibility for the service.
- Installer leaving the service pinned to the version it installed.

### Out-of-Scope

- Windows background-service support (no service exists there today).
- Running two versions side by side, or automatic switching.
- All other roadmap items (Group-DM support, Guided Token Extraction & Setup, Setup Diagnostics).

---

## Change Log

_(empty — no amendments yet)_
