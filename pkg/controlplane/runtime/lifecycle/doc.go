// Package lifecycle provides server startup and shutdown orchestration.
//
// The Service coordinates the lifecycle of all DittoFS components:
// adapter loading, API server startup, graceful shutdown with metadata
// flushing, and ordered component teardown.
package lifecycle
