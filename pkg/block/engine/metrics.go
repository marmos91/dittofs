package engine

import "time"

// DataplaneMetrics is the engine-side metrics seam for the mirror/upload path.
// It follows the nil-safe Record* convention of pkg/metrics, which *Metrics
// satisfies. The engine depends on this interface rather than importing
// pkg/metrics directly, keeping pkg/block free of the metrics dependency
// (the same reason local.MetricsRecorder exists for the local store).
type DataplaneMetrics interface {
	// RecordUpload records one CAS chunk upload to remote: result is "ok" or
	// "error", bytes is the chunk size, d is the remote Put latency.
	RecordUpload(bytes int, result string, d time.Duration)
	// UploadStarted/UploadFinished bracket one in-flight remote Put so the
	// inflight gauge reflects the mirror loop's effective upload concurrency.
	UploadStarted()
	UploadFinished()
	// SetUploadQueueDepth publishes pending-upload backlog at mirror-pass start.
	SetUploadQueueDepth(n int)
	// SetUploadWindow publishes the upload limiter's current in-flight ceiling
	// (the adaptive congestion window, or the static --parallel-uploads cap).
	SetUploadWindow(n int)
	// RecordRehash records pre-upload BLAKE3 re-hash latency for one chunk.
	RecordRehash(d time.Duration)
}
