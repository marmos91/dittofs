package handlers

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// TestEncodeSetClientIDError_ClidInUseCarriesClientUsing pins the shape of the
// SETCLIENTID4res union. Its NFS4ERR_CLID_INUSE arm carries a clientaddr4 that
// no other arm has, so a status-only reply for that status is two XDR strings
// short and the client's decode runs off the end of the whole COMPOUND instead
// of reading an error it could act on.
func TestEncodeSetClientIDError_ClidInUseCarriesClientUsing(t *testing.T) {
	encoded := encodeSetClientIDError(types.NFS4ERR_CLID_INUSE)

	reader := bytes.NewReader(encoded)
	status, err := xdr.DecodeUint32(reader)
	if err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status != types.NFS4ERR_CLID_INUSE {
		t.Fatalf("status = %d, want %d", status, types.NFS4ERR_CLID_INUSE)
	}
	if _, err := xdr.DecodeString(reader); err != nil {
		t.Fatalf("decode client_using.na_r_netid: %v", err)
	}
	if _, err := xdr.DecodeString(reader); err != nil {
		t.Fatalf("decode client_using.na_r_addr: %v", err)
	}
	if reader.Len() != 0 {
		t.Errorf("%d trailing bytes after client_using", reader.Len())
	}
}

// Every other status takes the void arm, so anything beyond the status would
// be read as the next operation in the COMPOUND.
func TestEncodeSetClientIDError_OtherStatusesAreStatusOnly(t *testing.T) {
	for _, status := range []uint32{types.NFS4ERR_BADXDR, types.NFS4ERR_STALE_CLIENTID, types.NFS4ERR_INVAL} {
		if got, want := len(encodeSetClientIDError(status)), 4; got != want {
			t.Errorf("encodeSetClientIDError(%d) is %d bytes, want %d", status, got, want)
		}
	}
}
