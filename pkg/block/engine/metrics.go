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
	// parallel_uploads value, or the adaptive controller's current window when
	// the window is not pinned.
	SetUploadWindow(n int)
	// SetUploadGoodput publishes the delivered bytes/sec the adaptive controller
	// measured over the last control interval — the signal it steers the window
	// by, paired with SetUploadWindow so a window change can be read against the
	// throughput that caused it.
	SetUploadGoodput(bytesPerSec float64)

	// --- Corruption detection / self-heal (read path) ---
	// All five counters are BOUNDED zero-label counters: no per-share, per-
	// hash, or per-block dimensions — any of those would make the label set grow
	// with the data, not with the config, and the series count unbounded.

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

// dataplaneMetrics returns the engine's data-plane metrics sink, or nil when
// the syncer is detached from a Store or no recorder was injected. Call sites
// must guard the result: it is a plain interface, not a nil-safe *Metrics.
func (m *RemoteSync) dataplaneMetrics() DataplaneMetrics {
	if m.metrics == nil {
		return nil
	}
	if p := m.metrics.Load(); p != nil {
		return *p
	}
	return nil
}

// SyncCounts returns lifetime (completed, failed) sync counts: blocks that
// reached remote and failed carve upload attempts.
func (m *RemoteSync) SyncCounts() (completed, failed int) {
	return int(m.completedSyncs.Load()), int(m.failedSyncs.Load())
}

// noteBlockCommitted records one block reaching the remote durably. Every carve
// routes its commits through the same sink, so counting here covers both the
// background dispatcher and the drain's force-carve — the latter runs as a
// single call that can span minutes, and counting only on its return would
// leave the progress signal flat for that whole time. The block's byte count
// goes to noteBlockUploaded instead, one step earlier.
func (m *RemoteSync) noteBlockCommitted(int64) {
	m.completedSyncs.Add(1)
}
