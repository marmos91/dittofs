package grantfsm

import (
	"errors"
	"testing"
)

const (
	rh  = Read | Handle
	rw  = Read | Write
	rwh = Read | Write | Handle
)

// Write or handle caching without read is no kind; Within falls to none
// rather than keep one.
func TestWithin(t *testing.T) {
	for _, c := range []struct{ k, allowed, want Kind }{
		{rwh, rh, rh},
		{rwh, rw, rw},
		{rwh, Write | Handle, None},
		{rh, Handle, None},
		{Read, rwh, Read},
	} {
		if got := Within(c.k, c.allowed); got != c.want {
			t.Errorf("Within(%b, %b) = %b, want %b", c.k, c.allowed, got, c.want)
		}
	}
}

// Write caching is held alone; read caching beside any read caching.
func TestCompatible(t *testing.T) {
	for _, c := range []struct {
		a, b Kind
		want bool
	}{
		{rh, Read, true},
		{rw, Read, false},
		{Read, rwh, false},
		{rwh, None, true},
	} {
		if got := Compatible(c.a, c.b); got != c.want {
			t.Errorf("Compatible(%b, %b) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// A conflict met while a recall is in flight starts none, and narrows what
// the recall must reach; each acknowledgement short of it starts the next
// stage, keeping read caching while write or handle caching goes, and every
// stage raises the epoch.
func TestProgressiveBreak(t *testing.T) {
	g := New(rwh, true)
	if !g.Break(rh) || g.Recall.To != rh || g.Epoch != 2 {
		t.Fatalf("first break: %+v, recall %+v; want a recall to RH at epoch 2", g, g.Recall)
	}
	if g.Break(None) || g.Recall.To != rh || g.Epoch != 2 {
		t.Fatalf("a break in flight: %+v, recall %+v; want none started, the stage kept", g, g.Recall)
	}
	for _, stage := range []struct {
		ack, next Kind
		epoch     uint16
	}{{rh, Read, 3}, {Read, None, 4}} {
		next, err := g.Ack(stage.ack, g.Epoch)
		if err != nil || !next || g.Kind != stage.ack || g.Recall.To != stage.next || g.Epoch != stage.epoch {
			t.Fatalf("ack to %b: next %v, %v, %+v, recall %+v; want a stage to %b at epoch %d", stage.ack, next, err, g, g.Recall, stage.next, stage.epoch)
		}
	}
	if next, err := g.Ack(None, g.Epoch); err != nil || next || g.Recall != nil || g.Kind != None {
		t.Fatalf("the last ack: next %v, %v, %+v", next, err, g)
	}
}

// An acknowledgement names at most what its recall offered, or with none in
// flight what the grant holds, and one naming an older epoch changes
// nothing.
func TestAck(t *testing.T) {
	g := New(rwh, true)
	g.Break(rh)
	if _, err := g.Ack(rwh, g.Epoch); !errors.Is(err, ErrNotHeld) {
		t.Errorf("ack past the recall's target: %v", err)
	}
	if _, err := g.Ack(Read, g.Epoch-1); err != nil || g.Kind != rwh || g.Recall == nil {
		t.Errorf("ack of an older epoch: %v, %+v; want nothing changed", err, g)
	}
	g.Ack(rh, g.Epoch)
	if _, err := g.Ack(rw, g.Epoch); !errors.Is(err, ErrNotHeld) {
		t.Errorf("unasked ack to caching not held: %v", err)
	}
	if _, err := g.Ack(Read, 0); err != nil || g.Kind != Read {
		t.Errorf("unasked narrowing: %v, %+v", err, g)
	}
}

// A recall that times out cuts the grant to none; a late acknowledgement of
// it changes nothing, and is accepted only where it took handle caching
// alone.
func TestLateAck(t *testing.T) {
	for _, c := range []struct {
		allowed Kind
		ok      bool
	}{{rw, true}, {rh, false}} {
		g := New(rwh, true)
		g.Break(c.allowed)
		g.Timeout()
		_, err := g.Ack(c.allowed, g.Epoch)
		if g.Kind != None || (err == nil) != c.ok {
			t.Errorf("late ack of a recall to %b: %v, %+v; want accepted %v and the grant left at none", c.allowed, err, g, c.ok)
		}
	}
}

// A later open widens a grant only to a valid strict superset within what is
// allowed, never during a recall, and raises the epoch.
func TestUpgrade(t *testing.T) {
	g := New(Read, true)
	if g.Upgrade(rwh, rh) || g.Upgrade(Read, rwh) {
		t.Fatalf("an upgrade past what is allowed, or to the same kind: %+v", g)
	}
	if !g.Upgrade(rh, rwh) || g.Kind != rh || g.Epoch != 2 {
		t.Fatalf("upgrade to RH: %+v", g)
	}
	g.Break(Read)
	if g.Upgrade(rwh, rwh) {
		t.Fatalf("an upgrade during a recall: %+v", g)
	}
}
