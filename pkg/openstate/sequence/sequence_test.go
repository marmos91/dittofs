package sequence

import "testing"

// A new owner accepts any number, and only a granted request gives it a slot:
// one refused leaves it new. A new owner of opens accepts only a confirm until
// confirmed; a new owner of locks has nothing to confirm.
func TestNewOwner(t *testing.T) {
	b := Book{}
	opens, locks := Owner{Client: 1, Kind: Opens, Name: "o"}, Owner{Client: 1, Kind: Locks, Name: "l"}
	b.Answer(opens, 7, Refused)
	if !b.Check(opens, 3, false) {
		t.Fatal("after a refused first request the owner was not new")
	}
	b.Answer(opens, 3, Granted)
	if b.Check(opens, 4, false) || !b.Check(opens, 4, true) {
		t.Error("a new owner of opens: want only a confirm numbered 4 accepted")
	}
	b.Answer(locks, 9, Granted)
	if !b.Check(locks, 10, false) {
		t.Error("a new owner of locks refused its next number")
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
		if !b.Check(o, 0, false) {
			t.Errorf("owner %q after 2^32-1 expects %d, want 0", o.Name, b[o].Next)
		}
	}
}
