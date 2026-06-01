.PHONY: setup-hooks check-hooks fmt lint vet build bench-phase12 build-bench bench-blockstore bench-all test-unit test-e2e test-posix test-smb-conformance test-all

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

# Phase 12 perf gate (D-43): rand-read regression gate vs per-machine
# microbench floor in test/e2e/BENCHMARKS.md. -benchtime=10s gives a
# stable b.N auto-tune; -run=^$$ skips all tests so only the gate
# benchmark runs. The benchmark fails fast (b.Fatalf) on >5%
# regression, which surfaces as a non-zero exit from `go test -bench`.
bench-phase12:
	go test -bench BenchmarkPerfGate_Phase12 -benchtime=10s -run=^$$ ./pkg/blockstore/engine/...

# Build the unified bench CLI (cmd/bench). See bench/README.md.
# Output is dfsbench (not "bench") to avoid colliding with the
# bench/ source directory at the repo root.
build-bench:
	go build -o dfsbench ./cmd/bench

# Run blockstore Go benchmarks (10 iterations for benchstat).
bench-blockstore:
	go test -bench=. -benchtime=10x -run=^$$ ./bench/blockstore/

# Umbrella target — only blockstore is wired today. As gc / snapshots /
# metadata / adapters / e2e land, append their bench-<area> targets.
bench-all: bench-blockstore

# Run unit + integration tests with the race detector, matching CI.
test-unit:
	go test -race ./...

# E2E suite. Requires root + a kernel NFS client, so invoke under sudo:
# `sudo make test-e2e ARGS="--verbose --s3"`. Flags pass through via ARGS.
test-e2e:
	test/e2e/run-e2e.sh $(ARGS)

# POSIX compliance suite. Requires root (mounts a DittoFS export; see the
# script header), so invoke under sudo: `sudo make test-posix ARGS=chmod`.
test-posix:
	test/posix/run-posix.sh $(ARGS)

# SMB conformance suite (WPTS FileServer BVT). Pass flags via ARGS,
# e.g. `make test-smb-conformance ARGS="--profile badger-fs"`.
test-smb-conformance:
	test/smb-conformance/run.sh $(ARGS)

# Run unit tests followed by all three protocol suites. Uses a recipe
# (not prerequisites) with $(MAKE) so the suites run strictly in order
# even under `make -j` — the protocol suites share ports/mounts and must
# not run concurrently. Requires root for the e2e/posix stages.
test-all:
	$(MAKE) test-unit
	$(MAKE) test-e2e
	$(MAKE) test-posix
	$(MAKE) test-smb-conformance
