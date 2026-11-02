package segment

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// TestSeedReplay replays the fuzz target's seed corpus, the same table
// FuzzSet registers from, against the real Set and asserts that each seed
// reaches the successful retires, recycles, takes and seals it is documented
// for, with the oracle agreeing after every op. A decoder or seed change
// that silently loses a seed's purpose — every sequence that only names the
// null id, or a spare no take precedes, exercises nothing but refusals —
// fails here, in plain go test, before any fuzz run does.
func TestSeedReplay(t *testing.T) {
	prof.Watch(t, prof.Options{})

	segCap := int64(4096)

	for _, sd := range corpus {
		t.Run(sd.name, func(t *testing.T) {
			s := NewSet(segCap, nil)
			o := newOracle(segCap)
			ops := decode(sd.ops, segCap)
			if len(ops) == 0 {
				t.Fatalf("%s: the seed decodes to no ops", sd.name)
			}
			var retires, recycles, takes, seals, drops int
			for _, op := range ops {
				want := o.apply(op)
				var got error
				switch op.kind {
				case opAddSpare:
					got = s.AddSpare(op.id)
				case opTake:
					got = s.Take(op.id)
					if got == nil {
						takes++
					}
				case opSeal:
					got = s.Seal(op.id, op.full)
					if got == nil {
						seals++
					}
				case opEnd:
					got = s.End(op.id)
				case opShrink:
					got = s.Shrink(op.id, op.n)
				case opAddRecord:
					got = s.AddRecord(op.id, op.tag, op.n)
				case opClaim:
					if s.Claim(op.id) != (want == nil) {
						t.Fatalf("%s: op %d (%d): the oracle holds the claim %v, the set does not", sd.name, op.kind, op.id, want == nil)
					}
					got = want
				case opRelease:
					s.Release(op.id)
				case opRetire:
					_, got = s.Retire(op.id)
					if got == nil {
						retires++
					}
				case opRecycle:
					got = s.Recycle(op.id, op.fresh)
					if got == nil {
						recycles++
					}
				case opDropSpare:
					_, got = s.DropSpare(op.id)
					if got == nil {
						drops++
					}
				}
				if !sameErrClass(got, want) {
					t.Fatalf("%s: op %d (%d): the set returned %v, the oracle %v", sd.name, op.kind, op.id, got, want)
				}
				if err := s.Check(); err != nil {
					t.Fatalf("%s: op %d (%d): %v", sd.name, op.kind, op.id, err)
				}
			}
			if retires != sd.retires {
				t.Errorf("%s: %d retires succeeded, want %d: the seed no longer reaches the retire its comment names", sd.name, retires, sd.retires)
			}
			if recycles != sd.recycles {
				t.Errorf("%s: %d recycles succeeded, want %d", sd.name, recycles, sd.recycles)
			}
			if takes != sd.takes {
				t.Errorf("%s: %d takes succeeded, want %d", sd.name, takes, sd.takes)
			}
			if seals != sd.seals {
				t.Errorf("%s: %d seals succeeded, want %d", sd.name, seals, sd.seals)
			}
			if drops != sd.drops {
				t.Errorf("%s: %d spares dropped, want %d", sd.name, drops, sd.drops)
			}
		})
	}
}
