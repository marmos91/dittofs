package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	mount_handlers "github.com/marmos91/dittofs/internal/adapter/nfs/mount/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// readAcceptedReply reads one RPC record off the client end of the pipe and
// returns the reply body. A nil return means nothing was written before the
// deadline, which is how a call that never reaches the version check looks
// from here.
func readAcceptedReply(t *testing.T, client net.Conn) []byte {
	t.Helper()
	if err := client.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	header := make([]byte, 4)
	if _, err := io.ReadFull(client, header); err != nil {
		return nil
	}
	bodyLen := binary.BigEndian.Uint32(header) &^ 0x80000000
	body := make([]byte, bodyLen)
	if _, err := io.ReadFull(client, body); err != nil {
		return nil
	}
	return body
}

func driveCall(t *testing.T, call *rpc.RPCCallMessage) []byte {
	t.Helper()
	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	conn := NewNFSConnection(adapter, server, 1)

	go func() {
		_ = conn.handleRPCCall(context.Background(), call, nil, nil)
	}()
	return readAcceptedReply(t, client)
}

func versionCall(program, version, procedure uint32) *rpc.RPCCallMessage {
	return &rpc.RPCCallMessage{
		XID:       0x5A5A5A5A,
		Program:   program,
		Version:   version,
		Procedure: procedure,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}
}

// TestHandleRPCCall_VersionNegotiation pins the version contract at the entry
// point that actually serves traffic. A client that asks for a version the
// server does not implement must learn the supported range from the reply
// rather than be met with silence or a generic error, because the range is
// what lets it retry at a version that works.
func TestHandleRPCCall_VersionNegotiation(t *testing.T) {
	tests := []struct {
		name              string
		program           uint32
		version           uint32
		procedure         uint32
		wantLow, wantHigh uint32
	}{
		{"NFS_v1", rpc.ProgramNFS, 1, 0, rpc.NFSVersion3, rpc.NFSVersion4},
		{"NFS_v2", rpc.ProgramNFS, 2, 0, rpc.NFSVersion3, rpc.NFSVersion4},
		{"NFS_v5", rpc.ProgramNFS, 5, 0, rpc.NFSVersion3, rpc.NFSVersion4},
		{"NSM_v2", rpc.ProgramNSM, 2, 0, rpc.NSMVersion1, rpc.NSMVersion1},
		{"NLM_v2", rpc.ProgramNLM, 2, 0, rpc.NLMVersion1, rpc.NLMVersion4},
		// MNT alone is version-pinned: it returns a v3 file handle, so it
		// cannot be served to a v1 caller that would misread the handle.
		{"Mount_MNT_v1", rpc.ProgramMount, 1, mount_handlers.MountProcMnt, rpc.MountVersion3, rpc.MountVersion3},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := driveCall(t, versionCall(tc.program, tc.version, tc.procedure))
			if body == nil {
				t.Fatal("no reply written; the version check must answer, not drop the call")
			}
			if len(body) < 32 {
				t.Fatalf("reply too short for a mismatch reply: %d bytes", len(body))
			}
			if got := binary.BigEndian.Uint32(body[20:24]); got != rpc.RPCProgMismatch {
				t.Fatalf("accept_stat = %d, want %d (PROG_MISMATCH)", got, rpc.RPCProgMismatch)
			}
			if got := binary.BigEndian.Uint32(body[24:28]); got != tc.wantLow {
				t.Errorf("mismatch_info.low = %d, want %d", got, tc.wantLow)
			}
			if got := binary.BigEndian.Uint32(body[28:32]); got != tc.wantHigh {
				t.Errorf("mismatch_info.high = %d, want %d", got, tc.wantHigh)
			}
		})
	}
}

// TestHandleRPCCall_NLMv1IsNotVersionRejected pins the case the two dispatch
// layers disagreed on. BSD and macOS NFSv3 clients negotiate NLM v1/v3 rather
// than v4, so rejecting v1 with PROG_MISMATCH would leave those clients unable
// to lock at all. The call may fail later for want of a wired handler — that is
// not what this test measures; it measures that the version check let it past.
func TestHandleRPCCall_NLMv1IsNotVersionRejected(t *testing.T) {
	for _, version := range []uint32{rpc.NLMVersion1, rpc.NLMVersion3, rpc.NLMVersion4} {
		body := driveCall(t, versionCall(rpc.ProgramNLM, version, 0))
		if body == nil {
			t.Fatalf("NLM v%d produced no reply; a served version must answer", version)
		}
		if len(body) < 24 {
			t.Fatalf("NLM v%d reply too short to carry accept_stat: %d bytes", version, len(body))
		}
		if got := binary.BigEndian.Uint32(body[20:24]); got != rpc.RPCSuccess {
			t.Errorf("NLM v%d accept_stat = %d, want %d (SUCCESS); v1/v3/v4 are all served",
				version, got, rpc.RPCSuccess)
		}
	}
}

// TestHandleRPCCall_ServedVersionsAreAccepted is the other half of the version
// contract: a version the server does implement must reach its handler rather
// than be answered with a mismatch. Without it, a regression that rejects
// everything would satisfy the negotiation table above.
func TestHandleRPCCall_ServedVersionsAreAccepted(t *testing.T) {
	tests := []struct {
		name      string
		program   uint32
		version   uint32
		procedure uint32
	}{
		{"NFS_v3_NULL", rpc.ProgramNFS, rpc.NFSVersion3, 0},
		// Every MOUNT procedure except MNT is version-agnostic, so a v1 caller
		// is served rather than told to speak v3.
		{"Mount_NULL_v1", rpc.ProgramMount, 1, 0},
		{"Mount_NULL_v3", rpc.ProgramMount, rpc.MountVersion3, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := driveCall(t, versionCall(tc.program, tc.version, tc.procedure))
			if body == nil {
				t.Fatal("no reply written for a version the server serves")
			}
			if len(body) < 24 {
				t.Fatalf("reply too short to carry accept_stat: %d bytes", len(body))
			}
			if got := binary.BigEndian.Uint32(body[20:24]); got != rpc.RPCSuccess {
				t.Errorf("accept_stat = %d, want %d (SUCCESS)", got, rpc.RPCSuccess)
			}
		})
	}
}
