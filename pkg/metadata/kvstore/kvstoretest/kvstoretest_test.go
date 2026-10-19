package kvstoretest_test

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore/kvstoretest"
)

// The suite through its own exported surface, against the embedded backend
// in memory: every case it holds runs and passes on a conforming store.
func memory(tb testing.TB) kv.KV {
	s, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() {
		if err := s.Close(); err != nil {
			tb.Error(err)
		}
	})
	return s
}

// The fixture as another package sees it: a share to create in, and a held
// store on which a race between two creates of one name commits the first
// and refuses the second.
func TestExportedFixture(t *testing.T) {
	f := kvstoretest.NewFS(t, memory)
	h, made := f.Create(t, f.Admin, f.Root, "docs", kvstore.Dir(0o755))
	if made.Type != entity.Directory {
		t.Fatalf("created %+v", made)
	}
	if _, got, err := f.Store.Namespace().Lookup(context.Background(), f.Admin, f.Root, []byte("docs")); err != nil || got.ID != made.ID {
		t.Fatalf("lookup %+v, %v", got, err)
	}
	f.Create(t, f.Admin, h, "README", kvstore.Regular(0o644))

	held, hold := kvstoretest.NewHeldFS(t, memory)
	held.Create(t, held.Admin, held.Root, "first", kvstore.Regular(0o644)) // reserves the share's numbers
	create := func() error {
		_, _, err := held.Store.Namespace().Create(context.Background(), held.Admin, held.Root, []byte("x"), kvstore.Regular(0o644))
		return err
	}
	outer, inner := hold.Race(create, create)
	if inner != nil || outer == nil {
		t.Errorf("two creates of one name: inner %v, outer %v", inner, outer)
	}
}
