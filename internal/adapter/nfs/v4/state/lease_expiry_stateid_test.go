package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
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

// TestExpiredLease_LockStateidsReportExpired covers the LOCK and LOCKU paths,
// which look their stateid up in the lock table rather than through
// ValidateStateid and used to answer NFS4ERR_BAD_STATEID for state the server
// itself had freed.
func TestExpiredLease_LockStateidsReportExpired(t *testing.T) {
	sm := NewStateManager(90 * time.Second)
	sm.SetLockManager(lock.NewManager())
	defer sm.Shutdown()

	clientID, fileHandle, openStateid, openSeqid := setupClientAndOpenState(t, sm)

	lockResult, err := sm.LockNew(context.Background(),
		clientID, []byte("lock-owner"), 1,
		openStateid, openSeqid+1,
		fileHandle, types.WRITE_LT, 0, 100, false,
	)
	if err != nil {
		t.Fatalf("LockNew: %v", err)
	}
	lockStateid := lockResult.Stateid

	unknown := &types.Stateid4{Seqid: 1}
	unknown.Other = sm.generateStateidOther(StateTypeLock)

	sm.onLeaseExpired(clientID)

	if _, err := sm.UnlockFile(&lockStateid, 2, types.WRITE_LT, 0, 100); !isStatus(err, types.NFS4ERR_EXPIRED) {
		t.Fatalf("UnlockFile after lease cancellation: got %v, want NFS4ERR_EXPIRED", err)
	}

	if _, err := sm.LockExisting(context.Background(),
		&lockStateid, 2, fileHandle, types.WRITE_LT, 200, 100, false,
	); !isStatus(err, types.NFS4ERR_EXPIRED) {
		t.Fatalf("LockExisting after lease cancellation: got %v, want NFS4ERR_EXPIRED", err)
	}

	if _, err := sm.UnlockFile(unknown, 2, types.WRITE_LT, 0, 100); !isStatus(err, types.NFS4ERR_BAD_STATEID) {
		t.Fatalf("UnlockFile of a never-issued stateid: got %v, want NFS4ERR_BAD_STATEID", err)
	}
}

func isStatus(err error, status uint32) bool {
	var stateErr *NFS4StateError
	return errors.As(err, &stateErr) && stateErr.Status == status
}
