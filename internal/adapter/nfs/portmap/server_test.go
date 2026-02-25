package portmap

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
)

// ============================================================================
// Test Helpers
// ============================================================================

// startTestServer starts a portmapper server on a random port and returns it.
// The server is stopped automatically when the test completes.
func startTestServer(t *testing.T) (*Server, *Registry) {
	t.Helper()

	registry := NewRegistry()
	srv := NewServer(ServerConfig{
		Port:      0, // Random port
		EnableTCP: true,
		EnableUDP: true,
		Registry:  registry,
	})

	ctx, cancel := context.WithCancel(context.Background())

	// We need to start TCP and UDP on different random ports
	// Use a custom start that picks random ports
	tcpListener, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatalf("listen TCP: %v", err)
	}
	srv.tcpListener = tcpListener

	udpAddr, err := net.ResolveUDPAddr("udp", ":0")
	if err != nil {
		t.Fatalf("resolve UDP: %v", err)
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		_ = tcpListener.Close()
		t.Fatalf("listen UDP: %v", err)
	}
	srv.udpConn = udpConn

	// Signal that listeners are ready (since we bound manually)
	close(srv.listenerReady)

	srv.wg.Add(2)
	go srv.serveTCP(ctx)
	go srv.serveUDP(ctx)

	// Monitor context cancellation
	go func() {
		select {
		case <-ctx.Done():
			srv.shutdownOnce.Do(func() {
				close(srv.shutdown)
				_ = srv.tcpListener.Close()
				_ = srv.udpConn.Close()
			})
		case <-srv.shutdown:
		}
	}()

	t.Cleanup(func() {
		cancel()
		// Close listeners to unblock goroutines
		srv.shutdownOnce.Do(func() {
			close(srv.shutdown)
			_ = srv.tcpListener.Close()
			_ = srv.udpConn.Close()
		})
		srv.wg.Wait()
	})

	return srv, registry
}

// buildRPCCall constructs a raw RPC call message for testing.
//
// Wire format:
//
//	xid(4) + msg_type=0(4) + rpc_vers=2(4) + prog(4) + vers(4) + proc(4)
//	+ cred_flavor=0(4) + cred_len=0(4) + verf_flavor=0(4) + verf_len=0(4)
//	+ [procedure args]
func buildRPCCall(xid, prog, vers, proc uint32, args []byte) []byte {
	header := make([]byte, 40) // 10 uint32 fields = 40 bytes

	binary.BigEndian.PutUint32(header[0:4], xid)
	binary.BigEndian.PutUint32(header[4:8], 0)  // msg_type = CALL
	binary.BigEndian.PutUint32(header[8:12], 2) // rpc_vers = 2
	binary.BigEndian.PutUint32(header[12:16], prog)
	binary.BigEndian.PutUint32(header[16:20], vers)
	binary.BigEndian.PutUint32(header[20:24], proc)
	binary.BigEndian.PutUint32(header[24:28], 0) // cred_flavor = AUTH_NULL
	binary.BigEndian.PutUint32(header[28:32], 0) // cred_len = 0
	binary.BigEndian.PutUint32(header[32:36], 0) // verf_flavor = AUTH_NULL
	binary.BigEndian.PutUint32(header[36:40], 0) // verf_len = 0

	if len(args) > 0 {
		msg := make([]byte, len(header)+len(args))
		copy(msg, header)
		copy(msg[len(header):], args)
		return msg
	}
	return header
}

// wrapWithRecordMarking adds TCP record marking header.
func wrapWithRecordMarking(data []byte) []byte {
	result := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(result[0:4], 0x80000000|uint32(len(data)))
	copy(result[4:], data)
	return result
}

// sendTCPRequest sends a TCP RPC request and reads the response.
func sendTCPRequest(t *testing.T, addr string, msg []byte) []byte {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("dial TCP: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// Send with record marking
	framedMsg := wrapWithRecordMarking(msg)
	if _, err := conn.Write(framedMsg); err != nil {
		t.Fatalf("write TCP: %v", err)
	}

	// Read reply fragment header
	var headerBuf [4]byte
	if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
		t.Fatalf("read reply header: %v", err)
	}
	headerVal := binary.BigEndian.Uint32(headerBuf[:])
	replyLen := headerVal & 0x7FFFFFFF

	// Read reply body
	replyBuf := make([]byte, replyLen)
	if _, err := io.ReadFull(conn, replyBuf); err != nil {
		t.Fatalf("read reply body: %v", err)
	}

	return replyBuf
}

