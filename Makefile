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

.PHONY: reinstall-service
reinstall-service: build ## Rebuild the binary and restart the local macOS LaunchAgent
	@if [ "$$(uname)" != "Darwin" ]; then \
	  echo "reinstall-service is macOS-only (uses launchctl / LaunchAgent)."; exit 1; \
	fi
	@if ! launchctl print gui/$$(id -u)/$(LAUNCHD_LABEL) >/dev/null 2>&1; then \
	  echo "LaunchAgent $(LAUNCHD_LABEL) is not installed. See the 'Running as a background service (launchd on macOS, systemd on Linux)' section in README.md for one-time setup, then re-run."; exit 1; \
	fi
	launchctl kickstart -k gui/$$(id -u)/$(LAUNCHD_LABEL)
	@echo "Restarted $(LAUNCHD_LABEL). Reconnect your MCP client to pick up tool changes."
	@if [ -x "$$HOME/.local/bin/slack-mcp-server" ]; then \
	  echo "Note: run-with-tokens.sh prefers ~/.local/bin/slack-mcp-server (curl install) over build/, so the service may NOT be running your fresh build."; \
	  echo "To pin the repo build, set SLACK_MCP_BIN=$(CURDIR)/build/$(BINARY_NAME) in the plist env — or remove the ~/.local/bin copy."; \
	fi
