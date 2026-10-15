package kvstore

import (
	"bytes"
	"cmp"
	"context"
	"maps"
	"slices"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/codec"
	"github.com/marmos91/dittofs/pkg/metadata/kv/keys"
)

// usageDelta is one transaction's change to a share's usage: the share's,
// each principal's and each project's, and the share's history bytes and
// refs still at an old namespace generation, which are reported, never
// charged. It is written under a key of its own, so it reads nothing and
// conflicts with nothing; the shard's fold adds it to the totals.
type usageDelta struct {
	Share      entity.Usage
	History    int64
	OldRefs    int64
	Principals map[entity.Principal]entity.Usage
	Projects   map[uint64]entity.Usage
}

// charge adds bytes and files to the share and to everyone f is charged
// to: its owner, its group when that is another principal, its project.
func (d *usageDelta) charge(f entity.File, bytes, files int64) {
	add := func(u entity.Usage) entity.Usage { return entity.Usage{Bytes: u.Bytes + bytes, Files: u.Files + files} }
	if d.Principals == nil {
		d.Principals, d.Projects = map[entity.Principal]entity.Usage{}, map[uint64]entity.Usage{}
	}
	d.Share = add(d.Share)
	d.Principals[f.Owner] = add(d.Principals[f.Owner])
	if f.Group != (entity.Principal{}) && f.Group != f.Owner {
		d.Principals[f.Group] = add(d.Principals[f.Group])
	}
	if f.Project != 0 {
		d.Projects[f.Project] = add(d.Projects[f.Project])
	}
}

// usageChange is the delta from a file's old record to its new one: a nil
// old is a create, a nil new a release, and a change of charge, owner,
// group or project moves exactly what the file was charged.
func usageChange(old, new *entity.File) usageDelta {
	var d usageDelta
	if old != nil {
		d.charge(*old, -old.Charged, -1)
	}
	if new != nil {
		d.charge(*new, new.Charged, 1)
	}
	return d
}

func (d usageDelta) empty() bool {
	zero := func(u entity.Usage) bool { return u == entity.Usage{} }
	return zero(d.Share) && d.History == 0 && d.OldRefs == 0 &&
		!slices.ContainsFunc(slices.Collect(maps.Values(d.Principals)), func(u entity.Usage) bool { return !zero(u) }) &&
		!slices.ContainsFunc(slices.Collect(maps.Values(d.Projects)), func(u entity.Usage) bool { return !zero(u) })
}

func usageDeltaKey(share entity.ShareID, shard uint64, unique [32]byte) []byte {
	return append(usageDeltasPrefix(share, shard), unique[:]...)
}

func usageDeltasPrefix(share entity.ShareID, shard uint64) []byte {
	return keys.Uint64(shareKey(share, keys.SUd), shard)
}

// writeUsage writes d as a delta of the shard, unless it changes nothing.
func writeUsage(tx kv.Txn, share entity.ShareID, shard uint64, d usageDelta) error {
	if d.empty() {
		return nil
	}
	return tx.Set(usageDeltaKey(share, shard, nextUnique()), encodeUsageDelta(d))
}

// chargeFile writes the usage delta from old to new for the file's shard.
func chargeFile(tx kv.Txn, share entity.ShareID, old, new *entity.File) error {
	shard := uint64(0)
	if new != nil {
		shard = new.Shard
	} else if old != nil {
		shard = old.Shard
	}
	return writeUsage(tx, share, shard, usageChange(old, new))
}

func putUsage(w *codec.Writer, u entity.Usage) *codec.Writer {
	return w.Int64(u.Bytes).Int64(u.Files)
}

func readUsage(r *codec.Reader) entity.Usage { return entity.Usage{Bytes: r.Int64(), Files: r.Int64()} }

// encodeUsageDelta writes the parties that changed in a fixed order, so a
// delta encodes the same way every time.
func encodeUsageDelta(d usageDelta) []byte {
	w := putUsage(codec.NewWriter(v1), d.Share).Int64(d.History).Int64(d.OldRefs)
	ps := slices.SortedFunc(maps.Keys(d.Principals), func(a, b entity.Principal) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), slices.Compare(a.ID[:], b.ID[:]))
	})
	ps = slices.DeleteFunc(ps, func(p entity.Principal) bool { return d.Principals[p] == entity.Usage{} })
	w.Len(len(ps))
	for _, p := range ps {
		putUsage(putPrincipal(w, p), d.Principals[p])
	}
	js := slices.Sorted(maps.Keys(d.Projects))
	js = slices.DeleteFunc(js, func(j uint64) bool { return d.Projects[j] == entity.Usage{} })
	w.Len(len(js))
	for _, j := range js {
		putUsage(w.Uint64(j), d.Projects[j])
	}
	return w.Encode()
}

