# Common developer and CI tasks. Run `make` (or `make help`) to see them.
# CI runs `make check`, so local and CI verify the exact same things.

GO  ?= go
PKG := ./...

# golangci-lint bundles staticcheck. Pinned so local and CI lint identically,
# and installed via the official script — upstream does not support
# `go install`.
GOLANGCI_VERSION := v2.12.2
GOLANGCI         := bin/golangci-lint

.DEFAULT_GOAL := help
.PHONY: help build test vet lint fmt fmt-check tidy check clean

help: ## List available targets
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "} {printf "  %-12s %s\n", $$1, $$2}'

build: ## Compile all commands
	$(GO) build $(PKG)

test: ## Run all tests (race detector on)
	$(GO) test -race $(PKG)

vet: ## Run go vet
	$(GO) vet $(PKG)

# The pinned binary is a file target, so it installs once and is reused; a
# fresh CI checkout installs the pinned version. To upgrade, bump the version
# and delete bin/golangci-lint. The install script is fetched from the same
# tag as the version it installs, not from a moving HEAD, so the script itself
# is pinned too. Download and run are separate lines, not `curl | sh`: sh over
# the empty input a failed curl leaves succeeds, and the failure would
# resurface later as a baffling "bin/golangci-lint: not found".
$(GOLANGCI):
	@mkdir -p bin
	curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/$(GOLANGCI_VERSION)/install.sh -o bin/golangci-install.sh
	sh bin/golangci-install.sh -b bin $(GOLANGCI_VERSION)
	@rm -f bin/golangci-install.sh

lint: $(GOLANGCI) ## Lint with golangci-lint (staticcheck included)
	$(GOLANGCI) run

fmt: ## Format all Go files in place
	gofmt -w .

fmt-check: ## Fail if any Go file is not gofmt-clean (CI guard)
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "Not gofmt-clean:"; echo "$$unformatted"; exit 1; \
	fi

tidy: ## Tidy module dependencies
	$(GO) mod tidy

check: fmt-check vet lint test build ## Run the full CI suite (format, vet, lint, test, build)

clean: ## Remove build artifacts
	rm -rf bin
