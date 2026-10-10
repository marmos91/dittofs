package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
)

// warmRegistry lets shutdown cancel warm work before waiting for lifecycle
// readers. A progress callback may itself call Close while another worker is
// downloading under a lifecycle pin, so cancellation cannot wait for that pin.
// Only warm runs register here; ordinary data operations still drain normally.
type warmRegistry struct {
	mu      sync.Mutex
	closing bool
	runs    map[*warmRun]struct{}
}

type warmRun struct {
	cancel context.CancelCauseFunc
}

// Distinguish shutdown from a caller cancelling with its own error cause.
var errWarmShutdown = errors.New("engine: warm stopped by shutdown")

func (r *warmRegistry) begin(ctx context.Context) (context.Context, func(), error) {
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closing {
		return ctx, nil, ErrStoreClosed
	}
	ctx, cancel := context.WithCancelCause(ctx)
	run := &warmRun{cancel: cancel}
	if r.runs == nil {
		r.runs = make(map[*warmRun]struct{})
	}
	r.runs[run] = struct{}{}
	return ctx, func() {
		r.mu.Lock()
		delete(r.runs, run)
		r.mu.Unlock()
		cancel(nil)
	}, nil
}

func (r *warmRegistry) stop() {
	r.mu.Lock()
	r.closing = true
	runs := make([]*warmRun, 0, len(r.runs))
	for run := range r.runs {
		runs = append(runs, run)
	}
	r.mu.Unlock()
	for _, run := range runs {
		run.cancel(errWarmShutdown)
	}
}

// WarmResult summarizes a WarmAll run: how many chunks came back from the
// remote tier and how many bytes they moved. Sparse, removed and unsynced rows
// count toward progress but not BlocksFetched. See WarmAll for why there is no
// already-local count.
type WarmResult struct {
	BlocksFetched int64 `json:"blocks_fetched"`
	BytesFetched  int64 `json:"bytes_fetched"`
}

type warmChunk struct {
	fb   *block.FileChunk
	span hydrateSpan
}

// warmFile retains a manifest snapshot without retaining payload admission.
// The observer pins only the admission entry's identity, so a replacement can
// advance its epoch even between the planning pass and the first download.
// Workers share one refreshed snapshot per epoch instead of rescanning the
// whole manifest to find the successor of every chunk.
type warmFile struct {
	payloadID string
	version   func() uint64
	mu        sync.Mutex
	epoch     uint64
	chunks    map[string]warmChunk
}

// warmTarget keeps the originally enumerated row ID, not its content. An
// exclusive replacement at that ID is resolved under admission when the worker
// runs; removed IDs become processed skips and new IDs wait for the next run.
type warmTarget struct {
	file *warmFile
	id   string
}

type warmEnter func(context.Context, ...string) (context.Context, func(), error)
type warmObserve func(context.Context, string) (func() uint64, func(), error)

