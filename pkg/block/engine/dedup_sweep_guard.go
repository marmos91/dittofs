package engine

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// numDedupGuardStripes must be a power of two so stripeFor can mask.
const numDedupGuardStripes = 64

// dedupSweepGuard orders the only two decisions in the engine that can destroy
// the last copy of a chunk's bytes:
//
//   - the carve dedup oracle deciding a hash is already remote-durable, after
//     which the carver drops the plaintext and writes a manifest row alone;
//   - the remote sweep deciding a hash is a globally-dead orphan, after which
//     the reclaimer decrements the enclosing block and frees its object.
//
// Neither decision appears in the other's inputs. A dedup hit refreshes no
// synced marker, so the sweep's grace gate cannot see it, and its manifest row
// lands after the mark phase has already snapshotted the live set, so the
// live-set gate cannot see it either. Left unordered, an oracle that answers
// "durable" for a hash the sweep is about to reclaim produces a file whose
// chunk points at bytes that no longer exist.
//
// The guard makes the two mutually exclusive per hash: an adoption blocks a
// claim and a claim blocks an adoption, and the loser of the race takes the
// safe branch — the carver uploads the bytes it would have deduped, or the
// sweep keeps the marker for the next pass.
//
// An adoption is held by age, not released explicitly when its manifest row
// commits: two carves can adopt the same hash, so a release would need a
// refcount, and a carve that dies between the oracle and its block commit would
// strand its hold and make the hash unreclaimable for the life of the process.
//
// ponytail: two ceilings, both from that age bound. It holds only within one
// process, which is all the GC supports today — the run lock in gc.go is
// process-local for the same reason. And a carve whose dedup-to-commit gap
// exceeds dedupAdoptionMaxAge drops out from under its own adoption while its
// row is still unwritten, which the mark phase would then miss. Both close by
// recording the adoption on the synced marker itself and deleting that marker
// conditionally, which is a new store method across all four backends plus its
// storetest conformance; do that when GC runs outside the writing process, or
// when a carve pass can credibly stall for an hour mid-run.
type dedupSweepGuard struct {
	stripes [numDedupGuardStripes]dedupGuardStripe
}

type dedupGuardStripe struct {
	mu sync.Mutex
	// adopted holds the time each in-flight dedup adoption was taken. An
	// entry survives until it ages past dedupAdoptionMaxAge.
	adopted map[block.ContentHash]time.Time
	// claimed holds the hashes the sweep is currently reclaiming. Entries
	// are short-lived: one reclamation each.
	claimed map[block.ContentHash]struct{}
	// sinceSweep counts adoptions recorded since this stripe was last
	// pruned. Every dedupPruneInterval adoptions the stripe sheds its aged
	// entries, which bounds the map against ongoing dedup traffic even with
	// the GC scheduler switched off. Traffic that stops leaves its final
	// entries in place until it resumes or a sweep runs — a residue of under
	// one interval per stripe.
	sinceSweep int
}

// dedupAdoptionMaxAge is how long an adoption protects its hash. It only has to
// outlive the carve's block commit, which follows the oracle by seconds; an
// hour matches the default grace period and leaves the margin wide.
const dedupAdoptionMaxAge = time.Hour

// dedupPruneInterval is how many adoptions a stripe records before it sheds
// aged ones itself. Large enough that the O(n) walk is amortized away, small
// enough that the map tracks recent dedup traffic rather than all of it.
const dedupPruneInterval = 512

// dedupGuard is the process-wide instance. The carve oracle is constructed per
// share while the sweep runs per remote store, so the two only meet at package
// scope — the same reason the GC run lock lives here.
var dedupGuard dedupSweepGuard

func (g *dedupSweepGuard) stripeFor(h block.ContentHash) *dedupGuardStripe {
	// The hash is already uniformly distributed; one byte of it is as good a
	// stripe index as any derived mix.
	return &g.stripes[uint(h[1])&(numDedupGuardStripes-1)]
}

// adopt runs probe under h's stripe and records an adoption when probe reports
// the bytes are already remote-durable. It answers false without probing while
// the sweep holds a claim on h, so a carver that loses the race uploads the
// chunk instead of pointing at bytes about to be freed.
//
// Probing under the stripe is what orders the two decisions: the sweep cannot
// claim h between the probe and the adoption being recorded.
func (g *dedupSweepGuard) adopt(h block.ContentHash, probe func() (bool, error)) (bool, error) {
	s := g.stripeFor(h)
	s.mu.Lock()
	defer s.mu.Unlock()

	if _, sweeping := s.claimed[h]; sweeping {
		return false, nil
	}
	durable, err := probe()
	if err != nil || !durable {
		return durable, err
	}
	now := time.Now()
	if s.adopted == nil {
		s.adopted = make(map[block.ContentHash]time.Time)
	}
	s.adopted[h] = now
	s.sinceSweep++
	if s.sinceSweep >= dedupPruneInterval {
		s.pruneLocked(now.Add(-dedupAdoptionMaxAge))
	}
	return true, nil
}

// pruneLocked drops adoptions older than cutoff. Callers hold s.mu.
func (s *dedupGuardStripe) pruneLocked(cutoff time.Time) {
	for h, at := range s.adopted {
		if at.Before(cutoff) {
			delete(s.adopted, h)
		}
	}
	s.sinceSweep = 0
}

// claim reserves h for reclamation, locking the dedup oracle out until
// releaseClaim. It refuses while a recent adoption still holds h: that carve's
// manifest row may not have landed before the mark phase read its share, so the
// hash cannot be proven dead. Callers that get true MUST pair it with
// releaseClaim.
//
// The age bound is dedupAdoptionMaxAge rather than the run's grace period on
// purpose. Grace answers a different question — how long a freshly-committed
// hash is spared — and an operator may legitimately set it to zero, which would
// leave every adoption claimable the moment it is recorded.
func (g *dedupSweepGuard) claim(h block.ContentHash) bool {
	s := g.stripeFor(h)
	s.mu.Lock()
	defer s.mu.Unlock()

	if at, adopted := s.adopted[h]; adopted && time.Since(at) <= dedupAdoptionMaxAge {
		return false
	}
	if s.claimed == nil {
		s.claimed = make(map[block.ContentHash]struct{})
	}
	s.claimed[h] = struct{}{}
	return true
}

// releaseClaim ends a reclamation. Once the marker is gone the oracle answers
// "not durable" on its own; if the reclamation failed the marker survives and
// the hash becomes adoptable again, which is the correct fail-open direction.
func (g *dedupSweepGuard) releaseClaim(h block.ContentHash) {
	s := g.stripeFor(h)
	s.mu.Lock()
	delete(s.claimed, h)
	s.mu.Unlock()
}

// pruneAdoptions drops adoptions past dedupAdoptionMaxAge. Anything that old
// has either committed its manifest row — where the mark phase takes over — or
// belongs to a carve that died before committing one.
func (g *dedupSweepGuard) pruneAdoptions() {
	cutoff := time.Now().Add(-dedupAdoptionMaxAge)
	for i := range g.stripes {
		s := &g.stripes[i]
		s.mu.Lock()
		s.pruneLocked(cutoff)
		s.mu.Unlock()
	}
}
