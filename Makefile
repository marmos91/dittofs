.PHONY: setup-hooks check-hooks fmt lint vet build

# Configure git to use the project's hooks directory and make hooks
# executable. Safe to re-run.
setup-hooks:
	@set -e; \
	if [ ! -d .githooks ]; then echo "setup-hooks: .githooks/ not found"; exit 1; fi; \
	chmod +x .githooks/*; \
	git config core.hooksPath .githooks; \
	echo "Git hooks configured:"; \
	for h in .githooks/*; do \
	    [ -f "$$h" ] || continue; \
	    name=$$(basename "$$h"); \
	    if [ -x "$$h" ]; then echo "  ✓ $$name"; else echo "  ✗ $$name (not executable)"; fi; \
	done; \
	echo "hooksPath = $$(git config --get core.hooksPath)"

# Verify every hook in .githooks/ is executable and wired via core.hooksPath.
check-hooks:
	@set -e; \
	want=$$(cd "$$(git rev-parse --show-toplevel)" && pwd)/.githooks; \
	got=$$(git config --get core.hooksPath || true); \
	resolved=$$(cd "$$(git rev-parse --show-toplevel)" && cd "$$got" 2>/dev/null && pwd || echo "$$got"); \
	if [ "$$resolved" != "$$want" ]; then \
	    echo "core.hooksPath = '$$got' (want '.githooks'). Run 'make setup-hooks'."; exit 1; \
	fi; \
	for h in .githooks/*; do \
	    [ -f "$$h" ] || continue; \
	    [ -x "$$h" ] || { echo "$$h is not executable"; exit 1; }; \
	done; \
	echo "hooks OK"

# Format all Go source files
fmt:
	gofmt -s -w .

# Run golangci-lint (must be installed separately)
lint:
	golangci-lint run

# Run go vet
vet:
	go vet ./...

# Build both CLI binaries
build:
	go build -o dfs cmd/dfs/main.go
	go build -o dfsctl cmd/dfsctl/main.go
