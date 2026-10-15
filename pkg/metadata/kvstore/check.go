package kvstore

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// Check verifies, in one read transaction, the invariants the store keeps
// between records rather than within one, and returns every one that does
// not hold, joined:
//
//   - every chunk's refcount equals its reverse keys and the refs naming it,
//     and a block is live exactly while a referenced chunk record names it;
//   - the version index holds one entry per file at its stored Version, one
//     per directory time delta and one per released file not yet pruned,
//     and nothing else;
//   - each share's usage, folded totals and unfolded deltas, equals what its
//     files' Charged and count say, for the share, every principal and
//     every project.
//
// Tests end with it, and so can a crash-recovery check.
//
// ponytail: one transaction over the whole store, its maps in memory. Page
// it per share and namespace when a store outgrows a test or a repair tool's
// patience.
func (s *Store) Check(ctx context.Context) error {
	var errs []error
	report := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	err := s.kv.View(ctx, func(r kv.Reader) error {
		type chunkAt struct {
			ns entity.NamespaceID
			h  entity.ChunkHash
		}
		refs := map[chunkAt]int64{}
		nss := map[entity.ShareID]shareNamespaces{}
		floor := map[entity.ShareID]map[string]bool{}
		files := map[entity.ShareID]*usageDelta{}
		for row, err := range r.Scan([]byte{keys.KindF}, nil) {
			if err != nil {
				return err
			}
			k := row.Key[1:]
			if len(k) < 2*keys.IDLen {
				return fmt.Errorf("%w: file key %x", errMalformed, row.Key)
			}
			share, id, sub := entity.ShareID(k[:keys.IDLen]), entity.FileID(k[keys.IDLen:2*keys.IDLen]), k[2*keys.IDLen:]
			if floor[share] == nil {
				floor[share], files[share] = map[string]bool{}, &usageDelta{}
			}
			switch {
			case len(sub) == 0:
				f, err := decodeFile(share, id, row.Value)
				if err != nil {
					return err
				}
				floor[share][string(floorKey(share, id, f.Version))] = true
				files[share].charge(f, f.Charged, 1)
			case sub[0] == keys.FT:
				d, err := decodeDelta(row.Value)
				if err != nil {
					return err
				}
				floor[share][string(deltaFloorKey(share, id, d.Version, [32]byte(sub[1:])))] = true
			case sub[0] == keys.FRm:
				v, _, err := keys.ReadVersion(sub[1:])
				if err != nil {
					return err
				}
				if m, err := decodeRemoval(v, row.Value); err == nil && m.Kind == entity.RemoveRelease {
					floor[share][string(floorKey(share, id, m.FileVersion))] = true
				}
			case len(sub) == 9 && sub[0] == keys.FRef:
				c, err := decodeRef(id, 0, row.Value)
				if err != nil {
					return err
				}
				if c.IsZeroRef() {
					continue
				}
				n, ok := nss[share]
				if !ok {
					if n, err = shareNamespacesOf(r, share); err != nil {
						return err
					}
					nss[share] = n
				}
				ns, err := n.namespace(c.NSGen)
				if err != nil {
					return err
				}
				refs[chunkAt{ns, c.Chunk}]++
			}
		}

		live := map[entity.NamespaceID]map[entity.BlockName]int64{}
		for row, err := range r.Scan([]byte{keys.KindC}, nil) {
			if err != nil {
				return err
			}
			k := row.Key[1:]
			ns, h := entity.NamespaceID(k[:keys.IDLen]), entity.ChunkHash(k[keys.IDLen:])
			rec, err := decodeChunk(h, row.Value)
			if err != nil {
				return err
			}
			n, err := countReverse(r, ns, h)
			if err != nil {
				return err
			}
			if want := refs[chunkAt{ns, h}]; rec.Refcount != n || rec.Refcount != want {
				report("chunk %x in %x: refcount %d, reverse keys %d, refs %d", h, ns, rec.Refcount, n, want)
			}
			if rec.Refcount > 0 {
				if live[ns] == nil {
					live[ns] = map[entity.BlockName]int64{}
				}
				live[ns][rec.Block]++
			}
		}
		for row, err := range r.Scan([]byte{keys.KindB}, nil) {
			if err != nil {
				return err
			}
			k := row.Key[1:]
			ns, name := entity.NamespaceID(k[:keys.IDLen]), entity.BlockName(k[keys.IDLen:])
			blk, err := decodeBlock(name, row.Value)
			if err != nil {
				return err
			}
			if blk.Live != live[ns][name] {
				report("block %x in %x: live %d, referenced chunk records %d", name, ns, blk.Live, live[ns][name])
			}
			if (blk.Live == 0) != (blk.State != entity.BlockLive) {
				report("block %x in %x: live %d in state %v", name, ns, blk.Live, blk.State)
			}
		}

		for share, want := range floor {
			got := 0
			for row, err := range r.Scan(shareKey(share, keys.SVf), nil) {
				if err != nil {
					return err
				}
				got++
				if !want[string(row.Key)] {
					report("share %s: stray version-index entry %x", share, row.Key)
				}
			}
			if got != len(want) {
				report("share %s: %d version-index entries, want %d", share, got, len(want))
			}
		}

		for share, want := range files {
			got, err := usageIn(r, share)
			if err != nil {
				return err
			}
			if got.Share != want.Share {
				report("share %s: usage %+v, files say %+v", share, got.Share, want.Share)
			}
			if g, w := nonzero(got.Principals), nonzero(want.Principals); !maps.Equal(g, w) {
				report("share %s: principal usage %v, files say %v", share, g, w)
			}
			if g, w := nonzero(got.Projects), nonzero(want.Projects); !maps.Equal(g, w) {
				report("share %s: project usage %v, files say %v", share, g, w)
			}
		}
		return nil
	})
	return errors.Join(append(errs, err)...)
}

// nonzero is m without its zero entries, which a fold may leave.
func nonzero[K comparable](m map[K]entity.Usage) map[K]entity.Usage {
	out := maps.Clone(m)
	maps.DeleteFunc(out, func(_ K, u entity.Usage) bool { return u == entity.Usage{} })
	return out
}
