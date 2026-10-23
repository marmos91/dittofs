package sequence

import "testing"

// A new owner accepts any number, and only a granted request gives it a slot:
// one refused leaves it new. A new owner of opens starts unconfirmed; a new
// owner of locks has nothing to confirm.
func TestNewOwner(t *testing.T) {
	b := Book{}
	opens, locks := Owner{Client: 1, Kind: Opens, Name: "o"}, Owner{Client: 1, Kind: Locks, Name: "l"}
	b.Answer(opens, 7, Refused)
	if !b.Check(opens, 3) || b.Unconfirmed(opens) {
		t.Fatal("after a refused first request the owner was not new")
	}
	b.Answer(opens, 3, Granted)
	if b.Check(opens, 3) || !b.Check(opens, 4) || !b.Unconfirmed(opens) {
		t.Error("a new owner of opens: want 4 next, unconfirmed")
	}
	b.Answer(locks, 9, Granted)
	if !b.Check(locks, 10) || b.Unconfirmed(locks) {
		t.Error("a new owner of locks: want 10 next, nothing to confirm")
	}
}

// The number after 2^32-1 is 0, whether the answer starts a slot or
// advances one, and a refusal advances a slot as a grant does.
func TestNextWraps(t *testing.T) {
	b := Book{}
	started, advanced := Owner{Client: 1, Kind: Locks, Name: "s"}, Owner{Client: 1, Kind: Locks, Name: "a"}
	b.Answer(started, ^uint32(0), Granted)
	b.Answer(advanced, ^uint32(0)-1, Granted)
	b.Answer(advanced, ^uint32(0), Refused)
	for _, o := range []Owner{started, advanced} {
		if !b.Check(o, 0) {
			t.Errorf("owner %q after 2^32-1 expects %d, want 0", o.Name, b[o].Next)
		}
	}
}
