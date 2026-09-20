# Every target here is read-only against the two live instances. The binary
# these targets build is not: since phase 5 it has an `apply` command. Nothing
# in this file runs it.

GO      ?= go
BIN     ?= bin/infisical-mirror
PKGS    ?= ./...

# A unit test that takes longer than this is hung, not slow. Without the flag
# the package default is 10 minutes, which in CI means a deadlocked lock test
# burns the whole job before anyone sees why.
TEST_TIMEOUT ?= 60s

# Build metadata. A binary that cannot say which commit it came from is one
# nobody can trace, which matters more once the thing being traced is what
# wrote to production secrets. goreleaser sets the same three variables through
# its own ldflags, so a release binary and a local one report the same shape.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X github.com/alekc/infisical-mirror/internal/cli.Version=$(VERSION) \
	-X github.com/alekc/infisical-mirror/internal/cli.Commit=$(COMMIT) \
	-X github.com/alekc/infisical-mirror/internal/cli.BuildDate=$(DATE)

.PHONY: all
all: fmt vet lint test

.PHONY: build
build:
	$(GO) build -ldflags '$(LDFLAGS)' -o $(BIN) ./cmd/infisical-mirror

# Proves the ldflags above still point at variables that exist. A renamed
# package or variable does not fail the link, it silently leaves the binary
# reporting "dev", and a version string nobody checks is one nobody notices is
# wrong.
.PHONY: version-check
version-check: build
	@if [ "$(COMMIT)" = none ]; then \
		echo "no git commit available, so this check would pass against the built-in default"; exit 1; \
	fi; \
	out=$$($(BIN) version); \
	case "$$out" in \
		*"$(COMMIT)"*) echo "$$out" ;; \
		*) echo "version ldflags did not reach the binary: $$out"; exit 1 ;; \
	esac

.PHONY: fmt
fmt:
	$(GO) fmt $(PKGS)

# Separate from fmt so CI can fail on unformatted code rather than rewrite it.
.PHONY: fmt-check
fmt-check:
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

.PHONY: vet
vet:
	$(GO) vet $(PKGS)

.PHONY: lint
lint:
	golangci-lint run $(PKGS)

.PHONY: test
test:
	$(GO) test -timeout $(TEST_TIMEOUT) $(PKGS)

# The race detector is not optional here: one client is shared across the
# goroutines that read both instances, and its folder and project caches are
# the part most likely to be got wrong.
.PHONY: test-race
test-race:
	$(GO) test -race -timeout $(TEST_TIMEOUT) $(PKGS)

.PHONY: cover
cover:
	$(GO) test -timeout $(TEST_TIMEOUT) -coverprofile=coverage.out $(PKGS)
	$(GO) tool cover -func=coverage.out | tail -1

.PHONY: tidy
tidy:
	$(GO) mod tidy

.PHONY: clean
clean:
	rm -rf bin coverage.out
