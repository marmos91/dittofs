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
// A reservation that outlives its request answers every later retransmission
// of that exact request with a silent drop until it ages past
// drcInProgressTTL. A handler that panics is recovered per request and leaves
// the connection open, so only an unconditional release returns that request
// to service before the bound expires.
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

// TestV3Dispatch_RoutesThroughDRC pins the v3 path to the shared protocol:
// one dispatch of a non-idempotent procedure leaves a recorded reply, so a
// retransmission of it replays instead of re-executing.
//
// The release on a panicking handler is pinned above rather than here: the v3
// dispatch produces a reply for every procedure it accepts, and provoking a
// panic inside the reservation needs a runtime the adapter does not have in a
// unit test. Both paths reserve and release through withDRC, so that is the
// seam the panic test covers.
func TestV3Dispatch_RoutesThroughDRC(t *testing.T) {
	adapter, conn := drcTestConn(t)
	const clientAddr = "10.4.5.6:2049"
	// Not a decodable file handle, so the dispatch answers from the handler
	// without needing a runtime behind it.
	body := []byte{0xff, 0xff, 0xff, 0xff}
	call := drcTestCall(0xC0DE)

	if !isCacheable(call.Procedure) {
		t.Fatalf("procedure %d is not cacheable; this test reserves no slot", call.Procedure)
	}

	reply, _ := conn.handleNFSProcedure(context.Background(), call, body, clientAddr)
	if len(reply) == 0 {
		t.Fatal("dispatch produced no reply; nothing could have been recorded")
	}

	res, cached := adapter.drc.lookup(clientAddr, call.XID, body)
	if res != drcReplay {
		t.Fatalf("retransmission lookup = %v, want drcReplay (the v3 dispatch bypassed the cache)", res)
	}
	if string(cached) != string(reply) {
		t.Fatalf("replayed %v, want the recorded reply %v", cached, reply)
	}
}
