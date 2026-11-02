package index

import (
	"cmp"
	"iter"
	"slices"

	"github.com/marmos91/dittofs/pkg/block/journalv2/internal/core"
)

// EntryKind is what a read-out entry records about its extent. It takes the
// same values as the public EntryKind, so the reader wiring the journal's
// inspection maps it one to one.
type EntryKind uint8

const (
	EntryHeld     EntryKind = iota + 1 // a held piece: a read serves it
	EntryRetained                      // superseded content kept, never served
	EntryMarker                        // an unsettled removal marker
)

// Entry is one entry of a file's placement index as the read-out reports it:
// a copy, which the caller may keep and change.
type Entry struct {
	Kind    EntryKind
	Extent  core.Extent // a marker running to the end of the file has Len = MaxInt64 - Off
	Version core.Version
	// Tag is the file's owner, not a per-record tag: the index keeps one tag
	// per file and refuses a second, so every entry of a file has its owner's.
	// A rivalled file reports its owner.
	//
	// decision: the public entry's Tag names the share whose record backs the
	// entry, and for a file with records under two tags — a rival the rebuild
	// keeps on purpose, since no rule decides which tag's records are read —
	// the two disagree: the index keeps no tag per piece, so the read-out
	// reports the owner on every entry, including the ones the rival's
	// records back. Overturned by a tag kept per piece, or by recovery
	// parking a rival's records out of the read-out until one tag is
	// forgotten, or by the public doc narrowed to the file's owner; the
	// reader wiring the journal's inspection must not map Tag one to one
	// for a rivalled file.
	Tag core.ShareTag // the file's owner; zero when the file has none
	// Record is held and retained: the record holding the bytes; zero for a
	// marker.
	Record RecordAt
	// Skip is held and retained: the payload offset of Extent.Off.
	Skip uint32
	// Offloaded is held and retained: the entry's offloaded bit.
	Offloaded bool
	// By is retained: the version of the record that superseded it.
	By core.Version
	// Removal is marker: the removal that left it; zero otherwise.
	Removal core.ChangeKind
	// Clone is a clone target's marker: a copy of its spec, which the caller
	// may keep; nil otherwise.
	Clone *core.CloneSpec
}

// appendEntries appends one file's every entry to out: each held piece, each
// retained piece and each marker, as a copy, and returns it. The caller holds
// the file's lock, read or write, which every field it reads is guarded by.
//
// The copy changes no index state. The only shared write it can make is a
// cost test's comparison counter, which is nil outside those tests.
func (f *file) appendEntries(out []Entry) []Entry {
	// The buffer is grown once, to the entry count, so appending allocates
	// nothing past it and a file of many extents holds the read lock for one
	// copy rather than for a series of regrowths.
	out = slices.Grow(out, int(f.entries()))
	tag := f.tag
	f.held.visit(whole, func(p Piece) bool {
		out = append(out, Entry{
			Kind:      EntryHeld,
			Extent:    p.Extent,
			Version:   p.Version,
			Tag:       tag,
			Record:    p.Record,
			Skip:      p.Skip,
			Offloaded: p.Offloaded,
		})
		return true
	})
	for _, r := range f.retained.pieces {
		out = append(out, Entry{
			Kind:      EntryRetained,
			Extent:    r.Extent,
			Version:   r.Version,
			Tag:       tag,
			Record:    r.Record,
			Skip:      r.Skip,
			Offloaded: r.Offloaded,
			By:        r.By,
		})
	}
	for _, m := range f.markers {
		e := Entry{
			Kind:    EntryMarker,
			Extent:  m.extent,
			Version: m.version,
			Tag:     tag,
			Removal: m.kind,
		}
		if m.clone != nil {
			spec := *m.clone
			e.Clone = &spec
		}
		out = append(out, e)
	}
	return out
}

// sortEntries orders one file's entries into read-out order: by offset, the
// stable sort keeping appendEntries' kind order — held, then retained, then
// markers — at an equal offset, and within a kind the storage order the loops
// appended in: compareRetained's for retained pieces, version order for
// markers. The caller holds no lock by then, as every read-out's sort runs
// after its view released the file's.
func sortEntries(es []Entry) {
	slices.SortStableFunc(es, func(a, b Entry) int {
		return cmp.Compare(a.Extent.Off, b.Extent.Off)
	})
}

// inspect returns id's entries, in read-out order. A file with no entry, or
// with a tag alone, yields nothing and creates nothing: the read-out claims
// no file and mutates nothing.
//
// It is the inspection read-out's per-file side: the caller maps the entries
// as the journal's inspection does.
//
// ponytail: the read-out copies a file's entries under its read lock, O(n),
// so a writer of that file waits for the copy; a file of 10^6 extents holds
// it for tens of milliseconds. Upgrade to a read-out in offset-bounded
// chunks, each consistent on its own, when an inspection pass shows in a
// write-latency profile of the file it reads.
func (fs *files) inspect(id core.FileID) []Entry {
	var out []Entry
	fs.view(id, func(f *file) { out = f.appendEntries(out) })
	sortEntries(out)
	return out
}

// compareFileIDs orders two FileIDs by their bytes.
func compareFileIDs(a, b core.FileID) int {
	x, y := a.Bytes(), b.Bytes()
	return slices.Compare(x[:], y[:])
}

// inspectAll yields every file's entries, one file at a time, in FileID
// order. Its contract is the public All's: it mutates nothing and stops
// nothing, each file's slice is the caller's, and a file holding no entry is
// skipped.
//
// decision: the bulk read-out is not a snapshot of the index. Each file is
// read out consistently under its own lock, one after another, and writers of
// every file proceed throughout: nothing stops the store. A file created
// after the FileIDs were collected is not yielded, and one whose entries are
// gone by its turn is skipped, so a file created or removed during the
// iteration may or may not appear. Overturned only if a caller needs a
// store-wide point in time, which would need a lock spanning the store.
//
// ponytail: the FileIDs are collected and sorted before any file is read,
// O(F) memory and O(F log F) time for F files, so the order is the FileIDs'
// and no lock spans two files. Upgrade to a merge of per-shard sorted walks
// when a journal of many millions of files shows the slice in a profile.
func (fs *files) inspectAll() iter.Seq2[core.FileID, []Entry] {
	return func(yield func(core.FileID, []Entry) bool) {
		var ids []core.FileID
		for i := range fs.shards {
			s := &fs.shards[i]
			s.mu.Lock()
			ids = slices.Grow(ids, len(s.m))
			for id := range s.m {
				ids = append(ids, id)
			}
			s.mu.Unlock()
		}
		slices.SortFunc(ids, compareFileIDs)
		for _, id := range ids {
			es := fs.inspect(id)
			if len(es) == 0 {
				continue
			}
			if !yield(id, es) {
				return
			}
		}
	}
}
