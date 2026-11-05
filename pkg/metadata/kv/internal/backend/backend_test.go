package backend

import (
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/kv"
)

// failed is a cursor whose read failed before its first record.
type failed struct{ err error }

func (c failed) Valid() bool            { return false }
func (c failed) Key() []byte            { return nil }
func (c failed) Value() ([]byte, error) { return nil, c.err }
func (c failed) Next()                  {}
func (c failed) Err() error             { return c.err }

// A failed read of the snapshot reaches the caller, from Scan and Before
// alike, with or without own writes to merge: it is never read as the end of
// the records.
func TestOverlayPassesOnAFailedRead(t *testing.T) {
	boom := errors.New("read failed")
	for name, o := range map[string]Overlay{"no own writes": nil, "own writes": {"a/1": {Value: []byte("x")}}} {
		var got error
		o.Scan([]byte("a/"), nil, failed{boom}, func(_ kv.KeyValue, err error) bool { got = err; return err == nil })
		if !errors.Is(got, boom) {
			t.Errorf("%s: Scan gave %v, want the read's error", name, got)
		}
		if _, _, err := o.Before([]byte("a/"), []byte("a/9"), failed{boom}); !errors.Is(err, boom) {
			t.Errorf("%s: Before gave %v, want the read's error", name, err)
		}
	}
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

// A clock that has not ticked between two calls, as a coarse one does not
// within its tick, still gives each a later Now.
func TestNowAdvancesOnAStillClock(t *testing.T) {
	c := &clock{time.Unix(1_800_000_000, 0)}
	s := NewClock(c.now, time.Time{})
	if err := s.Raise(func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	first := s.Now()
	if second := s.Now(); !second.After(first) {
		t.Fatalf("Now went from %v to %v with the clock still", first, second)
	}
}

// A raise whose write fails leaves the ceiling where it was, so Now holds at
// the last durable ceiling however far the clock moves.
func TestNowHoldsWhenARaiseFails(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	c := &clock{start}
	s := NewClock(c.now, time.Time{})
	if err := s.Raise(func([]byte) error { return nil }); err != nil {
		t.Fatal(err)
	}
	c.t = start.Add(time.Hour)
	if err := s.Raise(func([]byte) error { return errors.New("write failed") }); err == nil {
		t.Fatal("a failed write raised no error")
	}
	if now, ceiling := s.Now(), start.Add(CeilingAhead); now.After(ceiling) {
		t.Fatalf("Now %v passed the durable ceiling %v after a failed raise", now, ceiling)
	}
}
