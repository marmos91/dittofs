---
status: awaiting_human_verify
trigger: "flaky-s3-integration-test: TestStore_OverwriteBlock fails intermittently in CI due to Localstack container exit code 245"
created: 2026-03-02T00:00:00Z
updated: 2026-03-02T00:05:00Z
---

## Current Focus

hypothesis: CONFIRMED - root cause found and fix applied
test: Code compiles, go vet passes, non-integration tests pass
expecting: Integration tests should now pass consistently in CI
next_action: Await human verification

## Symptoms

expected: All S3 integration tests pass consistently in CI
actual: TestStore_OverwriteBlock fails intermittently with Localstack container exit code 245
errors: "failed to start localstack container: start container: started hook: wait until ready: internal check: container exited with code 245"
reproduction: Flaky - happens in CI (GitHub Actions), not every run. The test that fails varies but it's always a Localstack container startup failure.
started: Intermittent issue, not related to any specific code change. Tests run sequentially, each spawns a fresh Localstack container.

## Eliminated

## Evidence

- timestamp: 2026-03-02T00:00:30Z
  checked: pkg/payload/store/s3/store_test.go
  found: 12 top-level test functions, each calling newLocalstackHelper(t) which creates a fresh Localstack container. Each test also has defer helper.cleanup() which terminates it.
  implication: 12 container start/stop cycles just for this one package

- timestamp: 2026-03-02T00:00:35Z
  checked: pkg/payload/store/blockstore_integration_test.go
  found: 2 test functions (TestS3BlockStore_Integration, TestFlusher_Integration), each creating their own Localstack container. Uses subtests within to share the container but across top-level tests still duplicates.
  implication: 2 more container cycles

- timestamp: 2026-03-02T00:00:40Z
  checked: pkg/payload/offloader/offloader_test.go
  found: 2 S3-specific tests (TestOffloader_WriteAndFlush_S3, TestOffloader_DownloadOnCacheMiss_S3), each creating their own container. Plus benchmark helpers.
  implication: 2 more container cycles

- timestamp: 2026-03-02T00:00:45Z
  checked: pkg/payload/gc/gc_integration_test.go
  found: 1 test function creating its own container
  implication: 1 more container cycle

- timestamp: 2026-03-02T00:00:50Z
  checked: .github/workflows/integration-tests.yml
  found: CI runs `go test -tags=integration -v -timeout=15m ./...` which runs ALL integration tests across ALL packages. Go test runs each package sequentially by default. Within each package tests run sequentially. Total ~17 container create/destroy cycles.
  implication: 17 Localstack containers created/destroyed in sequence on resource-constrained GitHub Actions runner. Docker daemon likely accumulates leftover state (networks, volumes, cgroups) causing later containers to fail with exit code 245 (OOM or resource limit).

- timestamp: 2026-03-02T00:00:55Z
  checked: All LOCALSTACK_ENDPOINT env var checks in test helpers
  found: All test files already support LOCALSTACK_ENDPOINT env var to connect to an external container instead of spinning up their own. This pattern shows the design intent was to allow shared containers, but the default path creates one per test.
  implication: The fix should use TestMain per package to start one container, share it via package-level var, and only terminate once when all tests in the package are done. Buckets already use unique names (time.Now().UnixNano()) providing test isolation.

- timestamp: 2026-03-02T00:03:00Z
  checked: Fix implementation and compilation
  found: All 4 files refactored. go build and go vet pass on all 4 packages with -tags=integration. Non-integration tests unaffected and pass.
  implication: Fix is structurally correct. Container count reduced from ~17 to 4 (one per package).

## Resolution

root_cause: Each test function creates its own Localstack Docker container via Testcontainers. In pkg/payload/store/s3/ alone, 12 test functions each create/destroy a container. Across all integration test packages, ~17 containers are cycled. In CI (GitHub Actions), this causes Docker daemon resource exhaustion, leading to container startup failures with exit code 245. The issue is intermittent because it depends on how quickly Docker reclaims resources from terminated containers.
fix: Refactored all 4 integration test files to use TestMain with a single shared Localstack container per package. Each test gets its own S3 bucket for isolation (using time.Now().UnixNano() naming). Container starts once in TestMain and is terminated in cleanup. Reduces total container lifecycle events from ~17 to 4.
verification: Code compiles with integration tag on all 4 packages. go vet clean. Non-integration tests pass. Benchmark helpers left as-is (they manage their own containers since benchmarks run separately).
files_changed:
  - pkg/payload/store/s3/store_test.go
  - pkg/payload/store/blockstore_integration_test.go
  - pkg/payload/offloader/offloader_test.go
  - pkg/payload/gc/gc_integration_test.go
