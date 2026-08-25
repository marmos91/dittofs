package handlers

import "testing"

// The double-notify hazard: two CHANGE_NOTIFYs read in order 9,10 on one
// handle, dispatched in order 10,9. Request 10 must decline the buffered
// events so request 9 — the one the client is blocked on — can take them.
func TestHasEarlierInFlightNotify(t *testing.T) {
	r := NewNotifyRegistry()
	fid := [16]byte{1}
	other := [16]byte{2}

	// Read loop records both, in wire order, before either is dispatched.
	done9 := r.MarkNotifyInFlight(fid, 9)
	done10 := r.MarkNotifyInFlight(fid, 10)

	if !r.HasEarlierInFlightNotify(fid, 10) {
		t.Error("msgID 10 must see msgID 9 outstanding and decline the events")
	}
	if r.HasEarlierInFlightNotify(fid, 9) {
		t.Error("msgID 9 is the earliest; it must take the events")
	}
	if r.HasEarlierInFlightNotify(other, 10) {
		t.Error("a different handle must not be blocked by this one")
	}

	// Once 9 is answered, a later notify on the handle is free again.
	done9()
	if r.HasEarlierInFlightNotify(fid, 10) {
		t.Error("msgID 9 is done; msgID 10 must no longer decline")
	}
	done10()
	if len(r.inFlightNotify) != 0 {
		t.Errorf("tracking leaked: %v", r.inFlightNotify)
	}
}

// MessageIDs may be consumed out of order within the credit sequence window,
// so 100 can arrive before 50. Arrival order decides, not the number.
func TestHasEarlierInFlightNotify_OutOfOrderMessageIDs(t *testing.T) {
	r := NewNotifyRegistry()
	fid := [16]byte{1}

	done100 := r.MarkNotifyInFlight(fid, 100) // arrived first
	done50 := r.MarkNotifyInFlight(fid, 50)   // arrived second, lower number

	if r.HasEarlierInFlightNotify(fid, 100) {
		t.Error("100 arrived first; it must take the events despite the higher number")
	}
	if !r.HasEarlierInFlightNotify(fid, 50) {
		t.Error("50 arrived second; it must decline despite the lower number")
	}
	done100()
	if r.HasEarlierInFlightNotify(fid, 50) {
		t.Error("50 is now the earliest outstanding; it must take the events")
	}
	done50()
}
