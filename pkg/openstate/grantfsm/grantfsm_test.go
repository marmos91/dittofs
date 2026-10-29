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
	g := New(rwh, ptr(0))
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
		next, err := g.Ack(stage.ack)
		if err != nil || !next || g.Kind != stage.ack || g.Recall.To != stage.next || g.Epoch != stage.epoch {
			t.Fatalf("ack to %b: next %v, %v, %+v, recall %+v; want a stage to %b at epoch %d", stage.ack, next, err, g, g.Recall, stage.next, stage.epoch)
		}
	}
	if next, err := g.Ack(None); err != nil || next || g.Recall != nil || g.Kind != None {
		t.Fatalf("the last ack: next %v, %v, %+v", next, err, g)
	}
}

// An acknowledgement names at most what its recall's stage offered, so a
// late answer to an earlier stage is refused, or with none in flight what the
// grant holds.
func TestAck(t *testing.T) {
	g := New(rwh, ptr(0))
	g.Break(rh)
	g.Break(None)
	if _, err := g.Ack(rwh); !errors.Is(err, ErrNotHeld) {
		t.Errorf("ack past the recall's target: %v", err)
	}
	if next, err := g.Ack(rh); err != nil || !next || g.Recall.To != Read {
		t.Fatalf("ack to RH: next %v, %v, recall %+v; want a stage to R", next, err, g.Recall)
	}
	if _, err := g.Ack(rh); !errors.Is(err, ErrNotHeld) || g.Kind != rh || g.Recall.To != Read {
		t.Errorf("the first stage's answer again: %v, %+v; want ErrNotHeld, nothing changed", err, g)
	}
	g.Ack(Read)
	g.Ack(None)
	g = New(rh, nil)
	if _, err := g.Ack(rw); !errors.Is(err, ErrNotHeld) {
		t.Errorf("unasked ack to caching not held: %v", err)
	}
	if _, err := g.Ack(Read); err != nil || g.Kind != Read {
		t.Errorf("unasked narrowing: %v, %+v", err, g)
	}
}

// A recall that times out cuts the grant to none and raises no epoch: the cut
// is sent to no one.
func TestTimeout(t *testing.T) {
	g := New(rwh, ptr(0))
	g.Break(rh)
	e := g.Epoch
	g.Timeout()
	if g.Kind != None || g.Recall != nil || g.Epoch != e {
		t.Errorf("after a timeout: %+v; want none, no recall, epoch %d", g, e)
	}
}

// A later open widens a grant only to a valid strict superset within what is
// allowed, never during a recall, and raises the epoch.
func TestUpgrade(t *testing.T) {
	g := New(Read, ptr(0))
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

func ptr(v uint16) *uint16 { return &v }

// A v2 grant counts from its request's epoch, raised for the grant itself,
// and wraps through zero; any other is not counted.
func TestEpoch(t *testing.T) {
	if g := New(rh, ptr(0x4711)); !g.V2 || g.Epoch != 0x4712 {
		t.Errorf("a v2 grant from 0x4711: %+v, want epoch 0x4712", g)
	}
	g := New(rwh, ptr(0xffff))
	if g.Break(rh); g.Epoch != 1 {
		t.Errorf("a break past 0xffff: epoch %d, want 1, through zero", g.Epoch)
	}
	g = New(rwh, nil)
	if g.Break(rh); g.V2 || g.Epoch != 0 {
		t.Errorf("a v1 grant's break: %+v, want no epoch", g)
	}
}
