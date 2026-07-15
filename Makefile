# If you update this file, please follow
# https://suva.sh/posts/well-documented-makefiles

.DEFAULT_GOAL := help

TAG ?=
GO=go
PACKAGE = $(shell go list -m)
GIT_COMMIT_HASH = $(shell git rev-parse HEAD)
GIT_VERSION = $(shell git describe --tags --always --dirty)
BUILD_TIME = $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
BINARY_NAME = slack-mcp-server
LD_FLAGS = -s -w \
	-X '$(PACKAGE)/pkg/version.CommitHash=$(GIT_COMMIT_HASH)' \
	-X '$(PACKAGE)/pkg/version.Version=$(GIT_VERSION)' \
	-X '$(PACKAGE)/pkg/version.BuildTime=$(BUILD_TIME)' \
	-X '$(PACKAGE)/pkg/version.BinaryName=$(BINARY_NAME)'
COMMON_BUILD_ARGS = -ldflags "$(LD_FLAGS)"

OSES = darwin linux windows
ARCHS = amd64 arm64

CLEAN_TARGETS :=
CLEAN_TARGETS += '$(BINARY_NAME)'
CLEAN_TARGETS += $(foreach os,$(OSES),$(foreach arch,$(ARCHS),./build/$(BINARY_NAME)-$(os)-$(arch)$(if $(findstring windows,$(os)),.exe,)))

# The help will print out all targets with their descriptions organized bellow their categories. The categories are represented by `##@` and the target descriptions by `##`.
# The awk commands is responsible to read the entire set of makefiles included in this invocation, looking for lines of the file as xyz: ## something, and then pretty-format the target and help. Then, if there's a line with ##@ something, that gets pretty-printed as a category.
# More info over the usage of ANSI control characters for terminal formatting: https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info over awk command: http://linuxcommand.org/lc3_adv_awk.php
#
# Notice that we have a little modification on the awk command to support slash in the recipe name:
# origin: /^[a-zA-Z_0-9-]+:.*?##/
# modified /^[a-zA-Z_0-9\/\.-]+:.*?##/
.PHONY: help
help: ## Display this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9\/\.-]+:.*?##/ { printf "  \033[36m%-21s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: clean
clean: ## Clean up all build artifacts
	rm -rf $(CLEAN_TARGETS)

.PHONY: build
build: clean tidy format ## Build the project
	go build $(COMMON_BUILD_ARGS) -o ./build/$(BINARY_NAME) ./cmd/slack-mcp-server

.PHONY: build-all-platforms
build-all-platforms: clean ## Build the project for all platforms (release path: must not mutate the tree, or the stamped version gains -dirty)
	$(foreach os,$(OSES),$(foreach arch,$(ARCHS), \
		GOOS=$(os) GOARCH=$(arch) go build $(COMMON_BUILD_ARGS) -o ./build/$(BINARY_NAME)-$(os)-$(arch)$(if $(findstring windows,$(os)),.exe,) ./cmd/slack-mcp-server; \
	))

.PHONY: deps
deps: ## Download dependencies
	$(GO) mod download

.PHONY: test
test: ## Run the tests
	$(GO) test -count=1 -v -run=".*Unit.*" ./...

.PHONY: format
format: ## Format the code
	$(GO) fmt ./...

.PHONY: tidy
tidy: ## Tidy up the go modules
	$(GO) mod tidy

.PHONY: release
release: ## Create release tag. Usage: make release TAG=pv-vX.Y.Z
	@if ! echo "$(TAG)" | grep -Eq '^pv-v[0-9]+\.[0-9]+\.[0-9]+$$'; then \
	  echo "Usage: make release TAG=pv-vX.Y.Z (e.g. TAG=pv-v1.0.0)"; exit 1; \
	fi
	git tag -a "$(TAG)" -m "Release $(TAG)"
	git push fork "$(TAG)"

LAUNCHD_LABEL := com.slack-mcp-server
PIN_LINK = $(HOME)/.local/share/$(BINARY_NAME)/current
RELEASE_BIN = $(HOME)/.local/bin/$(BINARY_NAME)

# Internal: restart the background service (launchd on macOS, systemd user
# unit on Linux). Fails with setup instructions when no service is installed.
.PHONY: service-restart
service-restart:
	@if [ "$$(uname)" = "Darwin" ]; then \
	  if ! launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) >/dev/null 2>&1; then \
	    echo "LaunchAgent $(LAUNCHD_LABEL) is not installed. One-time setup: scripts/install.sh --with-service (see README)."; exit 1; \
	  fi; \
	  launchctl kickstart -k gui/$$(id -u)/$(LAUNCHD_LABEL); \
	else \
	  systemctl --user restart $(BINARY_NAME) || { \
	    echo "systemd user service $(BINARY_NAME) is not installed. One-time setup: scripts/install.sh --with-service (see README)."; exit 1; }; \
	fi
	@echo "Service restarted. Reconnect your MCP client to pick up tool changes."

.PHONY: service-local
service-local: build ## Pin the service to the repo build and restart it
	@mkdir -p "$(HOME)/.local/share/$(BINARY_NAME)"
	ln -sfn "$(CURDIR)/build/$(BINARY_NAME)" "$(PIN_LINK)"
	@$(MAKE) --no-print-directory service-restart service-status

.PHONY: service-release
service-release: ## Pin the service to the release binary (installs it if missing) and restart it
	@if [ ! -x "$(RELEASE_BIN)" ]; then \
	  echo "Release binary not found; running the installer..."; \
	  bash scripts/install.sh --with-updater; \
	fi
	@mkdir -p "$(HOME)/.local/share/$(BINARY_NAME)"
	ln -sfn "$(RELEASE_BIN)" "$(PIN_LINK)"
	@$(MAKE) --no-print-directory service-restart service-status

.PHONY: service-status
service-status: ## Show the pinned binary, service state and version
	@printf 'Pin:     '; \
	if [ -L "$(PIN_LINK)" ]; then readlink "$(PIN_LINK)"; \
	else echo "none (auto-detect: ~/.local/bin, then repo build/)"; fi
	@if [ "$$(uname)" = "Darwin" ]; then \
	  if launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) >/dev/null 2>&1; \
	  then echo "Service: loaded (launchd)"; else echo "Service: not installed (launchd)"; fi; \
	else \
	  if systemctl --user is-active $(BINARY_NAME) >/dev/null 2>&1; \
	  then echo "Service: running (systemd user)"; else echo "Service: not running (systemd user)"; fi; \
	fi
	@printf 'Binary:  '; \
	if [ -x "$(PIN_LINK)" ]; then "$(PIN_LINK)" --version | head -n 1; \
	elif [ -x "$(RELEASE_BIN)" ]; then "$(RELEASE_BIN)" --version | head -n 1; \
	elif [ -x "$(CURDIR)/build/$(BINARY_NAME)" ]; then "$(CURDIR)/build/$(BINARY_NAME)" --version | head -n 1; \
	else echo "no binary found"; fi

.PHONY: reinstall-service
reinstall-service: service-local ## Deprecated alias for service-local
