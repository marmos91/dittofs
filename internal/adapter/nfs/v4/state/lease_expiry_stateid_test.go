package state

import (
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// TestExpiredLease_StateidsReportExpired pins what a client is told when it
// comes back after its lease was cancelled and uses a stateid the server freed
// on the way out.
//
// RFC 7530 Section 9.6.3.2: "When a lease is canceled, all locking state
// associated with it is freed, and the use of any of the associated stateids
// will result in NFS4ERR_EXPIRED being returned." Lease expiry used to drop the
// state without a trace, so the stateid was merely absent from the tables and
// every path answered NFS4ERR_BAD_STATEID instead.
func TestExpiredLease_StateidsReportExpired(t *testing.T) {
	sm := NewStateManager(90 * time.Second)

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	// A stateid the server never issued: the control that keeps this test from
	// passing just because everything now answers NFS4ERR_EXPIRED.
	unknown := &types.Stateid4{Seqid: 1}
	unknown.Other = sm.generateStateidOther(StateTypeOpen)

	sm.onLeaseExpired(clientID)

	if _, err := sm.ValidateStateid(openStateid, fileHandle, StateidOpRead); !isStatus(err, types.NFS4ERR_EXPIRED) {
		t.Fatalf("ValidateStateid after lease cancellation: got %v, want NFS4ERR_EXPIRED", err)
	}

	if _, err := sm.CloseFile(openStateid, openSeqid+1); !isStatus(err, types.NFS4ERR_EXPIRED) {
		t.Fatalf("CloseFile after lease cancellation: got %v, want NFS4ERR_EXPIRED", err)
	}

	if _, err := sm.ValidateStateid(unknown, fileHandle, StateidOpRead); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Fatalf("ValidateStateid of a never-issued stateid: got %v, want NFS4ERR_BAD_STATEID", err)
	}
}

func isStatus(err error, status uint32) bool {
	var stateErr *NFS4StateError
	return errors.As(err, &stateErr) && stateErr.Status == status
}
