package kvstore_test

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
)

// What a change costs, in keys read and written: a rename touches its record
// and its name rows and nothing else.
func TestCosts(t *testing.T) {
	s, c := newStore(t)
	cp := s.ControlPlane()
	u := user("alice", uid("1000"))
	must(t, cp.PutUser(ctx, u))
	sh := share("photos", "/photos")
	must(t, cp.PutShare(ctx, sh))
	c.Take()

	u.Name = "alicia"
	must(t, cp.PutUser(ctx, u))
	// Read the record and the new name row; write both; delete the old row.
	if got, want := c.Take(), (kv.Counts{Gets: 2, Sets: 2, Deletes: 1}); got != want {
		t.Errorf("user rename: %+v, want %+v", got, want)
	}

	sh.Name = "pictures"
	must(t, cp.PutShare(ctx, sh))
	// Read the info and state records, the new name row and ShareList; write
	// the info, the name row and ShareList; delete the old name row. The path
	// did not change, so no path row is read.
	if got, want := c.Take(), (kv.Counts{Gets: 4, Sets: 3, Deletes: 1}); got != want {
		t.Errorf("share rename: %+v, want %+v", got, want)
	}

	must(t, cp.PutShare(ctx, sh))
	// The info and state records only.
	if got, want := c.Take(), (kv.Counts{Gets: 2}); got != want {
		t.Errorf("unchanged share put: %+v, want %+v", got, want)
	}
}
