package local

import (
	"context"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// MetricsRecorder is the narrow inline-metrics seam the local-store
// eviction/backpressure path emits to. *metrics.Metrics satisfies it.
// Defining it here (not in pkg/metrics) keeps the low-level block stores free
// of the prometheus dependency and avoids any import cycle: the runtime hands
// a recorder down via MetricsAware after construction. A recorder whose
// underlying handle is nil makes every method a no-op.
type MetricsRecorder interface {
	// RecordBackpressure records one write stall under local-cache
	// backpressure and the duration it waited for space.
	RecordBackpressure(d time.Duration)
	// RecordEviction records one evicted local-cache chunk and the bytes it
	// reclaimed.
	RecordEviction(bytes int64)
}

// MetricsAware is the optional capability a [LocalStore] exposes to receive
// the inline metrics recorder. The engine probes for it and forwards the
// handle the runtime hands down (shares are constructed before the metrics
// registry exists, so the handle arrives post-construction). Stores that emit
// no inline metrics simply don't implement it.
type MetricsAware interface {
	// SetMetrics installs the inline metrics recorder. Safe to call after the
	// store is serving; the hot path reads it atomically. A nil recorder (or
	// one with a nil underlying handle) disables recording.
	SetMetrics(rec MetricsRecorder)
}

// ChunkLifecycleHooks is the optional capability surface a [LocalStore]
// may expose to let the engine wire callbacks for rollup-time and
// chunk-emission events. Engine.New probes for this interface on
// cfg.Local at construction; implementations that don't need a given
// callback may install no-op setters (so the assertion always succeeds)
// and rely on engine.New supplying nil-safe closures.
//
// Implementations:
//
//   - *fs.FSStore — installs the rollup-completion FileBlock + ObjectID
//     persister and the synchronous chunk-completion cache-warming
//     callback. The chunk emitter is a no-op (FSStore drives FileBlock
//     rows through the rollup persister path instead).
//
//   - *memory.MemoryStore — installs the per-chunk emitter (in-memory
//     rollup fires this on every CAS chunk). The rollup-completion
//     persister and chunk-completion callbacks are no-ops; in-memory
//     callers don't materialize through the CAS chunkstore + cache hot
//     path that those two hooks support.
//
// The interface is defined here (alongside [LocalStore]) rather than at
// the engine consumer site because the contract is naturally a
// store-side capability surface: a foreign LocalStore implementation
// (e.g. a future backend) declares its participation by satisfying this
// interface, and the engine asserts once on the named type instead of
// three anonymous structural probes.
type ChunkLifecycleHooks interface {
	// SetObjectIDPersister installs the rollup-completion callback. The
	// engine wires a closure that delegates to the metadata coordinator's
	// PersistFileBlocks and writes per-block FileBlock rows so the
	// engine's CAS read path can resolve (payloadID, offset) -> hash.
	// Called once at engine.New; implementations that don't drive a
	// rollup-completion path may treat this as a no-op.
	SetObjectIDPersister(p func(ctx context.Context, payloadID string, blocks []block.BlockRef, objectID block.ObjectID) error)

	// SetOnChunkComplete installs the chunk-completion callback fired
	// once per successful chunkstore write. The engine wires a closure
	// that warms the read Cache on the write side so NFS COMMIT-then-
	// READ never falls back to disk for just-written chunks. The path
	// argument is informational (firing site contract); current
	// callers discard it. Implementations that don't materialize
	// through a hot-path chunkstore may treat this as a no-op.
	SetOnChunkComplete(fn func(hash block.ContentHash, data []byte, path string))

	// SetChunkEmitter installs the per-chunk emitter fired once per
	// freshly-emitted CAS chunk during synchronous rollup. The engine
	// wires a closure that mirrors each chunk into a FileBlock row so
	// the CAS read path can resolve (payloadID, offset) -> hash without
	// a separate manifest. Implementations that drive FileBlock rows
	// through a different path (e.g. rollup-completion persister) may
	// treat this as a no-op.
	SetChunkEmitter(emit func(payloadID string, chunkStart uint64, size uint32, hash block.ContentHash))
}
