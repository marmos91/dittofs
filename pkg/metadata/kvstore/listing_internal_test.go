package kvstore

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// A page never ends inside a collision chain: with pages of two, a chain of
// three digests comes back whole, and the next page starts after it.
func TestPageKeepsCollisionChains(t *testing.T) {
	old := entriesPage
	entriesPage = 2
	defer func() { entriesPage = old }()

	s := openStore(t)
	share := newShare(t, s)
	var root entity.FileID
	for sh, err := range s.ControlPlane().Shares(context.Background()) {
		if err != nil {
			t.Fatal(err)
		}
		root = sh.Root
	}
	entry := func(digest uint64, name string) [][]byte {
		e := entity.Entry{Parent: root, Name: []byte(name), Child: entity.NewFileID(), Type: entity.Regular}
		return [][]byte{entryKey(share, root, digest, []byte(name)), encodeEntry(e)}
	}
	var kvs [][]byte
	for _, e := range [][][]byte{entry(5, "a"), entry(9, "b"), entry(9, "c"), entry(9, "d"), entry(12, "e")} {
		kvs = append(kvs, e...)
	}
	rows(t, s, kvs...)

	admin := entity.Identity{Principal: entity.NewUser(), Privileged: true}
	rows(t, s, grantRow(share, admin.Principal, entity.AccessAdmin)...)
	h := entity.NewHandle(share, root)
	var pages [][]string
	after := uint64(0)
	for {
		var page []Listed
		if err := s.kv.View(context.Background(), func(r kv.Reader) (err error) {
			page, err = listPage(r, admin, h, after, false)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		var ns []string
		for _, l := range page {
			ns = append(ns, string(l.Entry.Name))
		}
		pages = append(pages, ns)
		if len(page) < entriesPage {
			break
		}
		after = page[len(page)-1].Cookie
	}
	want := [][]string{{"a", "b", "c", "d"}, {"e"}}
	if len(pages) != len(want) || len(pages[0]) != 4 || pages[1][0] != "e" {
		t.Fatalf("pages %v, want %v", pages, want)
	}
}
