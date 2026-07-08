package parity

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/marmos91/dittofs/bench/blockstore"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/remote"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// sizeClass describes one dataset shape (large or small files).
type sizeClass struct {
	name      string
	fileBytes int64
	fileCount int
	uploadQ   string
	downloadQ string
}

func (o *Opts) classes() []sizeClass {
	return []sizeClass{
		{"large", o.LargeFileBytes, o.LargeFileCount, QuadUploadLarge, QuadDownloadLarge},
		{"small", o.SmallFileBytes, o.SmallFileCount, QuadUploadSmall, QuadDownloadSmall},
	}
}

// wantLanes reports which lanes of a size class the run selected. The meta lane
// is measured once, on the small class only.
func (o *Opts) wantLanes(cl sizeClass) (up, down, meta bool) {
	return o.wantQuadrant(cl.uploadQ),
		o.wantQuadrant(cl.downloadQ),
		o.wantQuadrant(QuadMeta) && cl.name == "small"
}

// runDittofsConc runs every selected dittofs cell at one concurrency level.
// Each size class gets a fresh engine (fresh local dir, fresh metrics
// registry, remote prefix isolated per cell); conc drives the number of
// concurrent writers so upload parallelism is comparable to rclone --transfers.
func runDittofsConc(ctx context.Context, opts Opts, s3cfg *s3Config, basePrefix string, conc int) ([]Cell, map[string]Timeline, error) {
	var cells []Cell
	timelines := map[string]Timeline{}

	for _, cl := range opts.classes() {
		wantUp, wantDown, wantMeta := opts.wantLanes(cl)
		if !wantUp && !wantDown && !wantMeta {
			continue
		}

		prefix := fmt.Sprintf("%s/dittofs/c%d/%s", basePrefix, conc, cl.name)
		remoteStore, err := s3cfg.newRemote(ctx, prefix)
		if err != nil {
			return nil, nil, err
		}
		cfg := engine.DefaultConfig()
		// ponytail: upload concurrency moved off SyncerConfig to the remote's
		// parallel_uploads config in the blocks-only model (#1493). Re-wire conc
		// onto the remote config if this bench's upload-concurrency axis matters.

		// Engine 1: upload. Fresh metadata + local dir + metrics registry.
		// KeepRemoteOpen: the parity runner owns the remote store's lifetime
		// across BOTH engines (upload + cold-restart download).
		upMx := metrics.New("bench", gitCommit())
		upDir := filepath.Join(opts.WorkDir, fmt.Sprintf("engine-c%d-%s-up", conc, cl.name))
		bs, ms, cleanup, err := blockstore.NewEngineWithOpts(upDir, remoteStore, blockstore.EngineOpts{
			Syncer: &cfg, Metrics: upMx, KeepRemoteOpen: true, PackedBlocks: true,
			ChunkParams: opts.ChunkParams(),
		})
		if err != nil {
			_ = remoteStore.Close()
			return nil, nil, fmt.Errorf("engine setup (%s c%d): %w", cl.name, conc, err)
		}

		upCell, upTL, err := dittofsUploadCell(ctx, opts, bs, remoteStore, upMx, cl, conc)
		cleanup() // upload engine done; ms lives on for the download engine
		if err != nil {
			_ = remoteStore.Close()
			return nil, nil, err
		}
		if wantUp {
			cells = append(cells, upCell)
			timelines[timelineKey(cl.uploadQ, conc)] = upTL
		}

		if wantDown {
			// Engine 2: cold-restart download. Same metadata store (so the
			// chunk manifest resolves), EMPTY local dir — every byte must
			// come through the remote read-through path.
			downMx := metrics.New("bench", gitCommit())
			downDir := filepath.Join(opts.WorkDir, fmt.Sprintf("engine-c%d-%s-down", conc, cl.name))
			bs2, _, cleanup2, err := blockstore.NewEngineWithOpts(downDir, remoteStore, blockstore.EngineOpts{
				Syncer: &cfg, Metrics: downMx, Metadata: ms, KeepRemoteOpen: true, PackedBlocks: true,
			})
			if err != nil {
				_ = remoteStore.Close()
				return nil, nil, fmt.Errorf("download engine setup (%s c%d): %w", cl.name, conc, err)
			}
			downCell, downTL, err := dittofsDownloadCell(ctx, opts, bs2, downMx, cl, conc)
			cleanup2()
			if err != nil {
				_ = remoteStore.Close()
				return nil, nil, err
			}
			cells = append(cells, downCell)
			timelines[timelineKey(cl.downloadQ, conc)] = downTL
		}

		_ = remoteStore.Close()

		if wantMeta {
			metaCell, err := dittofsMetaCell(ctx, opts, s3cfg, prefix, conc)
			if err != nil {
				return nil, nil, err
			}
			cells = append(cells, metaCell)
		} else if !opts.KeepRemote {
			// Untimed cleanup keeps bucket usage bounded across the sweep.
			if err := deleteRemotePrefix(ctx, s3cfg, prefix, conc); err != nil {
				fmt.Fprintf(os.Stderr, "parity: cleanup %s: %v\n", prefix, err)
			}
		}
	}
	return cells, timelines, nil
}

