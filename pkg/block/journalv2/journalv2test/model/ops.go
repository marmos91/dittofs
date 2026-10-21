package model

import (
	"cmp"
	"errors"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2"
)

// enter checks the part every appending call shares: the model is not between
// a crash and its resolution, and the file's share allows the call. It returns
// the file, nil when the model has never seen it, and whether the call took
// effect. A call refused with ErrOtherShare, where that refusal is allowed,
// returns a nil mismatch and proceed false.
//
// decision: a refusal with ErrOtherShare is required while the file holds an
// extent or a marker under another share's tag, and allowed but not required
// once it holds neither: the journal keeps a file's tag while any segment
// holds a record of it, which the caller cannot see, so a call after the last
// record went may claim the file afresh or be refused. Require the refusal for
// longer if the journal ever reports when a file's last record leaves.
func (m *Model) enter(op string, tag shareTag, id fileID, err error) (f *file, proceed bool, mis error) {
	if mis := m.known(op, id); mis != nil {
		return nil, false, mis
	}
	f = m.files[id]
	other := f != nil && f.tag != tag
	if errors.Is(err, journalv2.ErrOtherShare) {
		if !other {
			return nil, false, m.mismatch(op, id, "refused with ErrOtherShare, but no other share holds the file")
		}
		return f, false, nil
	}
	if other && err == nil && f.holdsAnything() {
		return nil, false, m.mismatch(op, id, "accepted through share %d a file share %d holds", tag, f.tag)
	}
	return f, err == nil, nil
}

// claim returns the file, created if new, as belonging to tag: a call that
// appends a record of a file with no tag claims it.
func (m *Model) claim(f *file, tag shareTag, id fileID) *file {
	if f == nil {
		f = &file{id: id}
		m.files[id] = f
	}
	f.tag = tag
	return f
}

// issue checks a version the journal assigned: it exceeds every version issued
// before it, every version on disk, a fill's included, and the floor of the
// last reopen.
func (m *Model) issue(op string, id fileID, v version) error {
	if bound := maxVersion(maxVersion(m.issued, m.onDisk()), m.floor); v.Compare(bound) <= 0 {
		return m.mismatch(op, id, "assigned version %v, not above %v, the highest issued, on disk or in the floor", v, bound)
	}
	m.issued = v
	m.maxSeen = maxVersion(m.maxSeen, v)
	// The first version issued after a crash bounds every record that crash
	// left uncertain: it exceeds every version on disk, so a record at or
	// above it did not survive. The bound outlives later crashes.
	for _, f := range m.files {
		for _, r := range f.recs {
			if r.uncertain && r.bound == (version{}) {
				r.bound = v
			}
		}
	}
	return nil
}

func unexpected(m *Model, op string, id fileID, err error) error {
	return m.mismatch(op, id, "returned %v, which the model has no rule for", err)
}

// WriteAt applies a write of p at off through share tag, to which the journal
// returned v and err.
//
// A zero-length write may be accepted or refused with ErrZeroLength; accepted,
// it holds nothing and is not counted as a change of the file.
func (m *Model) WriteAt(tag shareTag, id fileID, off int64, p []byte, mtime int64, v version, err error) error {
	const op = "write"
	f, proceed, mis := m.enter(op, tag, id, err)
	switch {
	case mis != nil:
		return mis
	case errors.Is(err, journalv2.ErrOtherShare):
		return nil
	case len(p) == 0 && errors.Is(err, journalv2.ErrZeroLength):
		return nil
	case !proceed:
		return unexpected(m, op, id, err)
	}
	if err := m.issue(op, id, v); err != nil {
		return err
	}
	if len(p) == 0 {
		return nil
	}
	f = m.claim(f, tag, id)
	m.append(f, &record{kind: kindWrite, ext: extent{Off: off, Len: int64(len(p))}, ver: v, data: slices.Clone(p), mtime: mtime})
	return nil
}

// Sync applies a Sync of ids through share tag, to which the journal returned
// err. Returned nil, every operation on those files that returned before it is
// durable. A failed sync window is outside the model, so any error is one it
// has no rule for.
func (m *Model) Sync(tag shareTag, err error, ids ...fileID) error {
	const op = "sync"
	for _, id := range ids {
		if mis := m.known(op, id); mis != nil {
			return mis
		}
	}
	if err != nil {
		return unexpected(m, op, fileID{}, err)
	}
	for _, id := range ids {
		f := m.files[id]
		if f == nil || f.tag != tag {
			continue
		}
		for _, r := range f.recs {
			r.durable = true
		}
	}
	return nil
}

