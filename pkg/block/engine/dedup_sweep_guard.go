package engine

import (
	"sync"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
)

// numDedupGuardStripes must be a power of two so stripeFor can mask.
const numDedupGuardStripes = 64

// Compile-time guard: the mask below only yields a valid index when the count
// is a power of two. If it is not, the unsigned subtraction underflows and the
// build fails.
const _ = uint(-(numDedupGuardStripes & (numDedupGuardStripes - 1)))

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
// An adoption is held by age, not by an explicit release on commit: a carve
// that fails between the oracle and its block commit would otherwise strand a
// hold and make the hash permanently unreclaimable. The sweep supplies its own
// grace cutoff as the age bound, so an adoption protects a hash for exactly as
// long as the grace window already protects a freshly-synced one.
//
// ponytail: process-local, so it orders a sweep only against carves in the
// same process — which is all the GC itself supports today (the run lock in
// gc.go is process-local for the same reason). Cross-process safety needs the
// adoption recorded on the synced marker itself, which means a store-level
// conditional delete across all four backends; do that only when GC actually
// runs outside the writing process.
type dedupSweepGuard struct {
	stripes [numDedupGuardStripes]dedupGuardStripe
}

type dedupGuardStripe struct {
	mu sync.Mutex
	// adopted holds the time each in-flight dedup adoption was taken. An
	// entry survives until the sweep's grace cutoff passes it.
	adopted map[block.ContentHash]time.Time
	// claimed holds the hashes the sweep is currently reclaiming. Entries
	// are short-lived: one reclamation each.
	claimed map[block.ContentHash]struct{}
}

// dedupGuard is the process-wide instance. The carve oracle is constructed per
// share while the sweep runs per remote store, so the two only meet at package
// scope — the same reason the GC run lock lives here.
var dedupGuard dedupSweepGuard

func (g *dedupSweepGuard) stripeFor(h block.ContentHash) *dedupGuardStripe {
	// The hash is already uniformly distributed; its first two bytes are as
	// good a stripe index as any derived mix.
	return &g.stripes[(uint(h[0])<<8|uint(h[1]))&(numDedupGuardStripes-1)]
}

// adopt runs probe under h's stripe and records an adoption when probe reports
// the bytes are already remote-durable. It answers false without probing while
// the sweep holds a claim on h, so a carver that loses the race uploads the
// chunk instead of pointing at bytes about to be freed.
//
// Probing under the stripe is what orders the two decisions: the sweep cannot
// claim h between the probe and the adoption being recorded.
func (g *dedupSweepGuard) adopt(h block.ContentHash, now time.Time, probe func() (bool, error)) (bool, error) {
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
	if s.adopted == nil {
		s.adopted = make(map[block.ContentHash]time.Time)
	}
	s.adopted[h] = now
	return true, nil
}

// claim reserves h for reclamation, locking the dedup oracle out until
// releaseClaim. It refuses when an adoption taken at or after notBefore still
// holds h: that carve's manifest row may not have landed before the mark phase
// read its share, so the hash cannot be proven dead. Callers that get true MUST
// pair it with releaseClaim.
func (g *dedupSweepGuard) claim(h block.ContentHash, notBefore time.Time) bool {
	s := g.stripeFor(h)
	s.mu.Lock()
	defer s.mu.Unlock()

	if at, adopted := s.adopted[h]; adopted && !at.Before(notBefore) {
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

// pruneAdoptions drops adoptions older than cutoff. Anything that old has
// either committed its manifest row — where the mark phase takes over — or
// belongs to a carve that died before committing one.
func (g *dedupSweepGuard) pruneAdoptions(cutoff time.Time) {
	for i := range g.stripes {
		s := &g.stripes[i]
		s.mu.Lock()
		for h, at := range s.adopted {
			if at.Before(cutoff) {
				delete(s.adopted, h)
			}
		}
		s.mu.Unlock()
	}
}
