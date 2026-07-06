package engine

import "time"

// DataplaneMetrics is the engine-side metrics seam for the carve/upload path.
// It follows the nil-safe Record* convention of pkg/metrics, which *Metrics
// satisfies. The engine depends on this interface rather than importing
// pkg/metrics directly, keeping pkg/block free of the metrics dependency
// (the same reason local.MetricsRecorder exists for the local store).
type DataplaneMetrics interface {
	// RecordUpload records one packed-block upload to remote: result is "ok"
	// or "error", bytes is the block size, d is the remote PutBlock latency.
	RecordUpload(bytes int, result string, d time.Duration)
	// UploadStarted/UploadFinished bracket one in-flight remote PutBlock so
	// the inflight gauge reflects the carver's effective upload concurrency.
	UploadStarted()
	UploadFinished()
	// SetUploadQueueDepth publishes the pending-carve backlog.
	SetUploadQueueDepth(n int)
	// SetUploadWindow publishes the target upload concurrency — the pinned
	// parallel_uploads value or the adaptive controller's current window (#1407).
	SetUploadWindow(n int)
	// SetUploadGoodput publishes the delivered bytes/sec the adaptive controller
	// measured over the last control interval (#1407).
	SetUploadGoodput(bytesPerSec float64)

	// --- Corruption detection / self-heal (read path) ---
	// All five counters are BOUNDED zero-label counters: no per-share, per-
	// hash, or per-block dimensions (the #1188 unbounded-cardinality lesson).

	// RecordLocalCorruption records n local-chunk integrity failures detected
	// on read (blake3 of the local bytes != the chunk's content hash). One
	// event per corrupt chunk surfaced to the read path.
	RecordLocalCorruption(n int)
	// RecordSelfHealSuccess records n local corruptions that were repaired:
	// the corrupt local pointer was dropped and the chunk was re-fetched from
	// its remote block, re-verified, and durably re-staged into local storage.
	RecordSelfHealSuccess(n int)
	// RecordSelfHealFailure records n local corruptions that could NOT be
	// repaired-and-persisted: the chunk was unsynced (only copy is corrupt),
	// the remote re-fetch failed/was-corrupt (read fails closed), or the good
	// bytes were served but the local re-stage did not persist (degraded).
	RecordSelfHealFailure(n int)
	// RecordRemoteCorruption records n remote-chunk integrity failures detected
	// on fetch (blake3 of the fetched bytes != the chunk's content hash). The
	// read fails closed; a corrupt remote is never self-healed from.
	RecordRemoteCorruption(n int)
	// RecordBlockRangeRead records one successful block-range read (a ranged
	// GET into a packed block object that passed per-chunk verification);
	// bytes is the verified chunk-plaintext length returned.
	RecordBlockRangeRead(bytes int)
}
