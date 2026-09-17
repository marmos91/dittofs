package v41handlers

import (
	"bytes"
	"context"
	"encoding/binary"
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

// TestHandleCreateSession_RejectsOversizedAuthSysMachineName covers the handler
// half of the RFC 5531 Section 8.2 bound on a callback AUTH_SYS credential. The
// decode refuses a machinename past string<255>, and the operation must report
// that as NFS4ERR_INVAL rather than NFS4ERR_BADXDR: the body is well-formed
// XDR, and the credential names an identity the server will not store and
// re-emit on every callback. A session must not be created for it.
//
// The valid args are encoded with the real encoder, then the AUTH_SYS
// machinename is swapped for an over-long one in place, so the only thing
// unusual about the request is the value under test.
func TestHandleCreateSession_RejectsOversizedAuthSysMachineName(t *testing.T) {
	sm := state.NewStateManager(90 * time.Second)
	defer sm.Shutdown()
	d := &Deps{StateManager: sm}

	var verifier [8]byte
	copy(verifier[:], "overszver")
	eid, err := sm.ExchangeID([]byte("create-session-oversize-name"), verifier, 0, nil, "10.0.0.1:12345")
	if err != nil {
		t.Fatalf("ExchangeID: %v", err)
	}

	const machineName = "host"
	args := types.CreateSessionArgs{
		ClientID:   eid.ClientID,
		SequenceID: eid.SequenceID,
		Flags:      0,
		ForeChannelAttrs: types.ChannelAttrs{
			MaxRequestSize: 1 << 20, MaxResponseSize: 1 << 20,
			MaxResponseSizeCached: 4096, MaxOperations: 16, MaxRequests: 16,
		},
		BackChannelAttrs: types.ChannelAttrs{
			MaxRequestSize: 1 << 16, MaxResponseSize: 1 << 16,
			MaxResponseSizeCached: 1 << 16, MaxOperations: 2, MaxRequests: 4,
		},
		CbProgram: 0x40000000,
		CbSecParms: []types.CallbackSecParms4{{
			CbSecFlavor:  1, // AUTH_SYS
			AuthSysParms: &types.AuthSysParms{Stamp: 1, MachineName: machineName, UID: 1000, GID: 1000},
		}},
	}
	var buf bytes.Buffer
	if err := args.Encode(&buf); err != nil {
		t.Fatalf("encode args: %v", err)
	}

	// Swap the machinename for a 256-byte one: replace the length prefix and
	// the name bytes, keeping the XDR padding correct for the new length.
	raw := buf.Bytes()
	idx := bytes.Index(raw, append(be32(uint32(len(machineName))), []byte(machineName)...))
	if idx < 0 {
		t.Fatal("could not locate the encoded machinename")
	}
	const oversize = 256
	replacement := append(be32(oversize), bytes.Repeat([]byte{'a'}, oversize)...)
	replacement = append(replacement, make([]byte, (4-oversize%4)%4)...)
	raw = append(append(append([]byte{}, raw[:idx]...), replacement...), raw[idx+4+len(machineName):]...)

	ctx := &types.CompoundContext{
		Context:      context.Background(),
		ClientAddr:   "10.0.0.1:12345",
		ConnectionID: 7101,
	}

	res := HandleCreateSession(d, ctx, &types.V41RequestContext{}, bytes.NewReader(raw))
	if res.Status != types.NFS4ERR_INVAL {
		t.Fatalf("CREATE_SESSION status = %d, want NFS4ERR_INVAL for a %d-byte machinename",
			res.Status, oversize)
	}
	// The refusal must be complete: the connection it arrived on is not bound.
	if b := sm.GetConnectionBinding(7101); b != nil {
		t.Errorf("a rejected CREATE_SESSION still bound its connection (direction %v)", b.Direction)
	}
}

func be32(v uint32) []byte {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], v)
	return b[:]
}

// TestHandleBackchannelCtl_RejectsOversizedAuthSysMachineName is the
// BACKCHANNEL_CTL counterpart: the same RFC 5531 Section 8.2 bound, on the
// other operation that carries callback security parameters. The refusal must
// be NFS4ERR_INVAL for the same reason — the body is well-formed XDR and the
// credential is one the server will not store and re-emit.
func TestHandleBackchannelCtl_RejectsOversizedAuthSysMachineName(t *testing.T) {
	sm := state.NewStateManager(90 * time.Second)
	defer sm.Shutdown()
	d := &Deps{StateManager: sm}

	const machineName = "host"
	args := types.BackchannelCtlArgs{
		CbProgram: 0x40000000,
		SecParms: []types.CallbackSecParms4{{
			CbSecFlavor:  1, // AUTH_SYS
			AuthSysParms: &types.AuthSysParms{Stamp: 1, MachineName: machineName, UID: 1000, GID: 1000},
		}},
	}
	var buf bytes.Buffer
	if err := args.Encode(&buf); err != nil {
		t.Fatalf("encode args: %v", err)
	}

	raw := buf.Bytes()
	idx := bytes.Index(raw, append(be32(uint32(len(machineName))), []byte(machineName)...))
	if idx < 0 {
		t.Fatal("could not locate the encoded machinename")
	}
	const oversize = 256
	replacement := append(be32(oversize), bytes.Repeat([]byte{'a'}, oversize)...)
	replacement = append(replacement, make([]byte, (4-oversize%4)%4)...)
	raw = append(append(append([]byte{}, raw[:idx]...), replacement...), raw[idx+4+len(machineName):]...)

	ctx := &types.CompoundContext{
		Context:      context.Background(),
		ClientAddr:   "10.0.0.1:12345",
		ConnectionID: 7201,
	}

	res := HandleBackchannelCtl(d, ctx, &types.V41RequestContext{}, bytes.NewReader(raw))
	if res.Status != types.NFS4ERR_INVAL {
		t.Fatalf("BACKCHANNEL_CTL status = %d, want NFS4ERR_INVAL for a %d-byte machinename",
			res.Status, oversize)
	}
}
