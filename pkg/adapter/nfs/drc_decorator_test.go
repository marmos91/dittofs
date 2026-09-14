package nfs

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	nfs_types "github.com/marmos91/dittofs/internal/adapter/nfs/types"
)

func drcTestConn(t *testing.T) (*NFSAdapter, *NFSConnection) {
	t.Helper()
	adapter := New(NFSConfig{Enabled: true, Port: 12049})
	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	return adapter, NewNFSConnection(adapter, server, 1)
}

func drcTestCall(xid uint32) *rpc.RPCCallMessage {
	return &rpc.RPCCallMessage{
		XID:       xid,
		Program:   rpc.ProgramNFS,
		Version:   rpc.NFSVersion3,
		Procedure: nfs_types.NFSProcRemove,
		Cred:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
		Verf:      rpc.OpaqueAuth{Flavor: rpc.AuthNull, Body: []byte{}},
	}
}

// TestWithDRC_ReleasesReservationOnPanic is the reason the cache protocol lives
// in one place.
//
// lookup matches an in-progress entry before it considers age, so a
// reservation that outlives its request answers every later retransmission of
// that exact request with a silent drop, for as long as the connection lives.
// A handler that panics is recovered per request and leaves the connection
// open, so only an unconditional release covers that path.
func TestWithDRC_ReleasesReservationOnPanic(t *testing.T) {
	adapter, conn := drcTestConn(t)
	const clientAddr = "10.9.8.7:1234"
	body := []byte{1, 2, 3, 4}
	call := drcTestCall(0xFEED)

	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		_, _ = conn.withDRC(context.Background(), call, body, clientAddr, true, "TEST",
			func() ([]byte, bool, error) { panic("handler exploded") })
	}()
	if !panicked {
		t.Fatal("fn did not panic; this test no longer exercises the release path")
	}

	if res, _ := adapter.drc.lookup(clientAddr, call.XID, body); res != drcMiss {
		t.Fatalf("after a panicking handler, lookup = %v, want drcMiss (the reservation leaked)", res)
	}
}

// TestWithDRC_ReleasesReservationWhenNotRecordable pins the other release path:
// a request that completes but whose reply must not be replayed still has to
// give the reservation back, or a later legitimate retry is swallowed.
func TestWithDRC_ReleasesReservationWhenNotRecordable(t *testing.T) {
	adapter, conn := drcTestConn(t)
	const clientAddr = "10.9.8.7:5678"
	body := []byte{5, 6, 7, 8}
	call := drcTestCall(0xF00D)

	want := errors.New("decode failed")
	if _, err := conn.withDRC(context.Background(), call, body, clientAddr, true, "TEST",
		func() ([]byte, bool, error) { return nil, false, want },
	); !errors.Is(err, want) {
		t.Fatalf("withDRC returned %v, want the handler's error", err)
	}

	if res, _ := adapter.drc.lookup(clientAddr, call.XID, body); res != drcMiss {
		t.Fatalf("after an unrecordable reply, lookup = %v, want drcMiss (the reservation leaked)", res)
	}
}

// TestWithDRC_RecordsAndReplays is the non-vacuity guard for the two above: if
// the decorator never recorded anything, they would pass without the cache
// doing any work at all.
func TestWithDRC_RecordsAndReplays(t *testing.T) {
	_, conn := drcTestConn(t)
	const clientAddr = "10.9.8.7:9999"
	body := []byte{9, 9, 9, 9}
	call := drcTestCall(0xCAFE)
	reply := []byte("the one authoritative reply")

	calls := 0
	run := func() ([]byte, error) {
		return conn.withDRC(context.Background(), call, body, clientAddr, true, "TEST",
			func() ([]byte, bool, error) {
				calls++
				return reply, true, nil
			})
	}

	if _, err := run(); err != nil {
		t.Fatalf("first call: %v", err)
	}
	got, err := run()
	if err != nil {
		t.Fatalf("retransmission: %v", err)
	}
	if string(got) != string(reply) {
		t.Errorf("replayed reply = %q, want %q", got, reply)
	}
	if calls != 1 {
		t.Errorf("handler ran %d times; a retransmission must be replayed, not re-executed", calls)
	}
}

// TestWithDRC_BypassRunsHandlerEveryTime pins that an ineligible request is not
// silently cached.
func TestWithDRC_BypassRunsHandlerEveryTime(t *testing.T) {
	_, conn := drcTestConn(t)
	const clientAddr = "10.9.8.7:1111"
	body := []byte{4, 3, 2, 1}
	call := drcTestCall(0xABCD)

	calls := 0
	for i := 0; i < 2; i++ {
		if _, err := conn.withDRC(context.Background(), call, body, clientAddr, false, "TEST",
			func() ([]byte, bool, error) {
				calls++
				return []byte("x"), true, nil
			},
		); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if calls != 2 {
		t.Errorf("handler ran %d times; an ineligible request must not be cached", calls)
	}
}
