package v41handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// makeV41Client registers a confirmed v4.1 client with one session and returns
// its client ID and the session ID (used to identify the requester).
func makeV41Client(t *testing.T, sm *state.StateManager, ownerSuffix string) (uint64, types.SessionId4) {
	t.Helper()

	var verifier [8]byte
	copy(verifier[:], "verify01")
	eid, err := sm.ExchangeID([]byte("destroy-clientid-test-"+ownerSuffix), verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID(%s): %v", ownerSuffix, err)
	}

	csRes, _, err := sm.CreateSession(
		eid.ClientID, eid.SequenceID, 0,
		types.ChannelAttrs{MaxRequestSize: 1 << 20, MaxResponseSize: 1 << 20, MaxRequests: 16},
		types.ChannelAttrs{MaxRequestSize: 1 << 16, MaxResponseSize: 1 << 16, MaxRequests: 4},
		0, nil,
	)
	if err != nil {
		t.Fatalf("CreateSession(%s): %v", ownerSuffix, err)
	}
	return eid.ClientID, csRes.SessionID
}

func v41ClientExists(sm *state.StateManager, clientID uint64) bool {
	for _, rec := range sm.ListV41Clients() {
		if rec.ClientID == clientID {
			return true
		}
	}
	return false
}

func encodeDestroyClientidArgs(t *testing.T, clientID uint64) *bytes.Reader {
	t.Helper()
	var buf bytes.Buffer
	args := types.DestroyClientidArgs{ClientID: clientID}
	if err := args.Encode(&buf); err != nil {
		t.Fatalf("encode args: %v", err)
	}
	return bytes.NewReader(buf.Bytes())
}

// TestHandleDestroyClientID_CrossClientTargetPermitted pins the behaviour
// RFC 8881 Section 18.50.3 requires when the client ID derived from the
// preceding SEQUENCE is not the one being destroyed: the operation proceeds.
// The target here holds no session and no state, so it is destroyed.
func TestHandleDestroyClientID_CrossClientTargetPermitted(t *testing.T) {
	sm := state.NewStateManager(90 * time.Second)
	d := &Deps{StateManager: sm}

	_, requesterSession := makeV41Client(t, sm, "requester")

	var verifier [8]byte
	copy(verifier[:], "verify01")
	target, err := sm.ExchangeID([]byte("destroy-clientid-test-target"), verifier, 0, nil, "10.0.0.2:12345")
	if err != nil {
		t.Fatalf("ExchangeID(target): %v", err)
	}

	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: "10.0.0.9:1"}
	v41ctx := &types.V41RequestContext{SessionID: requesterSession}

	res := HandleDestroyClientID(d, ctx, v41ctx, encodeDestroyClientidArgs(t, target.ClientID))
	if res.Status != types.NFS4_OK {
		t.Fatalf("cross-client DESTROY_CLIENTID: status = %d, want NFS4_OK", res.Status)
	}
	if v41ClientExists(sm, target.ClientID) {
		t.Fatal("target client still exists after a successful DESTROY_CLIENTID")
	}
}

// TestHandleDestroyClientID_SelfTargetIsBusy verifies the converse: when the
// SEQUENCE identifies the same client the request targets, that client still
// holds the session carrying the request, so the reply is
// NFS4ERR_CLIENTID_BUSY (RFC 8881 Section 18.50).
func TestHandleDestroyClientID_SelfTargetIsBusy(t *testing.T) {
	sm := state.NewStateManager(90 * time.Second)
	d := &Deps{StateManager: sm}

	ownerClientID, ownerSession := makeV41Client(t, sm, "owner")

	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: "10.0.0.9:1"}
	v41ctx := &types.V41RequestContext{SessionID: ownerSession}

	res := HandleDestroyClientID(d, ctx, v41ctx, encodeDestroyClientidArgs(t, ownerClientID))
	if res.Status != types.NFS4ERR_CLIENTID_BUSY {
		t.Fatalf("self-target DESTROY_CLIENTID with active session: status = %d, want NFS4ERR_CLIENTID_BUSY (%d)",
			res.Status, types.NFS4ERR_CLIENTID_BUSY)
	}
}