// sendUDPRequest sends a UDP RPC request and reads the response.
func sendUDPRequest(t *testing.T, addr string, msg []byte) []byte {
	t.Helper()

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("resolve UDP addr: %v", err)
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		t.Fatalf("dial UDP: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	// Send raw (no record marking for UDP)
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write UDP: %v", err)
	}

	// Read reply
	replyBuf := make([]byte, 65535)
	n, err := conn.Read(replyBuf)
	if err != nil {
		t.Fatalf("read UDP reply: %v", err)
	}

	return replyBuf[:n]
}

// parseReplyHeader extracts xid, accept_stat, and payload from a reply body.
func parseReplyHeader(reply []byte) (xid uint32, acceptStat uint32, payload []byte) {
	if len(reply) < 24 {
		return 0, 0, nil
	}

	xid = binary.BigEndian.Uint32(reply[0:4])
	// reply[4:8]  = msg_type (should be 1 = REPLY)
	// reply[8:12] = reply_state (should be 0 = MSG_ACCEPTED)
	// reply[12:16] = verf_flavor
	// reply[16:20] = verf_len
	acceptStat = binary.BigEndian.Uint32(reply[20:24])

	if len(reply) > 24 {
		payload = reply[24:]
	}

	return xid, acceptStat, payload
}

// encodeMappingArgs creates XDR-encoded mapping arguments for SET/UNSET/GETPORT.
func encodeMappingArgs(prog, vers, prot, port uint32) []byte {
	return xdr.EncodeMapping(&xdr.Mapping{
		Prog: prog, Vers: vers, Prot: prot, Port: port,
	})
}

// ============================================================================
// TCP Tests
// ============================================================================

func TestServerTCPNull(t *testing.T) {
	srv, _ := startTestServer(t)

	msg := buildRPCCall(0x12345678, types.ProgramPortmap, types.PortmapVersion2, types.ProcNull, nil)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0x12345678 {
		t.Errorf("XID mismatch: got 0x%x, want 0x12345678", xid)
	}
	if acceptStat != 0 { // SUCCESS
		t.Errorf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
	}
	if len(payload) != 0 {
		t.Errorf("NULL should return empty payload, got %d bytes", len(payload))
	}
}

func TestServerTCPGetport(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register NFS on TCP port 12049
	registry.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})

	// Query GETPORT for NFS v3 TCP
	args := encodeMappingArgs(100003, 3, 6, 0)
	msg := buildRPCCall(0xAABBCCDD, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0xAABBCCDD {
		t.Errorf("XID mismatch: got 0x%x, want 0xAABBCCDD", xid)
	}
	if acceptStat != 0 {
		t.Errorf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
	}
	if len(payload) < 4 {
		t.Fatalf("payload too short: %d bytes", len(payload))
	}

	port := binary.BigEndian.Uint32(payload[0:4])
	if port != 12049 {
		t.Errorf("port: got %d, want 12049", port)
	}
}