// known returns why the model cannot take a call on file id now: it is
// between a crash and its reopen, or a crash left the file's state unknown
// until Compare finds it.
func (m *Model) known(op string, id fileID) error {
	if m.crashed {
		return m.mismatch(op, id, "called between Crash and Reopen")
	}
	if f := m.files[id]; f != nil && f.unresolved {
		return m.mismatch(op, id, "called on a file a crash left unresolved; Compare it first")
	}
	return nil
}

// Fill applies a fill of p at off at version v through share tag, made with
// the change version asOf a read returned, to which the journal returned err.
//
// The refusal is decided against the file's change version: newer than asOf,
// Fill must refuse with ErrStaleFill; asOf at or above every version issued,
// it must not; anywhere between, it may, because floor buckets are coarse and
// start at the highest version on disk after a reopen. Accepted, it holds the
// sub-extents absent at the call, offloaded, at version v.
func (m *Model) Fill(tag shareTag, id fileID, off int64, p []byte, asOf, v version, err error) error {
	const op = "fill"
	f, proceed, mis := m.enter(op, tag, id, err)
	switch {
	case mis != nil:
		return mis
	case errors.Is(err, journalv2.ErrOtherShare):
		return nil
	case len(p) == 0 && (err == nil || errors.Is(err, journalv2.ErrZeroLength)):
		return nil
	}

	st := m.current(id)
	mustRefuse := st.change.Compare(asOf) > 0
	mustAccept := asOf.Compare(maxVersion(m.maxSeen, m.floor)) >= 0
	switch {
	case errors.Is(err, journalv2.ErrStaleFill):
		if mustAccept {
			return m.mismatch(op, id, "refused as stale a fill as of %v, at or above every version issued", asOf)
		}
		return nil
	case !proceed:
		return unexpected(m, op, id, err)
	case mustRefuse:
		return m.mismatch(op, id, "accepted a fill as of %v of a file changed at %v", asOf, st.change)
	}

	if r := droppableAbove(f, extent{Off: off, Len: int64(len(p))}, v); r != nil {
		return m.mismatch(op, id, "fill at %v below a %s at %v over its bytes: repack may have dropped that record, which the model cannot see; fill at or above it", v, r.kind, r.ver)
	}

	f = m.claim(f, tag, id)
	for i := 0; i < len(p); {
		if _, held := st.held[off+int64(i)]; held {
			i++
			continue
		}
		j := i
		for j < len(p) {
			if _, held := st.held[off+int64(j)]; held {
				break
			}
			j++
		}
		m.append(f, &record{kind: kindFill, ext: extent{Off: off + int64(i), Len: int64(j - i)}, ver: v, data: slices.Clone(p[i:j])})
		m.maxSeen = maxVersion(m.maxSeen, v)
		i = j
	}
	return nil
}

// Truncate applies a truncate to size through share tag, to which the journal
// returned v and err. It stops holding every byte at or beyond size below v,
// and leaves a marker over [size, end of file).
func (m *Model) Truncate(tag shareTag, id fileID, size int64, v version, err error) error {
	return m.remove("truncate", kindTruncate, tag, id, extent{Off: size, Len: toEnd(size)}, v, err)
}

// Deallocate applies a deallocate of e through share tag, to which the journal
// returned v and err. A zero-length e must be refused with ErrZeroLength.
func (m *Model) Deallocate(tag shareTag, id fileID, e extent, v version, err error) error {
	return m.remove("deallocate", kindDeallocate, tag, id, e, v, err)
}

// Delete applies a delete through share tag, to which the journal returned v
// and err. It stops holding every byte of the file below v.
func (m *Model) Delete(tag shareTag, id fileID, v version, err error) error {
	return m.remove("delete", kindDelete, tag, id, extent{Off: 0, Len: toEnd(0)}, v, err)
}

