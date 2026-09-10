package v41handlers

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// TestHandleCreateSession_AutoBindDirection covers the connection CREATE_SESSION
// binds to the session it just made.
//
// RFC 8881 Section 18.36.3 associates the connection CREATE_SESSION arrived on
// with the new session, in both directions when csa_flags asks for
// CONN_BACK_CHAN. The normal mount flow for both the Linux client and pynfs
// stops there: neither sends BIND_CONN_TO_SESSION afterwards. Binding
// fore-only therefore left every such session with a back-channel slot table
// and no connection under it, so no callback could be sent and no delegation
// could be granted -- the whole back-channel path was reachable only by a
// client that volunteered an extra operation.
func TestHandleCreateSession_AutoBindDirection(t *testing.T) {
	tests := []struct {
		name    string
		flags   uint32
		wantDir state.ConnectionDirection
	}{
		{
			name:    "back channel requested binds both directions",
			flags:   types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN,
			wantDir: state.ConnDirBoth,
		},
		{
			name:    "no back channel requested stays fore-only",
			flags:   0,
			wantDir: state.ConnDirFore,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sm := state.NewStateManager(90 * time.Second)
			defer sm.Shutdown()
			d := &Deps{StateManager: sm}

			var verifier [8]byte
			copy(verifier[:], "csbcverf")
			eid, err := sm.ExchangeID([]byte("create-session-autobind"), verifier, 0, nil, "10.0.0.1:12345")
			if err != nil {
				t.Fatalf("ExchangeID: %v", err)
			}

			args := types.CreateSessionArgs{
				ClientID:   eid.ClientID,
				SequenceID: eid.SequenceID,
				Flags:      tc.flags,
				ForeChannelAttrs: types.ChannelAttrs{
					MaxRequestSize: 1 << 20, MaxResponseSize: 1 << 20,
					MaxResponseSizeCached: 4096, MaxOperations: 16, MaxRequests: 16,
				},
				BackChannelAttrs: types.ChannelAttrs{
					MaxRequestSize: 1 << 16, MaxResponseSize: 1 << 16,
					MaxResponseSizeCached: 1 << 16, MaxOperations: 2, MaxRequests: 4,
				},
				CbProgram:  0x40000000,
				CbSecParms: []types.CallbackSecParms4{{CbSecFlavor: 0}},
			}
			var buf bytes.Buffer
			if err := args.Encode(&buf); err != nil {
				t.Fatalf("encode args: %v", err)
			}

			const connID = uint64(7001)
			ctx := &types.CompoundContext{
				Context:      context.Background(),
				ClientAddr:   "10.0.0.1:12345",
				ConnectionID: connID,
			}

			res := HandleCreateSession(d, ctx, &types.V41RequestContext{}, bytes.NewReader(buf.Bytes()))
			if res.Status != types.NFS4_OK {
				t.Fatalf("CREATE_SESSION status = %d, want NFS4_OK", res.Status)
			}

			binding := sm.GetConnectionBinding(connID)
			if binding == nil {
				t.Fatal("CREATE_SESSION did not bind the connection it arrived on")
			}
			if binding.Direction != tc.wantDir {
				t.Fatalf("bound direction = %v, want %v", binding.Direction, tc.wantDir)
			}

			// The direction is only interesting because of what it unlocks: a
			// back-bound connection is the one thing standing between a session
			// with back-channel slots and a callback that can actually be sent.
			backUsable := binding.Direction == state.ConnDirBack || binding.Direction == state.ConnDirBoth
			wantBackUsable := tc.flags&types.CREATE_SESSION4_FLAG_CONN_BACK_CHAN != 0
			if backUsable != wantBackUsable {
				t.Fatalf("back channel usable = %v, want %v", backUsable, wantBackUsable)
			}
		})
	}
}
