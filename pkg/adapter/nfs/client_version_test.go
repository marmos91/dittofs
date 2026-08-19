package nfs

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	v4handlers "github.com/marmos91/dittofs/internal/adapter/nfs/v4/handlers"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/pseudofs"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/clients"
)

// encodeCall builds a complete RPC-over-TCP call record (RFC 5531): a
// record-marking header followed by an AUTH_NULL call header and the procedure
// arguments.
func encodeCall(xid, program, version, procedure uint32, args []byte) []byte {
	var body []byte
	for _, v := range []uint32{
		xid,
		0, // msg_type: CALL
		2, // rpcvers
		program,
		version,
		procedure,
		0, 0, // cred: AUTH_NULL, zero-length body
		0, 0, // verf: AUTH_NULL, zero-length body
	} {
		body = binary.BigEndian.AppendUint32(body, v)
	}
	body = append(body, args...)
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(body))|0x80000000), body...)
}

// readReply consumes a single RPC-over-TCP reply frame.
func readReply(t *testing.T, conn net.Conn) {
	t.Helper()

	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		t.Fatalf("reading fragment header: %v", err)
	}
	bodyLen := binary.BigEndian.Uint32(header) &^ 0x80000000
	if _, err := io.ReadFull(conn, make([]byte, bodyLen)); err != nil {
		t.Fatalf("reading reply body: %v", err)
	}
}

// reportedVersion returns the NFS version the registry holds for the client, or
// "" when no NFS details have been recorded.
func reportedVersion(t *testing.T, rt *runtime.Runtime, clientID string) string {
	t.Helper()

	record := rt.Clients().Get(clientID)
	if record == nil {
		t.Fatalf("client %q is not registered", clientID)
	}
	if record.NFS == nil {
		return ""
	}
	return record.NFS.Version
}

// waitForClient blocks until the connection has registered itself, so the
// assertions do not race the Serve goroutine's first statements.
func waitForClient(t *testing.T, rt *runtime.Runtime, clientID string) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if rt.Clients().Get(clientID) != nil {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("client %q never registered", clientID)
}

// TestClientRegistry_VersionFromDispatch drives a served connection with a
// single NFS call and checks the version the registry ends up reporting.
//
// The version used to be hardcoded to "3" when the connection was accepted,
// before anything had been read off the wire, so an NFSv4 client was reported
// as v3 by /api/v1/clients. It is now unset at accept time and filled in from
// the dispatched call.
func TestClientRegistry_VersionFromDispatch(t *testing.T) {
	const clientID = "nfs-1"
	const unknownProc = uint32(99) // absent from the dispatch tables: cheap reply

	tests := []struct {
		name    string
		version uint32
		want    string
	}{
		{name: "v3", version: rpc.NFSVersion3, want: "3"},
		{name: "v4", version: rpc.NFSVersion4, want: "4"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			adapter := New(NFSConfig{Enabled: true, Port: 12049})
			rt := runtime.New(nil)
			adapter.Registry = rt

			client, server := net.Pipe()
			t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			served := make(chan struct{})
			go func() {
				defer close(served)
				NewNFSConnection(adapter, server, 1).Serve(ctx)
			}()

			waitForClient(t, rt, clientID)
			if got := reportedVersion(t, rt, clientID); got != "" {
				t.Errorf("version before any RPC: got %q, want empty (not known at accept time)", got)
			}

			if _, err := client.Write(encodeCall(0xCAFEF00D, rpc.ProgramNFS, tc.version, unknownProc, nil)); err != nil {
				t.Fatalf("writing call: %v", err)
			}
			// The reply is written after dispatch, so by the time it lands the
			// version has been reported.
			readReply(t, client)

			if got := reportedVersion(t, rt, clientID); got != tc.want {
				t.Errorf("reported version: got %q, want %q", got, tc.want)
			}

			_ = client.Close()
			select {
			case <-served:
			case <-time.After(2 * time.Second):
				t.Fatal("Serve did not return after the client closed")
			}
		})
	}
}

