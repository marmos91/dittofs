package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// OfflineReadiness reports whether a share could keep serving reads with its
// remote store unreachable, and by how much it falls short.
//
// Every byte the share holds is either resident in the local tier or it is
// not. A range that is not — one written, mirrored to the remote and since
// evicted locally — needs the remote to serve, so a share with any such range
// would fail reads during an outage.
//
// The local tier's interval index is what reports those ranges, and it can only
// speak for ranges it still describes. An interval lost rather than evicted
// leaves nothing behind: it adds nothing to the tally, and a range the index has
// forgotten is indistinguishable there from one that was never written. So the
// tally alone proves nothing, and the measurement weighs the index against the
// share's manifest — an independent record of what the share holds — before it
// calls anything safe. Zero remote-only bytes AND no range the manifest places
// that the index cannot account for is a provably offline-safe share; a
// shortfall against the manifest makes the answer indeterminate rather than
// safe.
type OfflineReadiness struct {
	// RemoteOnlyBytes is the total size of ranges the local tier no longer
	// holds and would have to fetch from the remote to serve.
	RemoteOnlyBytes int64 `json:"remote_only_bytes"`

	// RemoteOnlyRanges is how many separate ranges those bytes span.
	RemoteOnlyRanges int64 `json:"remote_only_ranges"`

	// Known reports whether the counts above mean anything. When it is false
	// the share's residency cannot be determined and Reason says why —
	// reporting zero in that case would read as "offline safe" for exactly
	// the shares whose data is most likely to be remote-only.
	Known bool `json:"known"`

	// Reason names why Known is false, empty when it is true.
	Reason string `json:"reason,omitempty"`
}

// Safe reports whether the share can serve every byte it holds without the
// remote. An indeterminate readiness is never safe.
func (o OfflineReadiness) Safe() bool { return o.Known && o.RemoteOnlyBytes == 0 }

// coldRangeReporter is the local-tier capability this needs: which of its
// ranges are remote-only, which ranges it describes for one file, and whether
// it has been told what the manifest holds yet. The journal-backed tier
// satisfies it; the in-memory tier does not, and asserting for it here keeps
// that from becoming another method every fake has to implement.
type coldRangeReporter interface {
	ColdExtents(ctx context.Context) (bytes int64, extents int64, err error)
	ColdSeeded() bool
	DataExtents(ctx context.Context, payloadID string, size int64) ([][2]uint64, error)
}

// manifestLister is the manifest surface the cross-check reads: which files the
// share has, and which ranges each one's rows place. block.EngineFileChunkStore
// satisfies it.
type manifestLister interface {
	EnumeratePayloads(ctx context.Context, fn func(payloadID string) error) error
	ListFileChunks(ctx context.Context, payloadID string) ([]*block.FileChunk, error)
}

// shortfallFunc reports how many bytes, in how many ranges, the share's
// manifest places that the local tier's index does not describe at all. It is
// passed into the gating rules rather than reached for, so those rules can be
// exercised without a metadata store and so the store can memoize the walk.
type shortfallFunc func(ctx context.Context, index coldRangeReporter) (bytes int64, ranges int64, err error)

// errNoManifest is what the cross-check reports when the share has no manifest
// to weigh its index against. It is a non-answer, not a clean bill of health:
// the index alone cannot report a range it has forgotten.
var errNoManifest = errors.New("share has no manifest to check the local index against")

// OfflineReadiness measures how much of this share's data is remote-only, and
// whether the local index can account for everything the manifest places.
//
// ponytail: an O(live ranges) in-memory scan of the local tier's index on
// every call, holding one shard's lock at a time. Keeping a running counter
// instead would make it O(1), but the cold flag is set and cleared across
// insert, split, clamp, evict, hydrate and repack, and a counter that drifts
// on any one of those reports a share offline-safe when it is not. Take the
// scan until a metrics scrape actually shows up in a latency profile.
func (bs *Store) OfflineReadiness(ctx context.Context) OfflineReadiness {
	bs.closeMu.RLock()
	defer bs.closeMu.RUnlock()
	if bs.closed {
		return OfflineReadiness{Reason: "block store is closed"}
	}
	return offlineReadinessOf(ctx, bs.local, bs.HasRemoteStore(), bs.memoizedShortfall)
}