func timelineKey(quadrant string, conc int) string {
	return fmt.Sprintf("dittofs/%s/c%d", quadrant, conc)
}

// dittofsUploadCell writes the dataset into the engine with conc concurrent
// writers, then drains rollup + uploads. The clock covers write-start →
// fully-drained (the "dd → drain-uploads" methodology from the #1432
// baseline). Objects counts CAS + packed-block objects after the drain —
// blocks > 0 proves the carve path was exercised.
func dittofsUploadCell(ctx context.Context, opts Opts, bs *engine.Store, remoteStore remote.RemoteStore, mx *metrics.Metrics, cl sizeClass, conc int) (Cell, Timeline, error) {
	smp := newSampler(mx.Registry(), opts.Sample)
	smp.run(ctx)
	start := time.Now()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(conc)
	for i := 0; i < cl.fileCount; i++ {
		g.Go(func() error {
			return writePayload(gctx, bs, cl, i, opts.Seed)
		})
	}
	if err := g.Wait(); err != nil {
		smp.stop()
		return Cell{}, Timeline{}, fmt.Errorf("dittofs %s write (c%d): %w", cl.name, conc, err)
	}
	if err := bs.DrainAllUploads(ctx); err != nil {
		smp.stop()
		return Cell{}, Timeline{}, fmt.Errorf("dittofs %s drain (c%d): %w", cl.name, conc, err)
	}
	elapsed := time.Since(start)
	tl := smp.stop()

	casN, blockN := countRemoteObjects(ctx, remoteStore)
	totalBytes := cl.fileBytes * int64(cl.fileCount)
	cell := newCell(ToolDittofs, cl.uploadQ, conc, cl.fileCount, totalBytes, elapsed)
	cell.Objects = casN + blockN
	fmt.Printf("parity: dittofs %-14s c%-3d %8.1f Mbit/s  %6.1fs  (cas=%d blocks=%d)\n",
		cl.uploadQ, conc, cell.ThroughputMbps, cell.Seconds, casN, blockN)
	return cell, tl, nil
}

// writePayload streams one deterministic file into the engine in genChunk
// segments (protocol-client-sized writes).
func writePayload(ctx context.Context, bs *engine.Store, cl sizeClass, index int, seed uint64) error {
	pid := payloadID(cl.name, index)
	rng := rand.NewPCG(fileSeed(seed, cl.name, index), uint64(cl.fileBytes))
	buf := make([]byte, min(genChunk, cl.fileBytes))
	for off := int64(0); off < cl.fileBytes; {
		n := min(int64(len(buf)), cl.fileBytes-off)
		fillDeterministic(rng, buf[:n])
		if _, err := bs.WriteAt(ctx, pid, nil, buf[:n], uint64(off)); err != nil {
			return fmt.Errorf("WriteAt %s@%d: %w", pid, off, err)
		}
		off += n
	}
	return nil
}

// dittofsDownloadCell reads the full dataset back with conc concurrent
// readers on a COLD engine: same metadata (the chunk manifest resolves), but
// an empty local store — every byte must come from the remote, the
// cold-restart / cache-lost scenario. RemoteReadBytes is the verified
// ranged-block-read counter delta (a lower bound on remote traffic; the
// parallel-download pipeline fetches whole blocks through a separate path).
func dittofsDownloadCell(ctx context.Context, opts Opts, bs *engine.Store, mx *metrics.Metrics, cl sizeClass, conc int) (Cell, Timeline, error) {
	readBytesBefore := remoteReadBytes(mx)

	smp := newSampler(mx.Registry(), opts.Sample)
	smp.run(ctx)
	start := time.Now()

	var read atomic.Int64
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(conc)
	for i := 0; i < cl.fileCount; i++ {
		g.Go(func() error {
			n, err := readPayload(gctx, bs, cl, i)
			read.Add(n)
			return err
		})
	}
	err := g.Wait()
	elapsed := time.Since(start)
	tl := smp.stop()
	if err != nil {
		return Cell{}, Timeline{}, fmt.Errorf("dittofs %s read (c%d): %w", cl.name, conc, err)
	}

	cell := newCell(ToolDittofs, cl.downloadQ, conc, cl.fileCount, read.Load(), elapsed)
	cell.RemoteReadBytes = remoteReadBytes(mx) - readBytesBefore
	fmt.Printf("parity: dittofs %-14s c%-3d %8.1f Mbit/s  %6.1fs  (remote-read=%dMiB)\n",
		cl.downloadQ, conc, cell.ThroughputMbps, cell.Seconds, cell.RemoteReadBytes>>20)
	return cell, tl, nil
}