// newRegisteredConnection wires a connection to a live registry and registers it
// the way Serve does, without running the read loop.
func newRegisteredConnection(t *testing.T) (*NFSConnection, *runtime.Runtime) {
	t.Helper()

	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	rt := runtime.New(nil)
	adapter.Registry = rt

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	conn := NewNFSConnection(adapter, server, 1)
	conn.clientID = "nfs-1"
	rt.Clients().Register(&clients.ClientRecord{
		ClientID: conn.clientID,
		Protocol: "nfs",
		Address:  "127.0.0.1:12345",
	})
	return conn, rt
}

// TestNoteNFSVersion_MinorVersionRefinement verifies that the minor version
// decoded from a COMPOUND replaces the bare major learned from the RPC program
// version, and that a later bare major does not undo it — otherwise every v4
// COMPOUND would flap the record between "4" and "4.1", taking the registry
// write lock twice per request.
func TestNoteNFSVersion_MinorVersionRefinement(t *testing.T) {
	conn, rt := newRegisteredConnection(t)

	conn.noteNFSVersion("4")
	if got := reportedVersion(t, rt, conn.clientID); got != "4" {
		t.Fatalf("after program version: got %q, want %q", got, "4")
	}

	conn.noteNFSVersion("4.1")
	if got := reportedVersion(t, rt, conn.clientID); got != "4.1" {
		t.Fatalf("after COMPOUND minorversion: got %q, want %q", got, "4.1")
	}

	conn.noteNFSVersion("4")
	if got := reportedVersion(t, rt, conn.clientID); got != "4.1" {
		t.Errorf("after a further v4 call: got %q, want %q (minor version must not be dropped)", got, "4.1")
	}
}

// TestNoteNFSVersion_UnregisteredClient verifies the version report is a no-op
// once the client has gone away, rather than resurrecting a deregistered record.
func TestNoteNFSVersion_UnregisteredClient(t *testing.T) {
	conn, rt := newRegisteredConnection(t)

	rt.Clients().Deregister(conn.clientID)
	conn.noteNFSVersion("4.1")

	if record := rt.Clients().Get(conn.clientID); record != nil {
		t.Errorf("deregistered client was recreated: %+v", record)
	}
}

// encodeCompoundArgs builds COMPOUND4args: an empty tag, the minorversion, and
// a single PUTROOTFH operation.
func encodeCompoundArgs(minorVersion uint32) []byte {
	var args []byte
	for _, v := range []uint32{
		0, // tag: zero-length opaque
		minorVersion,
		1,  // numops
		24, // OP_PUTROOTFH
	} {
		args = binary.BigEndian.AppendUint32(args, v)
	}
	return args
}

// TestClientRegistry_VersionFromCompoundMinorVersion verifies that the dialect
// reported for a v4.1 client is "4.1" and not the bare "4" the RPC program
// version carries: the minorversion only appears inside the COMPOUND body.
func TestClientRegistry_VersionFromCompoundMinorVersion(t *testing.T) {
	const clientID = "nfs-1"

	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	rt := runtime.New(nil)
	adapter.Registry = rt
	adapter.v4Handler = v4handlers.NewHandler(rt, pseudofs.New())

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	served := make(chan struct{})
	go func() {
		defer close(served)
		NewNFSConnection(adapter, server, 1).Serve(ctx)
	}()

	waitForClient(t, rt, clientID)

	call := encodeCall(0xCAFEF00D, rpc.ProgramNFS, rpc.NFSVersion4, 1 /* COMPOUND */, encodeCompoundArgs(1))
	if _, err := client.Write(call); err != nil {
		t.Fatalf("writing COMPOUND: %v", err)
	}
	readReply(t, client)

	if got := reportedVersion(t, rt, clientID); got != "4.1" {
		t.Errorf("reported version: got %q, want %q", got, "4.1")
	}

	_ = client.Close()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the client closed")
	}
}