// snapshot resolves all rows and their overlap bounds while the caller owns
// shared payload admission. Its returned IDs define this run's fixed work list.
func (f *warmFile) snapshot(ctx context.Context, m *RemoteSync) ([]string, error) {
	// A write arriving after the dirty check must be newer than this bound.
	at := m.local.WriteVersion()
	dirty, err := m.local.HasDirty(ctx, journal.FileID(f.payloadID))
	if err != nil {
		return nil, fmt.Errorf("warm: inspect dirty data for %s: %w", f.payloadID, err)
	}
	if dirty {
		// decision: dirty bytes or an unfinished reap can predate this snapshot
		// while manifest rows still describe old contents. Carving and eviction
		// keep their version, so the initial bound excludes every recorded cold
		// interval. Fetch/count every planned row, but leave this dirty file's
		// cold ranges for demand reads or a later warm run. Range-level dirty
		// provenance would let a future warmer fill its unchanged cold ranges.
		at = 0
	}
	rows, err := m.listFileChunksSnapshot(ctx, f.payloadID)
	if err != nil {
		return nil, fmt.Errorf("warm: list blocks for %s: %w", f.payloadID, err)
	}
	starts := make([]uint64, 0, len(rows))
	for _, fb := range rows {
		if fb != nil {
			if absOff, ok := block.ParseChunkOffset(fb.ID); ok {
				starts = append(starts, absOff)
			}
		}
	}
	slices.Sort(starts)

	chunks := make(map[string]warmChunk, len(rows))
	ids := make([]string, 0, len(rows))
	for _, fb := range rows {
		if fb == nil {
			continue
		}
		absOff, ok := block.ParseChunkOffset(fb.ID)
		if !ok {
			continue
		}
		// A row gives up its claim at the next row's start. Hydrating its
		// whole extent would overwrite the newer row's head; any remainder
		// after the overlap is left for the demand reader to resolve.
		span := hydrateSpan{From: absOff, To: absOff + uint64(fb.DataSize), At: at}
		if i := sort.Search(len(starts), func(i int) bool { return starts[i] > absOff }); i < len(starts) && starts[i] < span.To {
			span.To = starts[i]
		}
		row := *fb
		chunks[fb.ID] = warmChunk{fb: &row, span: span}
		ids = append(ids, fb.ID)
	}
	f.chunks, f.epoch = chunks, f.version()
	return ids, nil
}

// resolve runs inside a worker's payload scope. Exclusive replacements cannot
// change the epoch until that worker finishes hydrating, while the journal's
// sampled write version protects against ordinary concurrent writes/truncates.
func (f *warmFile) resolve(ctx context.Context, m *RemoteSync, id string) (warmChunk, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.version() != f.epoch {
		if _, err := f.snapshot(ctx, m); err != nil {
			return warmChunk{}, err
		}
	}
	return f.chunks[id], nil
}

// WarmAll proactively fetches this share's planned remote chunks and fills
// eligible ranges in the local tier. Authoritative FileChunk metadata also finds
// payloads whose local journal has been discarded after upload. Downloads are
// bounded by ParallelDownloads; admission covers only a worker's current file.
//
// decision: warm attempts every planned row, including already-resident
// ranges, so progress describes the manifest walk and fetched counts describe
// successful remote downloads. Rehydration is idempotent. A residency-based
// skip should explicitly define how skipped ranges contribute to these counts.
//
// progress (may be nil) receives ordered (done, total) counts, starting at zero.
// total counts the valid row IDs found during planning. Removed rows are
// processed skips, exclusive replacements at the same ID use current content,
// and newly added IDs wait for another run. Ordinary writes after planning are
// fenced by the sampled journal version. Files with dirty or unpublished data
// at planning use the initial zero bound: their manifest can predate writes or
// still hold rows awaiting reap. Either case can leave cold ranges for a demand
// read or another warm run. Callbacks run synchronously on the caller, outside
// all lifecycle and payload pins, and may inspect or mutate the engine.
// Every worker is joined and every callback has finished before return. A
// callback panic propagates after active workers are cancelled and joined.
//
// A missing remote tier is an error. A full local tier or any fetch error
// cancels the remaining work; the result still counts successful downloads.
func (m *RemoteSync) WarmAll(ctx context.Context, progress func(done, total int64)) (WarmResult, error) {
	return m.warmAll(ctx, progress, m.enterWarmScope, m.admission.observe)
}

// enterWarmScope is the standalone syncer's admission path. The engine supplies
// enterPayload instead, pinning its lifecycle for each short scope as well.
func (m *RemoteSync) enterWarmScope(ctx context.Context, ids ...string) (context.Context, func(), error) {
	if err := m.checkReady(ctx); err != nil {
		return ctx, nil, err
	}
	if len(ids) == 0 {
		return ctx, func() {}, nil
	}
	release, err := m.admission.enter(ctx, ids[0])
	return ctx, release, err
}

