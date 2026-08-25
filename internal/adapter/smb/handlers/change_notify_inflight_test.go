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
	done9 := r.MarkNotifyInFlight(fid, 1, 9)
	done10 := r.MarkNotifyInFlight(fid, 1, 10)

	if !r.HasEarlierInFlightNotify(fid, 1, 10) {
		t.Error("msgID 10 must see msgID 9 outstanding and decline the events")
	}
	if r.HasEarlierInFlightNotify(fid, 1, 9) {
		t.Error("msgID 9 is the earliest; it must take the events")
	}
	if r.HasEarlierInFlightNotify(other, 1, 10) {
		t.Error("a different handle must not be blocked by this one")
	}

	// Once 9 is answered, a later notify on the handle is free again.
	done9()
	if r.HasEarlierInFlightNotify(fid, 1, 10) {
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

	done100 := r.MarkNotifyInFlight(fid, 1, 100) // arrived first
	done50 := r.MarkNotifyInFlight(fid, 1, 50)   // arrived second, lower number

	if r.HasEarlierInFlightNotify(fid, 1, 100) {
		t.Error("100 arrived first; it must take the events despite the higher number")
	}
	if !r.HasEarlierInFlightNotify(fid, 1, 50) {
		t.Error("50 arrived second; it must decline despite the lower number")
	}
	done100()
	if r.HasEarlierInFlightNotify(fid, 1, 50) {
		t.Error("50 is now the earliest outstanding; it must take the events")
	}
	done50()
}

// Releasing the earliest must promote the SECOND arrival, not the last one.
// A swap-remove moves the tail element into slot 0, which would make the
// newest request look like the oldest.
func TestHasEarlierInFlightNotify_FirstReleasePromotesSecond(t *testing.T) {
	r := NewNotifyRegistry()
	fid := [16]byte{1}

	doneA := r.MarkNotifyInFlight(fid, 1, 10) // arrived 1st
	doneB := r.MarkNotifyInFlight(fid, 1, 20) // arrived 2nd
	doneC := r.MarkNotifyInFlight(fid, 1, 30) // arrived 3rd

	doneA() // earliest is answered

	if r.HasEarlierInFlightNotify(fid, 1, 20) {
		t.Error("20 arrived before 30, so it is now the earliest and must take the events")
	}
	if !r.HasEarlierInFlightNotify(fid, 1, 30) {
		t.Error("30 is the newest; 20 is still outstanding ahead of it")
	}
	doneB()
	doneC()
	if len(r.inFlightNotify) != 0 {
		t.Errorf("tracking leaked: %v", r.inFlightNotify)
	}
}

// SMB3 multichannel: one session spans several TCP connections and a FileID is
// valid on all of them, so two notifies on one handle can arrive on different
// connections carrying the SAME MessageID. Identity is (ConnID, MessageID).
func TestHasEarlierInFlightNotify_SameMessageIDDifferentConns(t *testing.T) {
	r := NewNotifyRegistry()
	fid := [16]byte{1}

	doneA := r.MarkNotifyInFlight(fid, 1, 10) // conn 1, arrived first
	doneB := r.MarkNotifyInFlight(fid, 2, 10) // conn 2, same number, arrived second

	if r.HasEarlierInFlightNotify(fid, 1, 10) {
		t.Error("conn 1's request arrived first; it must take the events")
	}
	if !r.HasEarlierInFlightNotify(fid, 2, 10) {
		t.Error("conn 2's request must decline: conn 1's identically-numbered request is ahead of it")
	}
	doneA()
	if r.HasEarlierInFlightNotify(fid, 2, 10) {
		t.Error("conn 1 released; conn 2 is now earliest and must take the events")
	}
	doneB()
	if len(r.inFlightNotify) != 0 {
		t.Errorf("release removed the wrong entry: %v", r.inFlightNotify)
	}
}
