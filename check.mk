# Shared, containerized Go checks for every Zinc module - the tools (zc, zcr) and
# the shared library (common). Included by tool.mk (which adds the binary targets)
# and by common's Makefile directly. Podman-only: there is no host Go; every command
# runs in the digest-pinned container. Run from a module's root: `make <target>`.

CONTAINER_TOOL ?= podman

# Path from the including module's dir to the repo root - i.e. the directory holding
# this check.mk. Derived from check.mk's own include path, so it is correct at any
# nesting depth: a module at container/<m> reaches the root via ../.., one at <m> via
# ".." - no module needs to hardcode how deep it sits.
REPO_REL := $(patsubst %/,%,$(dir $(lastword $(MAKEFILE_LIST))))

# Pinned Go toolchain image - KEEP IN SYNC with the repo-root Containerfile's GO_IMAGE.
GO_IMAGE       ?= docker.io/library/golang:1.24-alpine@sha256:757779acac4af1b349a20f357c7296097b4a0b89da4ad0e370b339060077282a

# Containerized go for checks/tests against THIS module: mount the module dir and
# use its vendored deps. Recursive (=) so $$PWD expands in the recipe shell.
#
# --network=none is what makes `make check` hermetic rather than merely containerized.
# `go test ./...` runs arbitrary test code, so with a network a test could quietly depend
# on fetching something and still pass - the same invariant the build step already enforces
# at tool.mk. Nothing in fmt, vet or test needs the network: the deps are vendored and
# GOTOOLCHAIN=local forbids downloading a toolchain.
#
# --cap-drop=ALL matches the baseline Zinc applies to every app container. A test compiler
# has no more business holding CAP_NET_RAW than an app does.
GO_RUN          = $(CONTAINER_TOOL) run --rm --security-opt label=disable \
                    --network=none --cap-drop=ALL --security-opt no-new-privileges \
                    -v "$$PWD":/src -w /src -e GOTOOLCHAIN=local $(GO_IMAGE)

# Vendoring is different: a local `replace => ../../common` means tidy/vendor need the
# whole repo in scope, and must ignore any go.work (GOWORK=off) so the module's own
# go.mod/replace drive the result. Mount the REPO ROOT (not just the module's parent,
# which is too shallow for a nested module's replace) and work in the module's subdir
# under it. This is the only step that needs network.
# Network stays ON here - this is the one step that legitimately fetches - but the
# capabilities do not, and this is the widest mount in the repo (the whole tree, writable).
GO_VENDOR       = ROOT="$$(cd "$(REPO_REL)" && pwd)"; \
                  $(CONTAINER_TOOL) run --rm --security-opt label=disable \
                    --cap-drop=ALL --security-opt no-new-privileges \
                    -v "$$ROOT":/repo -w "/repo/$${PWD\#$$ROOT/}" \
                    -e GOTOOLCHAIN=local -e GOWORK=off $(GO_IMAGE)

# gofmt recurses into directory args, so feed it the .go FILES with vendor/ pruned -
# vendored third-party code is not ours to reformat.
GOFILES         = find . -path ./vendor -prune -o -name "*.go" -print

.DEFAULT_GOAL := help

## help: list available targets
help:
	@if [ -t 1 ]; then color=1; else color=0; fi; \
	grep -hE '^## ' $(MAKEFILE_LIST) | sed 's/^## //' | \
	awk -v color=$$color '{ \
	  i = index($$0, ": "); name = substr($$0, 1, i-1); desc = substr($$0, i+2); \
	  if (color) printf "  \033[36m%-16s\033[0m %s\n", name, desc; \
	  else       printf "  %-16s %s\n", name, desc }'

## test: run unit tests (in the pinned container)
test:
	$(GO_RUN) go test ./...

## vet: run go vet (in the pinned container)
vet:
	$(GO_RUN) go vet ./...

## fmt: format our sources in place - vendor/ excluded (in the pinned container)
fmt:
	$(GO_RUN) sh -c 'gofmt -w $$($(GOFILES))'

## fmt-check: fail if any of our sources need formatting - vendor/ excluded (in the pinned container)
fmt-check:
	$(GO_RUN) sh -c 'unformatted="$$(gofmt -l $$($(GOFILES)))"; \
		if [ -n "$$unformatted" ]; then echo "needs gofmt:"; echo "$$unformatted"; exit 1; fi'

## check: fmt-check + vet + test (the pre-commit gate, ROADMAP "every change")
check: fmt-check vet test

# --- Dependency / vendor maintenance (run by hand; these need network, the build never does) ---

## vendor: refresh vendored deps to match go.mod - tidy, vendor, verify
vendor:
	$(GO_VENDOR) sh -c 'go mod tidy && go mod vendor && go mod verify'

## vendor-check: fail if 'tidy + vendor' would change anything (dirty-tree safe; no-op for dep-free modules)
vendor-check:
	$(GO_VENDOR) sh -c 'set -e; snapshot="$$(mktemp -d)"; \
		cp go.mod "$$snapshot/go.mod"; \
		[ -f go.sum ] && cp go.sum "$$snapshot/go.sum" || true; \
		[ -d vendor ] && cp -a vendor "$$snapshot/vendor" || true; \
		go mod tidy && go mod vendor; \
		status=0; \
		diff -q "$$snapshot/go.mod" go.mod >/dev/null 2>&1 || status=1; \
		if [ -f go.sum ] || [ -f "$$snapshot/go.sum" ]; then diff -q "$$snapshot/go.sum" go.sum >/dev/null 2>&1 || status=1; fi; \
		if [ -d vendor ] || [ -d "$$snapshot/vendor" ]; then diff -rq "$$snapshot/vendor" vendor >/dev/null 2>&1 || status=1; fi; \
		rm -rf "$$snapshot"; \
		if [ $$status -ne 0 ]; then echo "vendor out of sync - run make vendor and commit the result"; exit 1; fi; \
		echo "vendor-check: in sync"'

## verify: check module checksums against go.sum (offline; integrity only)
verify:
	$(GO_RUN) go mod verify

## update: upgrade dependencies (minor/patch, no major bumps), then re-vendor
update:
	$(GO_VENDOR) sh -c 'go get -u ./... && go mod tidy && go mod vendor && go mod verify'

.PHONY: help test vet fmt fmt-check check vendor vendor-check verify update