func TestServerTCPGetportNotFound(t *testing.T) {
	srv, _ := startTestServer(t)

	// Query GETPORT for unregistered service
	args := encodeMappingArgs(99999, 1, 6, 0)
	msg := buildRPCCall(0x11111111, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	_, acceptStat, payload := parseReplyHeader(reply)
	if acceptStat != 0 {
		t.Errorf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
	}
	if len(payload) < 4 {
		t.Fatalf("payload too short: %d bytes", len(payload))
	}

	port := binary.BigEndian.Uint32(payload[0:4])
	if port != 0 {
		t.Errorf("port: got %d, want 0 (not registered)", port)
	}
}

func TestServerTCPDump(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register multiple services
	registry.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})
	registry.Set(&xdr.Mapping{Prog: 100005, Vers: 3, Prot: 6, Port: 12049})
	registry.Set(&xdr.Mapping{Prog: 100021, Vers: 4, Prot: 17, Port: 12049})

	msg := buildRPCCall(0x22222222, types.ProgramPortmap, types.PortmapVersion2, types.ProcDump, nil)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0x22222222 {
		t.Errorf("XID mismatch: got 0x%x, want 0x22222222", xid)
	}
	if acceptStat != 0 {
		t.Errorf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
	}

	// Parse the DUMP response: linked list of entries
	// Each entry: discriminant(4) + mapping(16), terminated by discriminant(0)
	count := 0
	offset := 0
	for offset < len(payload) {
		if offset+4 > len(payload) {
			break
		}
		disc := binary.BigEndian.Uint32(payload[offset : offset+4])
		offset += 4
		if disc == 0 {
			break // End of list
		}
		if offset+16 > len(payload) {
			t.Fatal("truncated mapping entry in DUMP response")
		}
		offset += 16 // Skip mapping data
		count++
	}

	if count != 3 {
		t.Errorf("DUMP entry count: got %d, want 3", count)
	}
}

func TestServerTCPCallit(t *testing.T) {
	srv, _ := startTestServer(t)

	// CALLIT is procedure 5, which is NOT in the dispatch table
	msg := buildRPCCall(0x33333333, types.ProgramPortmap, types.PortmapVersion2, types.ProcCallit, nil)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	xid, acceptStat, _ := parseReplyHeader(reply)
	if xid != 0x33333333 {
		t.Errorf("XID mismatch: got 0x%x, want 0x33333333", xid)
	}
	if acceptStat != 3 { // PROC_UNAVAIL
		t.Errorf("accept_stat: got %d, want 3 (PROC_UNAVAIL)", acceptStat)
	}
}

func TestServerTCPVersionMismatch(t *testing.T) {
	srv, _ := startTestServer(t)

	// Send request with version=1 (only version 2 is supported)
	msg := buildRPCCall(0x44444444, types.ProgramPortmap, 1, types.ProcNull, nil)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0x44444444 {
		t.Errorf("XID mismatch: got 0x%x, want 0x44444444", xid)
	}
	if acceptStat != 2 { // PROG_MISMATCH
		t.Errorf("accept_stat: got %d, want 2 (PROG_MISMATCH)", acceptStat)
	}

	// Parse low/high version range
	if len(payload) < 8 {
		t.Fatalf("PROG_MISMATCH payload too short: %d bytes", len(payload))
	}
	low := binary.BigEndian.Uint32(payload[0:4])
	high := binary.BigEndian.Uint32(payload[4:8])

	if low != 2 || high != 2 {
		t.Errorf("version range: got [%d, %d], want [2, 2]", low, high)
	}
}

func TestServerTCPWrongProgram(t *testing.T) {
	srv, _ := startTestServer(t)

	// Send request with wrong program number -- should get PROG_UNAVAIL (1)
	msg := buildRPCCall(0xCCCCCCCC, 999999, types.PortmapVersion2, types.ProcNull, nil)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	xid, acceptStat, _ := parseReplyHeader(reply)
	if xid != 0xCCCCCCCC {
		t.Errorf("XID mismatch: got 0x%x, want 0xCCCCCCCC", xid)
	}
	if acceptStat != 1 { // PROG_UNAVAIL
		t.Errorf("accept_stat: got %d, want 1 (PROG_UNAVAIL)", acceptStat)
	}
}

func TestServerTCPConnectionReuse(t *testing.T) {
	srv, registry := startTestServer(t)

	registry.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})

	// Open a single TCP connection
	conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	// Send multiple requests over the same connection
	for i := range 3 {
		xid := uint32(0xD0000000 + i)
		args := encodeMappingArgs(100003, 3, 6, 0)
		msg := buildRPCCall(xid, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
		framedMsg := wrapWithRecordMarking(msg)

		if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("set deadline: %v", err)
		}

		if _, err := conn.Write(framedMsg); err != nil {
			t.Fatalf("write request %d: %v", i, err)
		}

		// Read reply
		var headerBuf [4]byte
		if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
			t.Fatalf("read reply header %d: %v", i, err)
		}
		headerVal := binary.BigEndian.Uint32(headerBuf[:])
		replyLen := headerVal & 0x7FFFFFFF

		replyBuf := make([]byte, replyLen)
		if _, err := io.ReadFull(conn, replyBuf); err != nil {
			t.Fatalf("read reply body %d: %v", i, err)
		}

		replyXID, acceptStat, payload := parseReplyHeader(replyBuf)
		if replyXID != xid {
			t.Errorf("request %d: XID mismatch: got 0x%x, want 0x%x", i, replyXID, xid)
		}
		if acceptStat != 0 {
			t.Errorf("request %d: accept_stat: got %d, want 0", i, acceptStat)
		}
		if len(payload) < 4 {
			t.Fatalf("request %d: payload too short", i)
		}
		port := binary.BigEndian.Uint32(payload[0:4])
		if port != 12049 {
			t.Errorf("request %d: port: got %d, want 12049", i, port)
		}
	}
}

