package handlers

import (
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// panicWhenReserved panics on the first runtime call made while the replay
// reservation is held, and behaves normally otherwise. Targeting the live
// reservation rather than a call count keeps the injection pinned to the
// window under test even if the CREATE path is reordered around it.
type panicWhenReserved struct {
	smbRuntime
	reserved func() bool
}

func (p *panicWhenReserved) GetMetadataService() *metadata.Service {
	if p.reserved() {
		panic("handler exploded inside the replay-reservation window")
	}
	return p.smbRuntime.GetMetadataService()
}

// TestCreateReplay_PanicReleasesReservation pins the reservation lifecycle
// against a panicking completion.
//
// The inline CREATE path reserves the CreateGuid before dispatching the lease
// break and releases it once the completion returns. A panic in between skips
// that release, and nothing else collects it: a reservation carries no
// timestamp, the cache's prune walks stored entries rather than reservations,
// and only session teardown clears the remainder. The per-request recover in
// the connection layer logs the panic and keeps the session alive, so that
// teardown does not run either.
//
// A stranded reservation makes every later replay of the guid answer
// STATUS_FILE_NOT_AVAILABLE for the rest of the session — the refusal
// TestReplay_PendingReservationFileNotAvailable asserts for a genuinely parked
// CREATE, now owed to a client whose CREATE is long dead.
func TestCreateReplay_PanicReleasesReservation(t *testing.T) {
	e := setupReopen2Env(t)

	sessionID := e.tree.SessionID
	createGuid := [16]byte{0xAA, 0xBB, 0xCC, 0xDD}
	e.h.CreateSessionWithID(sessionID, "127.0.0.1:1", false, "alice", "WORKGROUP")

	e.h.Registry = &panicWhenReserved{
		smbRuntime: e.h.Registry,
		reserved:   func() bool { return e.h.CreateReplayCache.IsReserved(sessionID, createGuid) },
	}

	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		ctx := e.makeSMBCtx(sessionID)
		//nolint:errcheck // expected to panic before returning
		e.h.Create(ctx, &CreateRequest{
			FileName:          "durable.txt",
			DesiredAccess:     0x001F01FF,
			ShareAccess:       0x07,
			CreateDisposition: types.FileOpen,
			OplockLevel:       OplockLevelBatch,
			CreateContexts:    []CreateContext{dh2qContext(createGuid, 300000)},
		})
		return p
	}()

	// A guard that never fired proves nothing: without this check the test
	// would pass whenever the injection stopped reaching the reserved window.
	if !panicked {
		t.Fatal("injection never fired; the test no longer reaches the reserved window")
	}

	if e.h.CreateReplayCache.IsReserved(sessionID, createGuid) {
		t.Fatal("reservation survived the panic: every later replay of this guid " +
			"answers STATUS_FILE_NOT_AVAILABLE until the session ends")
	}
}
