//go:build e2e

// Package snapshot holds e2e tests for the snapshot REST surface. The
// tests live behind the `e2e` build tag so they do not run as part of
// the default `go test ./...` sweep — invoke with
// `go test -tags=e2e ./test/e2e/snapshot/...`.
package snapshot

import (
	"os"
	"testing"
)

// TestMain is a placeholder so the package compiles cleanly under the
// e2e build tag. The snapshot e2e tests are self-contained — they do
// not need the Docker-backed test environment that the parent e2e
// package boots.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