// ============================================================================
// UDP Tests
// ============================================================================

func TestServerUDPNull(t *testing.T) {
	srv, _ := startTestServer(t)

	msg := buildRPCCall(0x55555555, types.ProgramPortmap, types.PortmapVersion2, types.ProcNull, nil)
	reply := sendUDPRequest(t, srv.UDPAddr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0x55555555 {
		t.Errorf("XID mismatch: got 0x%x, want 0x55555555", xid)
	}
	if acceptStat != 0 {
		t.Errorf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
	}
	if len(payload) != 0 {
		t.Errorf("NULL should return empty payload, got %d bytes", len(payload))
	}
}

func TestServerUDPGetport(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register MOUNT on UDP port 12049
	registry.Set(&xdr.Mapping{Prog: 100005, Vers: 3, Prot: 17, Port: 12049})

	args := encodeMappingArgs(100005, 3, 17, 0)
	msg := buildRPCCall(0x66666666, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
	reply := sendUDPRequest(t, srv.UDPAddr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0x66666666 {
		t.Errorf("XID mismatch: got 0x%x, want 0x66666666", xid)
	}
	if acceptStat != 0 {
		t.Errorf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
	}
	if len(payload) < 4 {
		t.Fatalf("payload too short: %d bytes", len(payload))
	}

	port := binary.BigEndian.Uint32(payload[0:4])
	if port != 12049 {
		t.Errorf("port: got %d, want 12049", port)
	}
}

// ============================================================================
// Lifecycle Tests
// ============================================================================

func TestServerGracefulShutdown(t *testing.T) {
	srv, _ := startTestServer(t)

	tcpAddr := srv.Addr()
	udpAddr := srv.UDPAddr()

	// Verify server is accepting connections
	conn, err := net.DialTimeout("tcp", tcpAddr, 2*time.Second)
	if err != nil {
		t.Fatalf("should connect before shutdown: %v", err)
	}
	_ = conn.Close()

	// Stop the server (closes listeners and waits for goroutines)
	srv.shutdownOnce.Do(func() {
		close(srv.shutdown)
		_ = srv.tcpListener.Close()
		_ = srv.udpConn.Close()
	})

	// Give a moment for shutdown to propagate
	time.Sleep(100 * time.Millisecond)

	// TCP listener should be closed
	_, err = net.DialTimeout("tcp", tcpAddr, 500*time.Millisecond)
	if err == nil {
		t.Error("TCP connection should fail after shutdown")
	}

	// UDP should be closed -- send should still work but no reply
	udpAddrResolved, _ := net.ResolveUDPAddr("udp", udpAddr)
	udpConn, err := net.DialUDP("udp", nil, udpAddrResolved)
	if err != nil {
		// Connection error expected on some platforms
		return
	}
	defer func() { _ = udpConn.Close() }()

	if err := udpConn.SetReadDeadline(time.Now().Add(500 * time.Millisecond)); err != nil {
		return
	}
	msg := buildRPCCall(0x77777777, types.ProgramPortmap, types.PortmapVersion2, types.ProcNull, nil)
	_, _ = udpConn.Write(msg)

	buf := make([]byte, 1024)
	_, err = udpConn.Read(buf)
	if err == nil {
		t.Error("UDP should not respond after shutdown")
	}
}

// ============================================================================
// Integration Tests: SET + GETPORT + UNSET workflow
// ============================================================================

func TestServerSetUnset(t *testing.T) {
	srv, _ := startTestServer(t)

	tcpAddr := srv.Addr()

	// Step 1: SET -- register a service (from localhost, so it should succeed)
	setArgs := encodeMappingArgs(100099, 1, 6, 9999)
	setMsg := buildRPCCall(0x00000001, types.ProgramPortmap, types.PortmapVersion2, types.ProcSet, setArgs)
	setReply := sendTCPRequest(t, tcpAddr, setMsg)

	_, acceptStat, payload := parseReplyHeader(setReply)
	if acceptStat != 0 {
		t.Fatalf("SET accept_stat: got %d, want 0", acceptStat)
	}
	if len(payload) < 4 {
		t.Fatalf("SET payload too short: %d bytes", len(payload))
	}
	setBool := binary.BigEndian.Uint32(payload[0:4])
	if setBool != 1 {
		t.Fatalf("SET result: got %d, want 1 (true)", setBool)
	}

	// Step 2: GETPORT -- verify it's registered
	getArgs := encodeMappingArgs(100099, 1, 6, 0)
	getMsg := buildRPCCall(0x00000002, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, getArgs)
	getReply := sendTCPRequest(t, tcpAddr, getMsg)

	_, acceptStat, payload = parseReplyHeader(getReply)
	if acceptStat != 0 {
		t.Fatalf("GETPORT accept_stat: got %d, want 0", acceptStat)
	}
	port := binary.BigEndian.Uint32(payload[0:4])
	if port != 9999 {
		t.Fatalf("GETPORT: got %d, want 9999", port)
	}

	// Step 3: UNSET -- remove the service
	unsetArgs := encodeMappingArgs(100099, 1, 6, 0) // port ignored per RFC 1057
	unsetMsg := buildRPCCall(0x00000003, types.ProgramPortmap, types.PortmapVersion2, types.ProcUnset, unsetArgs)
	unsetReply := sendTCPRequest(t, tcpAddr, unsetMsg)

	_, acceptStat, payload = parseReplyHeader(unsetReply)
	if acceptStat != 0 {
		t.Fatalf("UNSET accept_stat: got %d, want 0", acceptStat)
	}
	unsetBool := binary.BigEndian.Uint32(payload[0:4])
	if unsetBool != 1 {
		t.Fatalf("UNSET result: got %d, want 1 (true)", unsetBool)
	}

	// Step 4: GETPORT -- verify it's gone
	getMsg2 := buildRPCCall(0x00000004, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, getArgs)
	getReply2 := sendTCPRequest(t, tcpAddr, getMsg2)

	_, acceptStat, payload = parseReplyHeader(getReply2)
	if acceptStat != 0 {
		t.Fatalf("GETPORT2 accept_stat: got %d, want 0", acceptStat)
	}
	port = binary.BigEndian.Uint32(payload[0:4])
	if port != 0 {
		t.Fatalf("GETPORT2: got %d, want 0 (not registered)", port)
	}
}

// ============================================================================
// Edge Case Tests
// ============================================================================

func TestServerTCPUnknownProcedure(t *testing.T) {
	srv, _ := startTestServer(t)

	// Procedure 99 does not exist
	msg := buildRPCCall(0x88888888, types.ProgramPortmap, types.PortmapVersion2, 99, nil)
	reply := sendTCPRequest(t, srv.Addr(), msg)

	xid, acceptStat, _ := parseReplyHeader(reply)
	if xid != 0x88888888 {
		t.Errorf("XID mismatch: got 0x%x, want 0x88888888", xid)
	}
	if acceptStat != 3 { // PROC_UNAVAIL
		t.Errorf("accept_stat: got %d, want 3 (PROC_UNAVAIL)", acceptStat)
	}
}

func TestServerUDPDump(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register services via registry directly
	registry.RegisterDittoFSServices(12049)

	msg := buildRPCCall(0x99999999, types.ProgramPortmap, types.PortmapVersion2, types.ProcDump, nil)
	reply := sendUDPRequest(t, srv.UDPAddr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0x99999999 {
		t.Errorf("XID mismatch: got 0x%x, want 0x99999999", xid)
	}
	if acceptStat != 0 {
		t.Errorf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
	}

	// Count dump entries
	count := 0
	offset := 0
	for offset < len(payload) {
		if offset+4 > len(payload) {
			break
		}
		disc := binary.BigEndian.Uint32(payload[offset : offset+4])
		offset += 4
		if disc == 0 {
			break
		}
		if offset+16 > len(payload) {
			t.Fatal("truncated mapping entry")
		}
		offset += 16
		count++
	}

	// RegisterDittoFSServices registers 5 entries (TCP-only)
	if count != 5 {
		t.Errorf("DUMP entry count: got %d, want 5", count)
	}
}

func TestServerAddr(t *testing.T) {
	srv, _ := startTestServer(t)

	tcpAddr := srv.Addr()
	if tcpAddr == "" {
		t.Error("Addr() should return non-empty address")
	}

	udpAddr := srv.UDPAddr()
	if udpAddr == "" {
		t.Error("UDPAddr() should return non-empty address")
	}

	// Addr should contain a port number
	_, port, err := net.SplitHostPort(tcpAddr)
	if err != nil {
		t.Errorf("invalid TCP address format: %v", err)
	}
	if port == "0" {
		t.Error("TCP port should not be 0")
	}
}

func TestServerUDPVersionMismatch(t *testing.T) {
	srv, _ := startTestServer(t)

	msg := buildRPCCall(0xBBBBBBBB, types.ProgramPortmap, 99, types.ProcNull, nil)
	reply := sendUDPRequest(t, srv.UDPAddr(), msg)

	xid, acceptStat, payload := parseReplyHeader(reply)
	if xid != 0xBBBBBBBB {
		t.Errorf("XID mismatch: got 0x%x, want 0xBBBBBBBB", xid)
	}
	if acceptStat != 2 { // PROG_MISMATCH
		t.Errorf("accept_stat: got %d, want 2 (PROG_MISMATCH)", acceptStat)
	}

	if len(payload) < 8 {
		t.Fatalf("PROG_MISMATCH payload too short: %d bytes", len(payload))
	}
	low := binary.BigEndian.Uint32(payload[0:4])
	high := binary.BigEndian.Uint32(payload[4:8])
	if low != 2 || high != 2 {
		t.Errorf("version range: got [%d, %d], want [2, 2]", low, high)
	}
}

func TestServerMultipleTCPClients(t *testing.T) {
	srv, registry := startTestServer(t)

	registry.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})

	// Send multiple concurrent TCP requests
	const numClients = 5
	results := make(chan error, numClients)

	for i := range numClients {
		go func(idx int) {
			args := encodeMappingArgs(100003, 3, 6, 0)
			msg := buildRPCCall(uint32(0xF0000000+idx), types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)

			conn, err := net.DialTimeout("tcp", srv.Addr(), 2*time.Second)
			if err != nil {
				results <- fmt.Errorf("client %d dial: %w", idx, err)
				return
			}
			defer func() { _ = conn.Close() }()

			conn.SetDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck

			framedMsg := wrapWithRecordMarking(msg)
			if _, err := conn.Write(framedMsg); err != nil {
				results <- fmt.Errorf("client %d write: %w", idx, err)
				return
			}

			var headerBuf [4]byte
			if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
				results <- fmt.Errorf("client %d read header: %w", idx, err)
				return
			}
			headerVal := binary.BigEndian.Uint32(headerBuf[:])
			replyLen := headerVal & 0x7FFFFFFF

			replyBuf := make([]byte, replyLen)
			if _, err := io.ReadFull(conn, replyBuf); err != nil {
				results <- fmt.Errorf("client %d read reply: %w", idx, err)
				return
			}

			_, acceptStat, payload := parseReplyHeader(replyBuf)
			if acceptStat != 0 {
				results <- fmt.Errorf("client %d: accept_stat=%d", idx, acceptStat)
				return
			}
			if len(payload) < 4 {
				results <- fmt.Errorf("client %d: payload too short", idx)
				return
			}
			port := binary.BigEndian.Uint32(payload[0:4])
			if port != 12049 {
				results <- fmt.Errorf("client %d: port=%d, want 12049", idx, port)
				return
			}

			results <- nil
		}(i)
	}

	for range numClients {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
}