func (m *RemoteSync) warmAll(ctx context.Context, progress func(done, total int64), enter warmEnter, observe warmObserve) (WarmResult, error) {
	var payloadIDs []string
	if err := func() error {
		ctx, release, err := enter(ctx)
		if err != nil {
			return err
		}
		defer release()
		if err := m.checkReady(ctx); err != nil {
			return err
		}
		if m.remoteStore == nil {
			return errors.New("warm: share has no remote tier to warm from")
		}
		if err := m.fileChunkStore.EnumeratePayloads(ctx, func(id string) error {
			payloadIDs = append(payloadIDs, id)
			return nil
		}); err != nil {
			return fmt.Errorf("warm: enumerate payloads: %w", err)
		}
		return nil
	}(); err != nil {
		return WarmResult{}, err
	}
	slices.Sort(payloadIDs)
	payloadIDs = slices.Compact(payloadIDs)

	var observers []func()
	defer func() {
		for _, release := range observers {
			release()
		}
	}()
	var targets []warmTarget
	for _, id := range payloadIDs {
		if err := func() error {
			ctx, release, err := enter(ctx, id)
			if err != nil {
				return err
			}
			defer release()
			version, unobserve, err := observe(ctx, id)
			if err != nil {
				return err
			}
			observers = append(observers, unobserve)
			file := &warmFile{payloadID: id, version: version}
			ids, err := file.snapshot(ctx, m)
			if err != nil {
				return err
			}
			for _, rowID := range ids {
				targets = append(targets, warmTarget{file: file, id: rowID})
			}
			return nil
		}(); err != nil {
			return WarmResult{}, err
		}
	}

	total := int64(len(targets))
	if progress != nil {
		progress(0, total)
	}
	if total == 0 {
		return WarmResult{}, nil
	}

	workers := min(len(targets), max(1, m.config.ParallelDownloads))
	type completion struct {
		fetched bool
		bytes   int64
	}
	completed := make(chan completion, workers)
	var next atomic.Int64
	workerCtx, cancelWorkers := context.WithCancel(ctx)
	g, gctx := m.fetchGroup(workerCtx)
	for range workers {
		g.Go(func() error {
			for {
				i := int(next.Add(1) - 1)
				if i >= len(targets) {
					return nil // A final callback may close a fully completed run.
				}
				if err := gctx.Err(); err != nil {
					return err
				}
				target := targets[i]
				data, err := func() ([]byte, error) {
					ctx, release, err := enter(gctx, target.file.payloadID)
					if err != nil {
						return nil, err
					}
					defer release()
					chunk, err := target.file.resolve(ctx, m, target.id)
					if err != nil {
						return nil, err
					}
					return m.fetchResolvedBlock(ctx, chunk.fb, chunk.span)
				}()
				if err != nil {
					if errors.Is(err, journal.ErrLocalStoreFull) {
						return fmt.Errorf("warm: local tier full while fetching %s (raise journal_size or evict): %w", target.id, err)
					}
					return fmt.Errorf("warm: fetch %s: %w", target.id, err)
				}
				// Never send while holding admission or a lifecycle pin. Even
				// a callback waiting for Close or a replacement can let all
				// active scopes drain when this bounded buffer fills.
				completed <- completion{fetched: data != nil, bytes: int64(len(data))}
			}
		})
	}
	finished := make(chan struct{})
	var fetchErr error
	go func() {
		fetchErr = g.Wait()
		close(completed)
		close(finished)
	}()
	defer func() {
		// A callback can panic after workers start. Cancel remote operations
		// and drain successful completions so no sender remains blocked, then
		// join before the earlier defer releases the manifest observers.
		cancelWorkers()
		for range completed {
		}
		<-finished
	}()
	var result WarmResult
	var done int64
	for event := range completed {
		if event.fetched {
			result.BlocksFetched++
			result.BytesFetched += event.bytes
		}
		done++
		if progress != nil {
			progress(done, total)
		}
	}
	return result, fetchErr
}
