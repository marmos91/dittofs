package engine

// OfflineReadiness reports whether a share could keep serving reads with its
// remote store unreachable, and by how much it falls short.
//
// The question has one honest answer per share: every byte the share holds is
// either resident in the local tier or it is not. A range that is not — one
// written, mirrored to the remote and since evicted locally — needs the remote
// to serve, so a share with any such range would fail reads during an outage.
// Zero remote-only bytes is a provable offline-safe share.
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
// ranges are remote-only, and whether it has been told what the manifest holds
// yet. The journal-backed tier satisfies it; the in-memory tier does not, and
// asserting for it here keeps that from becoming another method every fake has
// to implement.
type coldRangeReporter interface {
	ColdExtents() (bytes int64, extents int64)
	ColdSeeded() bool
}

// OfflineReadiness measures how much of this share's data is remote-only.
//
// ponytail: an O(live ranges) in-memory scan of the local tier's index on
// every call, holding one shard's lock at a time. Keeping a running counter
// instead would make it O(1), but the cold flag is set and cleared across
// insert, split, clamp, evict, hydrate and repack, and a counter that drifts
// on any one of those reports a share offline-safe when it is not. Take the
// scan until a metrics scrape actually shows up in a latency profile.
func (bs *Store) OfflineReadiness() OfflineReadiness {
	bs.closeMu.RLock()
	defer bs.closeMu.RUnlock()
	if bs.closed {
		return OfflineReadiness{Reason: "block store is closed"}
	}
	return offlineReadinessOf(bs.local, bs.HasRemoteStore())
}

// offlineReadinessOf holds the gating rules, separated from the store lookup so
// they can be exercised directly. Each of them refuses to answer rather than
// answering zero, because a zero here reads as "provably offline safe" and
// would say that about exactly the shares whose data is most likely to be
// remote-only.
func offlineReadinessOf(localTier any, hasRemote bool) OfflineReadiness {
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
	bytes, ranges := reporter.ColdExtents()
	return OfflineReadiness{RemoteOnlyBytes: bytes, RemoteOnlyRanges: ranges, Known: true}
}