// remove applies a removal. A removal is durable on return and never gated on
// the offloaded bit.
func (m *Model) remove(op string, kind recordKind, tag shareTag, id fileID, e extent, v version, err error) error {
	f, proceed, mis := m.enter(op, tag, id, err)
	zero := kind == kindDeallocate && e.Len == 0
	switch {
	case mis != nil:
		return mis
	case errors.Is(err, journalv2.ErrOtherShare):
		return nil
	case zero && errors.Is(err, journalv2.ErrZeroLength):
		return nil
	case !proceed:
		return unexpected(m, op, id, err)
	case zero:
		return m.mismatch(op, id, "accepted a zero-length extent")
	}
	if err := m.issue(op, id, v); err != nil {
		return err
	}
	f = m.claim(f, tag, id)
	m.append(f, &record{kind: kind, ext: e, ver: v, durable: true})
	return nil
}

// Settle applies the caller's statement that metadata has recorded every
// removal of file id at or below v: their markers are dropped. Settling is not
// persisted, so a crash brings the markers back. The journal's Settle returns
// nothing; an error here is the model's, for a file whose state it does not
// know yet.
func (m *Model) Settle(tag shareTag, id fileID, v version) error {
	if mis := m.known("settle", id); mis != nil {
		return mis
	}
	f := m.files[id]
	if f == nil || f.tag != tag {
		return nil
	}
	for _, r := range f.recs {
		if r.kind.removal() && r.ver.Compare(v) <= 0 {
			r.settled = true
		}
	}
	return nil
}

// Since returns what the journal's Since(id, from) must yield through share
// tag: every held extent, as a write, and every unsettled marker whose version
// is strictly above from, in version order. Held extents are split where the
// record they are read from changes; Compare checks Since byte by byte, so a
// journal may split them differently.
//
// The error is the model's, for a file whose state it does not know yet.
func (m *Model) Since(tag shareTag, id fileID, from version) ([]journalv2.Change, error) {
	if mis := m.known("since", id); mis != nil {
		return nil, mis
	}
	f := m.files[id]
	if f == nil || f.tag != tag {
		return nil, nil
	}
	st := derive(f, all)
	var out []journalv2.Change
	for _, r := range st.runs() {
		if r.ver.Compare(from) <= 0 {
			continue
		}
		var mtime int64
		if r.rec.kind == kindWrite {
			mtime = r.rec.mtime
		}
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Version == r.ver && last.Mtime == mtime && last.Extent.Off+last.Extent.Len == r.ext.Off {
				last.Extent.Len += r.ext.Len
				continue
			}
		}
		out = append(out, journalv2.Change{Kind: journalv2.ChangeWrite, Extent: r.ext, Version: r.ver, Mtime: mtime})
	}
	for _, r := range st.markers {
		if r.ver.Compare(from) > 0 {
			out = append(out, journalv2.Change{Kind: changeKind(r.kind), Extent: r.ext, Version: r.ver})
		}
	}
	slices.SortStableFunc(out, func(a, b journalv2.Change) int {
		if c := a.Version.Compare(b.Version); c != 0 {
			return c
		}
		return cmp.Compare(a.Extent.Off, b.Extent.Off)
	})
	return out, nil
}

func changeKind(k recordKind) journalv2.ChangeKind {
	switch k {
	case kindTruncate:
		return journalv2.ChangeTruncate
	case kindDeallocate:
		return journalv2.ChangeDeallocate
	case kindDelete:
		return journalv2.ChangeDelete
	}
	return journalv2.ChangeWrite
}

// droppableAbove returns a record over e with a version above v that the
// running journal no longer keeps in its index, nil when there is none: a
// release, which leaves no marker, or a removal whose marker was settled,
// including one whose marker did not come back after a crash.
//
// decision: a fill never carries a version below a release or a settled
// removal over its bytes. The running journal fills every sub-extent its
// index holds nothing for, so it holds such a fill; after a restart the
// record's precedence hides it again, and repack may have dropped the record,
// so either way it is undecided. A fill carries its ref's version, and the
// content a release made absent was offloaded at the released version, so a
// refill carries that version or a later one. Model the index apart from the
// records if a caller ever fills from a ref older than what it released.
func droppableAbove(f *file, e extent, v version) *record {
	if f == nil {
		return nil
	}
	for _, r := range f.recs {
		if _, ok := intersect(r.ext, e); !ok || r.ver.Compare(v) <= 0 {
			continue
		}
		if r.kind == kindRelease || r.kind.removal() && r.settled {
			return r
		}
	}
	return nil
}