func decodeUsageDelta(b []byte) (usageDelta, error) {
	r, err := open(b)
	if err != nil {
		return usageDelta{}, err
	}
	d := usageDelta{Share: readUsage(r), History: r.Int64(), OldRefs: r.Int64(),
		Principals: map[entity.Principal]entity.Usage{}, Projects: map[uint64]entity.Usage{}}
	for range r.Len() {
		p := readPrincipal(r)
		d.Principals[p] = readUsage(r)
	}
	for range r.Len() {
		j := r.Uint64()
		d.Projects[j] = readUsage(r)
	}
	return d, r.Done()
}

// Folded usage, per shard: only the shard's fold writes these.
func shareTotalKey(share entity.ShareID, shard uint64) []byte {
	return keys.Uint64(shareKey(share, keys.SUt), shard)
}

func principalTotalsPrefix(share entity.ShareID, p entity.PrincipalID) []byte {
	return keys.ID(shareKey(share, keys.SPu), p[:])
}

func projectTotalsPrefix(share entity.ShareID, project uint64) []byte {
	return keys.Uint64(shareKey(share, keys.SPj), project)
}

func shardKey(shard uint64) []byte { return keys.Uint64([]byte{keys.KindSH}, shard) }

// foldPartyKeys bounds the totals one delta touches: the share, an owner
// and a group on each side of a chown, and a project on each side.
const foldPartyKeys = 7

// FoldUsage adds a batch of the shard's usage deltas to its totals and
// deletes them, in one transaction within the key budget, and reports
// whether deltas are left. Only the shard's primary runs it. A delta
// written while it runs is not in its scan and is folded by a later batch,
// never twice, since the batch deletes exactly the deltas it added.
//
// ponytail: the guard on the shard record is meant to make a former
// primary's fold conflict with the takeover that rewrites the record, but
// nothing writes shard records yet, so on one node it guards nothing. It
// protects once a takeover writes SH‖shard.
func (c Capacity) FoldUsage(ctx context.Context, share entity.ShareID, shard uint64) (more bool, err error) {
	n := max(1, c.s.k/(1+foldPartyKeys))
	err = c.s.kv.Update(ctx, func(tx kv.Txn) error {
		more = false
		if err := tx.Guard(shardKey(shard)); err != nil {
			return err
		}
		var sum usageDelta
		var folded [][]byte
		for row, err := range tx.Scan(usageDeltasPrefix(share, shard), nil) {
			if err != nil {
				return err
			}
			if len(folded) == n {
				more = true
				break
			}
			d, err := decodeUsageDelta(row.Value)
			if err != nil {
				return err
			}
			sum.add(d)
			folded = append(folded, bytes.Clone(row.Key))
		}
		if len(folded) == 0 {
			return nil
		}
		for _, k := range folded {
			if err := tx.Delete(k); err != nil {
				return err
			}
		}
		return addTotals(tx, share, shard, sum)
	})
	return more, err
}

// add sums o into d.
func (d *usageDelta) add(o usageDelta) {
	plus := func(a, b entity.Usage) entity.Usage {
		return entity.Usage{Bytes: a.Bytes + b.Bytes, Files: a.Files + b.Files}
	}
	if d.Principals == nil {
		d.Principals, d.Projects = map[entity.Principal]entity.Usage{}, map[uint64]entity.Usage{}
	}
	d.Share = plus(d.Share, o.Share)
	d.History += o.History
	d.OldRefs += o.OldRefs
	for p, u := range o.Principals {
		d.Principals[p] = plus(d.Principals[p], u)
	}
	for j, u := range o.Projects {
		d.Projects[j] = plus(d.Projects[j], u)
	}
}

// addTotals adds sum to the shard's totals, reading each total it changes.
func addTotals(tx kv.Txn, share entity.ShareID, shard uint64, sum usageDelta) error {
	key := shareTotalKey(share, shard)
	t, err := readShareTotal(tx, key)
	if err != nil {
		return err
	}
	t.add(usageDelta{Share: sum.Share, History: sum.History, OldRefs: sum.OldRefs})
	if err := tx.Set(key, encodeShareTotal(t)); err != nil {
		return err
	}
	for p, u := range sum.Principals {
		if err := addTotal(tx, keys.Uint64(principalTotalsPrefix(share, p.ID), shard), u, &p); err != nil {
			return err
		}
	}
	for j, u := range sum.Projects {
		if err := addTotal(tx, keys.Uint64(projectTotalsPrefix(share, j), shard), u, nil); err != nil {
			return err
		}
	}
	return nil
}