// memoizedShortfall is the store's cross-check, memoized. See shortfallMemo for
// why the result is reused rather than recomputed on every call.
func (bs *Store) memoizedShortfall(ctx context.Context, index coldRangeReporter) (int64, int64, error) {
	if bs.fileChunkStore == nil {
		return 0, 0, errNoManifest
	}
	return bs.offlineShortfall.get(ctx, index, bs.fileChunkStore)
}

// offlineReadinessOf holds the gating rules, separated from the store lookup so
// they can be exercised directly. Each of them refuses to answer rather than
// answering zero, because a zero here reads as "provably offline safe" and
// would say that about exactly the shares whose data is most likely to be
// remote-only.
func offlineReadinessOf(ctx context.Context, localTier any, hasRemote bool, shortfall shortfallFunc) OfflineReadiness {
	reporter, ok := localTier.(coldRangeReporter)
	if !ok {
		// A tier that cannot report residency and has no remote also has
		// nothing to evict to, so everything it holds is local. With a remote
		// it could hold evicted ranges it cannot tell us about.
		if !hasRemote {
			return OfflineReadiness{Known: true}
		}
		return OfflineReadiness{Reason: "local tier does not track remote-only ranges"}
	}

	// An unseeded tier holds no interval for ranges that live only on the
	// remote, so its index would report them as absent rather than cold and the
	// count would come back zero on the worst case this exists to catch.
	// Seeding only ever runs for a remote-backed share, so an unseeded tier is
	// a blind spot only when there is a remote it might not have caught up with.
	if hasRemote && !reporter.ColdSeeded() {
		return OfflineReadiness{Reason: "local tier has not been seeded from the manifest"}
	}

	// Ask the tier even with no remote configured. A share can hold cold ranges
	// and have no remote at once — unbinding a remote from a share that had
	// already evicted leaves the cold intervals in place, and the journal
	// replays them from its cold log on the next open. Those ranges are worse
	// than unsafe: reads reconcile on the cold flag whether or not a remote
	// exists, so there is nothing to fetch them from and they never serve. A
	// non-zero count is the honest report; treating "no remote" as "all local"
	// would call that share provably offline-safe.
	bytes, ranges, err := reporter.ColdExtents(ctx)
	if err != nil {
		return OfflineReadiness{Reason: "residency scan did not finish: " + err.Error()}
	}

	// That count is the index's account of itself, so it cannot see a range the
	// index has forgotten: no interval means no contribution, which is exactly
	// what an absent range looks like. The manifest outlives a lost interval, so
	// it is the only thing here that can tell those two apart.
	missing, missingRanges, err := shortfall(ctx, reporter)
	if err != nil {
		return OfflineReadiness{Reason: "manifest cross-check did not run: " + err.Error()}
	}
	if missing > 0 {
		return OfflineReadiness{Reason: fmt.Sprintf(
			"local index does not describe %d bytes in %d ranges the manifest places, "+
				"so whether they are remote-only or lost cannot be determined",
			missing, missingRanges)}
	}
	return OfflineReadiness{RemoteOnlyBytes: bytes, RemoteOnlyRanges: ranges, Known: true}
}

// manifestShortfall reports how much of what the share's manifest places the
// local tier's index does not describe at all.
//
// It is the outside opinion the readiness answer needs. The index is the thing
// being doubted, so its own tally cannot say whether it is complete; manifest
// rows are written by a different subsystem and survive a lost interval, so a
// row whose range no interval covers is evidence the index has forgotten
// something. Those bytes serve neither locally nor from the remote — nothing
// left knows where to fetch them from — which is why a shortfall is reported as
// indeterminate rather than folded into the remote-only count.
//
// Rows holding no committed bytes yet place nothing, and neither do rows whose
// ID carries no chunk offset: where those bytes belong is unknowable from here,
// and reporting them is CheckManifests' job. Everything else is unioned per
// file and weighed against the ranges the index reports for the same file,
// which include cold ones — a cold range is described, just not resident.
//
// A lost interval is not the only way to reach a shortfall. A server-side copy
// writes the destination's manifest rows and no interval for them at all, so a
// clone nobody has read back yet looks the same from here. That is not a false
// alarm: those bytes need the remote too, and the index cannot say which of the
// two it is looking at, which is why the answer is indeterminate rather than a
// remote-only count.
//
// ponytail: one ListFileChunks per payload, the same walk warm and the
// block-count stats take, which is why the caller memoizes the result rather
// than paying for it on every scrape.
func manifestShortfall(ctx context.Context, index coldRangeReporter, chunks manifestLister) (bytes int64, ranges int64, err error) {
	var payloads []string
	if err := chunks.EnumeratePayloads(ctx, func(id string) error {
		payloads = append(payloads, id)
		return nil
	}); err != nil {
		return 0, 0, err
	}
	for _, id := range payloads {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		rows, err := chunks.ListFileChunks(ctx, id)
		if err != nil {
			return 0, 0, err
		}
		placed, end := placedRanges(rows)
		if len(placed) == 0 {
			continue
		}
		described, err := index.DataExtents(ctx, id, end)
		if err != nil {
			return 0, 0, err
		}
		for _, gap := range subtractExtents(placed, described) {
			ranges++
			bytes += int64(gap[1] - gap[0])
		}
	}
	return bytes, ranges, nil
}

