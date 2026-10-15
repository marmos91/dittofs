package kvstoretest

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// FS is one share in a fresh store, with an administrator who may do
// anything and a handle on the share's root: a starting point for another
// package's tests.
type FS struct {
	Store  *kvstore.Store
	Counts *kv.Counting // what each operation read and wrote; nil on a held store
	Share  entity.Share
	Admin  entity.Identity
	Root   entity.Handle
}

// NewFS makes an FS over a fresh store from open, with names compared as
// given.
func NewFS(t *testing.T, open Opener) *FS { return newFS(t, open, entity.FoldIdentity).export() }

// Held holds a store's next Update between its snapshot and its commit,
// which makes a race between two operations deterministic.
type Held struct{ h *holding }

// NewHeldFS makes an FS over a held store from open.
func NewHeldFS(t *testing.T, open Opener) (*FS, Held) {
	f, h := newHeldFS(t, backend{name: "kv", open: open}, entity.FoldIdentity)
	return f.export(), Held{h}
}

// Race runs outer with inner run to completion inside outer's first attempt,
// after its reads and writes and before its commit, and returns both
// results: inner commits first, in the order where outer must conflict.
//
// The hold catches outer's first Update, whatever it is. A create first
// reserves a range of file numbers in an Update of its own, under a lock
// inner would then wait on, so make one create on the share before racing
// two.
func (h Held) Race(outer, inner func() error) (outerErr, innerErr error) {
	return race(h.h, outer, inner)
}

// Create makes name in dir as id, failing the test on an error.
func (f *FS) Create(t *testing.T, id entity.Identity, dir entity.Handle, name string, nf kvstore.NewFile) (entity.Handle, entity.File) {
	t.Helper()
	h, file, err := f.Store.Namespace().Create(ctx, id, dir, []byte(name), nf)
	must(t, err)
	return h, file
}

func (f *fs) export() *FS {
	return &FS{Store: f.s, Counts: f.c, Share: f.share, Admin: f.admin, Root: f.root}
}
