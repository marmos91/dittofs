package handlers

import (
	"context"
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/lease"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// TestOplockBreakAck_LeaseNotBoundToSession_Rejected pins the ownership check
// on the traditional oplock break ack: a session with no binding for the
// lease key must be rejected with STATUS_INVALID_DEVICE_REQUEST and must not
// have the acknowledged level applied to the file's oplock state.
func TestOplockBreakAck_LeaseNotBoundToSession_Rejected(t *testing.T) {
	h := NewHandler()

	uid := uint32(1001)
	user := &models.User{ID: "alice", Username: "alice", UID: &uid}
	sess := h.CreateSession("127.0.0.1:0", false, "alice", "")
	sess.User = user

	// No LeaseManager: the handler returns before the ownership check, so give
	// it one whose share resolver has no lock manager (VerifyLeaseAckOwnership
	// then finds no binding for any key — exactly the "not bound to this
	// session" case).
	h.LeaseManager = lease.NewLeaseManager(&nilLockResolver{}, nil)

	const treeID uint32 = 7
	h.StoreTree(&TreeConnection{
		TreeID:    treeID,
		SessionID: sess.SessionID,
		ShareName: "/share",
	})

	openFile := (&OpenFile{
		FileID:      [16]byte{0x01, 0x02},
		TreeID:      treeID,
		SessionID:   sess.SessionID,
		LeaseKey:    [16]byte{0x77},
		OplockLevel: OplockLevelII,
	}).WithName(OpenName{Path: "/share/a.txt"})
	h.StoreOpenFile(openFile)

	// 24-byte OPLOCK_BREAK ack: StructureSize(2)=24, Reserved(2), OplockLevel(1)
	// at offset 4, Reserved(5), FileId(16) at offset 8.
	ackBytes := make([]byte, 24)
	binary.LittleEndian.PutUint16(ackBytes[0:2], 24)
	ackBytes[4] = 0 // acked down to none
	copy(ackBytes[8:24], []byte{0x01, 0x02})

	handlerCtx := &SMBHandlerContext{
		Context:    context.Background(),
		ClientAddr: "127.0.0.1:0",
		SessionID:  sess.SessionID,
		TreeID:     treeID,
	}
	res, err := h.handleOplockBreakAck(handlerCtx, ackBytes)
	if err != nil {
		t.Fatalf("handleOplockBreakAck: %v", err)
	}
	if res.Status != types.StatusInvalidDeviceRequest {
		t.Errorf("status = %v, want STATUS_INVALID_DEVICE_REQUEST (lease not bound to this session)", res.Status)
	}

	got, ok := h.GetOpenFile([16]byte{0x01, 0x02})
	if !ok {
		t.Fatal("open file disappeared")
	}
	if got.OplockLevel != OplockLevelII {
		t.Errorf("OplockLevel = %d, want %d — the acked level was applied despite the failed ownership check", got.OplockLevel, OplockLevelII)
	}
}

// nilLockResolver returns no lock manager for any share, so every lease key is
// unbound — the ownership check's rejection path.
type nilLockResolver struct{}

func (nilLockResolver) GetLockManagerForShare(string) lock.LockManager { return nil }

var _ = lock.LeaseStateNone