// placedRanges returns one payload's manifest coverage in canonical form, plus
// the offset its last placed byte ends at — the clamp DataExtents needs to
// report the index's view of the same span.
func placedRanges(rows []*block.FileChunk) ([][2]uint64, int64) {
	placed := make([][2]uint64, 0, len(rows))
	var end uint64
	for _, row := range rows {
		if row == nil || row.DataSize == 0 || row.Hash.IsZero() {
			continue
		}
		off, ok := block.ParseChunkOffset(row.ID)
		if !ok {
			continue
		}
		rowEnd := off + uint64(row.DataSize)
		if rowEnd <= off {
			continue // an end that wrapped describes no range
		}
		placed = append(placed, [2]uint64{off, rowEnd})
		if rowEnd > end {
			end = rowEnd
		}
	}
	if end > 1<<62 {
		end = 1 << 62 // keep a corrupt row from overflowing the int64 clamp
	}
	return coalesceExtents(placed), int64(end)
}

// subtractExtents returns the parts of a that no extent of b covers. Both must
// be sorted and non-overlapping, as coalesceExtents and DataExtents return
// them; the result is too.
//
// ponytail: rescans b from the front for every span of a. Both lists are one
// file's coalesced coverage, so a is usually a single span; carry a cursor
// across spans only if a profile ever shows this walk.
func subtractExtents(a, b [][2]uint64) [][2]uint64 {
	var out [][2]uint64
	for _, span := range a {
		cur := span[0]
		for _, cover := range b {
			if cover[1] <= cur {
				continue // already behind the part of span still uncovered
			}
			if cover[0] >= span[1] {
				break // b is sorted, so nothing later reaches back into span
			}
			if cover[0] > cur {
				out = append(out, [2]uint64{cur, cover[0]})
			}
			cur = cover[1]
		}
		if cur < span[1] {
			out = append(out, [2]uint64{cur, span[1]})
		}
	}
	return out
}

// shortfallInterval is how long a cross-check result stands before the walk is
// paid for again.
const shortfallInterval = time.Minute

// shortfallMemo caches the last manifest cross-check.
//
// The walk costs one metadata round-trip per file, and OfflineReadiness sits on
// the metrics scrape path, which is deliberately free of per-file DB walks (see
// MetricsBlockStats). Recomputing it per scrape would put that walk back.
//
// ponytail: a plain time window rather than invalidation. A shortfall is a
// durable structural condition — a range the index has forgotten stays
// forgotten until something repairs it — so a verdict a minute old is still the
// right verdict, and a fresh process starts with an empty memo. Wire it to an
// invalidation signal only if a caller ever needs the answer to move faster
// than that.
type shortfallMemo struct {
	mu     sync.Mutex
	at     time.Time
	bytes  int64
	ranges int64
}

func (m *shortfallMemo) get(ctx context.Context, index coldRangeReporter, chunks manifestLister) (int64, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	// A zero at is older than any interval, so an empty memo falls through.
	if time.Since(m.at) < shortfallInterval {
		return m.bytes, m.ranges, nil
	}
	bytes, ranges, err := manifestShortfall(ctx, index, chunks)
	if err != nil {
		// A walk that ran out of its caller's deadline says nothing about the
		// share, so there is nothing worth remembering: the next call gets to
		// try again on its own budget rather than inheriting this one's.
		return 0, 0, err
	}
	m.at, m.bytes, m.ranges = time.Now(), bytes, ranges
	return bytes, ranges, nil
}
