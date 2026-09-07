package types

import "testing"

func v41Ctx() *CompoundContext {
	return &CompoundContext{MinorVersion: 1, MinorVersionAccepted: true}
}

func realStateid(seqid uint32, tag byte) *Stateid4 {
	sid := &Stateid4{Seqid: seqid}
	sid.Other[0] = tag
	return sid
}

func TestIsCurrentStateidPlaceholder(t *testing.T) {
	placeholder := &Stateid4{Seqid: 1}
	if !placeholder.IsCurrentStateidPlaceholder() {
		t.Error("(1, all-zeros) should be the current-stateid placeholder")
	}
	// The anonymous stateid is (0, all-zeros) and must not be mistaken for it.
	if (&Stateid4{}).IsCurrentStateidPlaceholder() {
		t.Error("the anonymous stateid is not the placeholder")
	}
	if realStateid(1, 0x01).IsCurrentStateidPlaceholder() {
		t.Error("a real stateid with seqid 1 is not the placeholder")
	}
}

func TestResolveStateid_NoCurrentStateidIsBadStateid(t *testing.T) {
	ctx := v41Ctx()
	if _, status := ctx.ResolveStateid(&Stateid4{Seqid: 1}); status != NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID (%d)", status, NFS4ERR_BAD_STATEID)
	}
}

func TestResolveStateid_SubstitutesCurrent(t *testing.T) {
	ctx := v41Ctx()
	open := realStateid(3, 0x01)
	ctx.SetCurrentStateid(open)

	got, status := ctx.ResolveStateid(&Stateid4{Seqid: 1})
	if status != NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", status)
	}
	if *got != *open {
		t.Errorf("resolved = %+v, want %+v", *got, *open)
	}

	// A filehandle operation drops it again.
	ctx.ClearCurrentStateid()
	if _, status := ctx.ResolveStateid(&Stateid4{Seqid: 1}); status != NFS4ERR_BAD_STATEID {
		t.Errorf("after clear: status = %d, want NFS4ERR_BAD_STATEID", status)
	}
}

func TestResolveStateid_V40LeavesPlaceholderAlone(t *testing.T) {
	ctx := &CompoundContext{MinorVersion: 0, MinorVersionAccepted: true}
	placeholder := &Stateid4{Seqid: 1}

	got, status := ctx.ResolveStateid(placeholder)
	if status != NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", status)
	}
	if got != placeholder {
		t.Error("a v4.0 COMPOUND has no current stateid; (1, 0) is an ordinary stateid there")
	}
}

func TestSaveRestoreCurrentStateid(t *testing.T) {
	ctx := v41Ctx()
	open := realStateid(3, 0x01)
	ctx.SetCurrentStateid(open)

	ctx.SaveCurrentStateid() // SAVEFH
	ctx.ClearCurrentStateid()
	ctx.RestoreCurrentStateid() // RESTOREFH

	got, status := ctx.ResolveStateid(&Stateid4{Seqid: 1})
	if status != NFS4_OK {
		t.Fatalf("status = %d, want NFS4_OK", status)
	}
	if *got != *open {
		t.Errorf("restored = %+v, want %+v", *got, *open)
	}
}

func TestSaveCurrentStateid_CarriesAbsence(t *testing.T) {
	ctx := v41Ctx()
	ctx.SaveCurrentStateid() // SAVEFH with nothing current
	ctx.SetCurrentStateid(realStateid(3, 0x01))
	ctx.RestoreCurrentStateid()

	if _, status := ctx.ResolveStateid(&Stateid4{Seqid: 1}); status != NFS4ERR_BAD_STATEID {
		t.Errorf("status = %d, want NFS4ERR_BAD_STATEID: RESTOREFH must bring back the absence SAVEFH saved", status)
	}
}

func TestClearsCurrentStateid(t *testing.T) {
	for _, op := range []uint32{OP_PUTFH, OP_PUTROOTFH, OP_PUTPUBFH, OP_LOOKUP, OP_LOOKUPP, OP_CREATE, OP_OPENATTR} {
		if !ClearsCurrentStateid(op) {
			t.Errorf("%s sets the current filehandle without returning a stateid; it must drop the current stateid", OpName(op))
		}
	}
	// RESTOREFH restores the saved stateid rather than dropping one, and the
	// operations that merely consume a stateid leave it in place.
	for _, op := range []uint32{OP_RESTOREFH, OP_SAVEFH, OP_READ, OP_WRITE, OP_SETATTR, OP_CLOSE, OP_LOCK, OP_LOCKU} {
		if ClearsCurrentStateid(op) {
			t.Errorf("%s must not drop the current stateid", OpName(op))
		}
	}
}