func readPayload(ctx context.Context, bs *engine.Store, cl sizeClass, index int) (int64, error) {
	pid := payloadID(cl.name, index)
	buf := make([]byte, min(genChunk, cl.fileBytes))
	var total int64
	for off := int64(0); off < cl.fileBytes; {
		n := min(int64(len(buf)), cl.fileBytes-off)
		got, err := bs.ReadAt(ctx, pid, nil, buf[:n], uint64(off))
		if err != nil {
			return total, fmt.Errorf("ReadAt %s@%d: %w", pid, off, err)
		}
		if int64(got) != n {
			return total, fmt.Errorf("ReadAt %s@%d: short read %d != %d", pid, off, got, n)
		}
		off += n
		total += n
	}
	return total, nil
}

// dittofsMetaCell lists then deletes every remote object under prefix using
// dittofs's own S3 client with conc parallel deleters. The listing/deletion
// object count also documents the packing asymmetry vs rclone (few packed
// blocks vs one object per file).
func dittofsMetaCell(ctx context.Context, opts Opts, s3cfg *s3Config, prefix string, conc int) (Cell, error) {
	store, err := s3cfg.newRemote(ctx, prefix)
	if err != nil {
		return Cell{}, err
	}
	defer func() { _ = store.Close() }()

	start := time.Now()
	var blockIDs []string
	rbs, hasBlocks := store.(remote.RemoteBlockStore)
	if hasBlocks {
		if err := rbs.WalkBlocks(ctx, func(id string, _ block.Meta) error {
			blockIDs = append(blockIDs, id)
			return nil
		}); err != nil {
			return Cell{}, fmt.Errorf("meta walk blocks: %w", err)
		}
	}

	objects := int64(len(blockIDs))
	if !opts.KeepRemote {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(conc)
		for _, id := range blockIDs {
			g.Go(func() error { return rbs.DeleteBlock(gctx, id) })
		}
		if err := g.Wait(); err != nil {
			return Cell{}, fmt.Errorf("meta delete: %w", err)
		}
	}
	elapsed := time.Since(start)

	cell := newCell(ToolDittofs, QuadMeta, conc, 0, 0, elapsed)
	cell.Objects = objects
	if elapsed > 0 {
		cell.OpsPerSec = float64(objects) / elapsed.Seconds()
	}
	fmt.Printf("parity: dittofs %-14s c%-3d %8.1f obj/s   %6.1fs  (objects=%d)\n",
		QuadMeta, conc, cell.OpsPerSec, cell.Seconds, objects)
	return cell, nil
}

// deleteRemotePrefix is the untimed cleanup path for non-meta cells: list and
// delete every CAS + packed-block object under prefix with conc deleters.
func deleteRemotePrefix(ctx context.Context, s3cfg *s3Config, prefix string, conc int) error {
	store, err := s3cfg.newRemote(ctx, prefix)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(conc)
	var walkErr error
	if rbs, ok := store.(remote.RemoteBlockStore); ok {
		walkErr = rbs.WalkBlocks(ctx, func(id string, _ block.Meta) error {
			g.Go(func() error { return rbs.DeleteBlock(gctx, id) })
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	return walkErr
}

func payloadID(class string, index int) string {
	return fmt.Sprintf("parity/%s/%s", class, fileName(index))
}

func countRemoteObjects(ctx context.Context, store remote.RemoteStore) (cas, blocks int64) {
	// cas is always 0 in the blocks-only model (#1493); kept in the signature so
	// the scorecard columns stay stable.
	if rbs, ok := store.(remote.RemoteBlockStore); ok {
		_ = rbs.WalkBlocks(ctx, func(string, block.Meta) error { blocks++; return nil })
	}
	return cas, blocks
}

func remoteReadBytes(mx *metrics.Metrics) int64 {
	families, err := mx.Registry().Gather()
	if err != nil {
		return 0
	}
	for _, mf := range families {
		if mf.GetName() == "dittofs_datapath_block_range_read_bytes_total" {
			return int64(firstCounter(mf))
		}
	}
	return 0
}

func newCell(tool, quadrant string, conc, files int, bytes int64, d time.Duration) Cell {
	c := Cell{Tool: tool, Quadrant: quadrant, Conc: conc, Files: files, Bytes: bytes, Seconds: d.Seconds()}
	if d > 0 && bytes > 0 {
		c.ThroughputMbps = float64(bytes) * 8 / 1e6 / d.Seconds()
	}
	if d > 0 && files > 0 {
		c.OpsPerSec = float64(files) / d.Seconds()
	}
	return c
}
