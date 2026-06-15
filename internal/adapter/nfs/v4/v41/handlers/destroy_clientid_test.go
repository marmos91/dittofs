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

// TestHandleDestroyClientID_CrossClientRejected is the core security regression
// test: a request whose identity resolves to client A must NOT be able to
// destroy client B's client ID. Before the fix DESTROY_CLIENTID performed no
// ownership check, so any peer operating its own session could tear down a
// victim's state by targeting the victim's 64-bit client ID. The server must
// now return NFS4ERR_NOT_SAME and leave the victim intact.
func TestHandleDestroyClientID_CrossClientRejected(t *testing.T) {
	sm := state.NewStateManager(90 * time.Second)
	d := &Deps{StateManager: sm}

	_, attackerSession := makeV41Client(t, sm, "attacker")
	victimClientID, _ := makeV41Client(t, sm, "victim")

	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: "10.0.0.9:1"}
	// The attacker identifies via its own SEQUENCE (v41ctx points at its session)
	// but targets the victim's client ID.
	v41ctx := &types.V41RequestContext{SessionID: attackerSession}

	res := HandleDestroyClientID(d, ctx, v41ctx, encodeDestroyClientidArgs(t, victimClientID))
	if res.Status != types.NFS4ERR_NOT_SAME {
		t.Fatalf("cross-client DESTROY_CLIENTID: status = %d, want NFS4ERR_NOT_SAME (%d)",
			res.Status, types.NFS4ERR_NOT_SAME)
	}

	// The victim's client record must still exist -- it was NOT destroyed.
	if !v41ClientExists(sm, victimClientID) {
		t.Fatal("victim client was destroyed despite ownership mismatch")
	}
}

// TestHandleDestroyClientID_OwnerPassesOwnershipCheck verifies the owner is not
// wrongly blocked: a request whose SEQUENCE identifies the same client that owns
// the target client ID clears the ownership gate. Because the owner still holds
// the identifying session, the operation then correctly returns
// NFS4ERR_CLIENTID_BUSY (RFC 8881 Section 18.50) rather than an authz error.
func TestHandleDestroyClientID_OwnerPassesOwnershipCheck(t *testing.T) {
	sm := state.NewStateManager(90 * time.Second)
	d := &Deps{StateManager: sm}

	ownerClientID, ownerSession := makeV41Client(t, sm, "owner")

	ctx := &types.CompoundContext{Context: context.Background(), ClientAddr: "10.0.0.9:1"}
	v41ctx := &types.V41RequestContext{SessionID: ownerSession}

	res := HandleDestroyClientID(d, ctx, v41ctx, encodeDestroyClientidArgs(t, ownerClientID))
	if res.Status != types.NFS4ERR_CLIENTID_BUSY {
		t.Fatalf("owner DESTROY_CLIENTID with active session: status = %d, want NFS4ERR_CLIENTID_BUSY (%d)",
			res.Status, types.NFS4ERR_CLIENTID_BUSY)
	}
}