func addTotal(tx kv.Txn, key []byte, u entity.Usage, p *entity.Principal) error {
	if (u == entity.Usage{}) {
		return nil
	}
	var t entity.Usage
	if b, ok, err := get(tx, key); err != nil {
		return err
	} else if ok {
		if _, t, err = decodeTotal(b); err != nil {
			return err
		}
	}
	t.Bytes, t.Files = t.Bytes+u.Bytes, t.Files+u.Files
	var kind entity.PrincipalKind
	if p != nil {
		kind = p.Kind
	}
	return tx.Set(key, encodeTotal(kind, t))
}

func readShareTotal(r kv.Reader, key []byte) (usageDelta, error) {
	b, ok, err := get(r, key)
	if err != nil || !ok {
		return usageDelta{}, err
	}
	return decodeShareTotal(b)
}

func encodeShareTotal(t usageDelta) []byte {
	return putUsage(codec.NewWriter(v1), t.Share).Int64(t.History).Int64(t.OldRefs).Encode()
}

func decodeShareTotal(b []byte) (usageDelta, error) {
	r, err := open(b)
	if err != nil {
		return usageDelta{}, err
	}
	t := usageDelta{Share: readUsage(r), History: r.Int64(), OldRefs: r.Int64()}
	return t, r.Done()
}

// A principal's or project's total; a principal's carries its kind, which
// its key, the principal's ID alone, does not.
func encodeTotal(kind entity.PrincipalKind, u entity.Usage) []byte {
	return putUsage(codec.NewWriter(v1).Uint8(uint8(kind)), u).Encode()
}

func decodeTotal(b []byte) (entity.PrincipalKind, entity.Usage, error) {
	r, err := open(b)
	if err != nil {
		return 0, entity.Usage{}, err
	}
	kind := entity.PrincipalKind(r.Uint8())
	u := readUsage(r)
	return kind, u, r.Done()
}

// Capacity is the filesystem service's view of what a share holds.
type Capacity struct{ s *Store }

func (s *Store) Capacity() Capacity { return Capacity{s} }

// Usage is a share's usage: its totals over every shard plus the deltas
// not yet folded.
//
// ponytail: a usage read scans one total per shard of the share, O(shards).
// Add a share-wide total folded from the per-shard ones on a slow period
// when a share with many shards shows that scan in a statfs profile.
func (c Capacity) Usage(ctx context.Context, share entity.ShareID) (entity.Usage, error) {
	d, err := c.s.usageOf(ctx, share)
	return d.Share, err
}

// PrincipalUsage is one principal's usage in a share, what rquota and an
// SMB per-user quota report.
func (c Capacity) PrincipalUsage(ctx context.Context, share entity.ShareID, p entity.Principal) (entity.Usage, error) {
	d, err := c.s.usageOf(ctx, share)
	return d.Principals[p], err
}

// usageOf sums every total and unfolded delta of a share.
//
// ponytail: a principal's read sums every principal's totals with the
// rest, O(principals with usage in the share). Read the one principal's
// prefix and the deltas alone when quota checks show it.
func (s *Store) usageOf(ctx context.Context, share entity.ShareID) (out usageDelta, err error) {
	err = s.kv.View(ctx, func(r kv.Reader) (err error) {
		out, err = usageIn(r, share)
		return err
	})
	return out, err
}

// usageIn is usageOf within one transaction.
func usageIn(r kv.Reader, share entity.ShareID) (out usageDelta, err error) {
	out = usageDelta{Principals: map[entity.Principal]entity.Usage{}, Projects: map[uint64]entity.Usage{}}
	err = func() error {
		for row, err := range r.Scan(shareKey(share, keys.SUt), nil) {
			if err != nil {
				return err
			}
			t, err := decodeShareTotal(row.Value)
			if err != nil {
				return err
			}
			out.add(t)
		}
		pu := shareKey(share, keys.SPu)
		for row, err := range r.Scan(pu, nil) {
			if err != nil {
				return err
			}
			kind, u, err := decodeTotal(row.Value)
			if err != nil {
				return err
			}
			p := entity.Principal{Kind: kind, ID: entity.PrincipalID(row.Key[len(pu) : len(pu)+keys.IDLen])}
			out.add(usageDelta{Principals: map[entity.Principal]entity.Usage{p: u}})
		}
		pj := shareKey(share, keys.SPj)
		for row, err := range r.Scan(pj, nil) {
			if err != nil {
				return err
			}
			j, _, err := keys.ReadUint64(row.Key[len(pj):])
			if err != nil {
				return err
			}
			_, u, err := decodeTotal(row.Value)
			if err != nil {
				return err
			}
			out.add(usageDelta{Projects: map[uint64]entity.Usage{j: u}})
		}
		for row, err := range r.Scan(shareKey(share, keys.SUd), nil) {
			if err != nil {
				return err
			}
			d, err := decodeUsageDelta(row.Value)
			if err != nil {
				return err
			}
			out.add(d)
		}
		return nil
	}()
	return out, err
}
