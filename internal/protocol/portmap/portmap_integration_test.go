package portmap

import (
	"encoding/binary"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/protocol/portmap/types"
	"github.com/marmos91/dittofs/internal/protocol/portmap/xdr"
)

// ============================================================================
// Integration Test Helpers
// ============================================================================

// buildRPCCallMsg constructs a complete RPC call message for integration tests.
//
// Wire format:
//
//	xid(4) + msg_type=0(4) + rpc_vers=2(4) + prog(4) + vers(4) + proc(4)
//	+ cred_flavor=0(4) + cred_len=0(4) + verf_flavor=0(4) + verf_len=0(4)
//	+ [procedure args]
func buildRPCCallMsg(xid, prog, vers, proc uint32, args []byte) []byte {
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

// sendTCPRPCMsg sends a record-marked TCP RPC request and reads the reply body.
func sendTCPRPCMsg(t *testing.T, addr string, callBody []byte) []byte {
	t.Helper()

	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatalf("sendTCPRPC: dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("sendTCPRPC: set deadline: %v", err)
	}

	// Write with record marking header (last-fragment bit set)
	header := make([]byte, 4)
	binary.BigEndian.PutUint32(header, 0x80000000|uint32(len(callBody)))
	if _, err := conn.Write(header); err != nil {
		t.Fatalf("sendTCPRPC: write header: %v", err)
	}
	if _, err := conn.Write(callBody); err != nil {
		t.Fatalf("sendTCPRPC: write body: %v", err)
	}

	// Read reply fragment header
	var replyHeader [4]byte
	if _, err := readFull(conn, replyHeader[:]); err != nil {
		t.Fatalf("sendTCPRPC: read reply header: %v", err)
	}
	replyLen := binary.BigEndian.Uint32(replyHeader[:]) & 0x7FFFFFFF

	// Read reply body
	replyBody := make([]byte, replyLen)
	if _, err := readFull(conn, replyBody); err != nil {
		t.Fatalf("sendTCPRPC: read reply body: %v", err)
	}

	return replyBody
}

// sendUDPRPCMsg sends a raw UDP RPC request and reads the reply.
func sendUDPRPCMsg(t *testing.T, addr string, callBody []byte) []byte {
	t.Helper()

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		t.Fatalf("sendUDPRPC: resolve %s: %v", addr, err)
	}

	conn, err := net.DialUDP("udp", nil, udpAddr)
	if err != nil {
		t.Fatalf("sendUDPRPC: dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("sendUDPRPC: set deadline: %v", err)
	}

	// Send raw (no record marking for UDP)
	if _, err := conn.Write(callBody); err != nil {
		t.Fatalf("sendUDPRPC: write: %v", err)
	}

	// Read reply
	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("sendUDPRPC: read reply: %v", err)
	}

	return buf[:n]
}

// readFull reads exactly len(buf) bytes from conn.
func readFull(conn net.Conn, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := conn.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// parseRPCReply extracts the accept_stat and reply data from an RPC reply body.
//
// Reply body format:
//
//	xid(4) + msg_type(4) + reply_stat(4) + verf_flavor(4) + verf_len(4) + [verf_body] + accept_stat(4) + [data]
func parseRPCReply(data []byte) (acceptStat uint32, replyData []byte, err error) {
	if len(data) < 24 {
		return 0, nil, fmt.Errorf("reply too short: %d bytes", len(data))
	}

	// xid(4) + msg_type(4) + reply_stat(4) = 12 bytes
	// verf_flavor(4) + verf_len(4) = 8 bytes -> offset 20
	verfLen := binary.BigEndian.Uint32(data[16:20])

	// Skip past verifier body
	acceptOffset := 20 + verfLen
	if uint32(len(data)) < acceptOffset+4 {
		return 0, nil, fmt.Errorf("reply truncated at accept_stat: %d bytes", len(data))
	}

	acceptStat = binary.BigEndian.Uint32(data[acceptOffset : acceptOffset+4])

	if uint32(len(data)) > acceptOffset+4 {
		replyData = data[acceptOffset+4:]
	}

	return acceptStat, replyData, nil
}

// encodeGetportArgs creates XDR-encoded GETPORT arguments.
func encodeGetportArgs(prog, vers, prot uint32) []byte {
	return xdr.EncodeMapping(&xdr.Mapping{
		Prog: prog, Vers: vers, Prot: prot, Port: 0,
	})
}

// ============================================================================
// TestPortmapperIntegrationTCP: Full TCP integration test
// ============================================================================

func TestPortmapperIntegrationTCP(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register NFS on TCP port 12049
	registry.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: types.ProtoTCP, Port: 12049})

	tcpAddr := srv.Addr()

	t.Run("GETPORT_found", func(t *testing.T) {
		args := encodeGetportArgs(100003, 3, types.ProtoTCP)
		msg := buildRPCCallMsg(0x10000001, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
		reply := sendTCPRPCMsg(t, tcpAddr, msg)

		acceptStat, data, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 0 {
			t.Fatalf("accept_stat: got %d, want 0 (SUCCESS)", acceptStat)
		}
		if len(data) < 4 {
			t.Fatalf("reply data too short: %d bytes", len(data))
		}
		port := binary.BigEndian.Uint32(data[0:4])
		if port != 12049 {
			t.Errorf("GETPORT: got port %d, want 12049", port)
		}
	})

	t.Run("GETPORT_not_found", func(t *testing.T) {
		args := encodeGetportArgs(999999, 1, types.ProtoTCP)
		msg := buildRPCCallMsg(0x10000002, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
		reply := sendTCPRPCMsg(t, tcpAddr, msg)

		acceptStat, data, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 0 {
			t.Fatalf("accept_stat: got %d, want 0", acceptStat)
		}
		port := binary.BigEndian.Uint32(data[0:4])
		if port != 0 {
			t.Errorf("GETPORT unregistered: got port %d, want 0", port)
		}
	})

	t.Run("DUMP", func(t *testing.T) {
		msg := buildRPCCallMsg(0x10000003, types.ProgramPortmap, types.PortmapVersion2, types.ProcDump, nil)
		reply := sendTCPRPCMsg(t, tcpAddr, msg)

		acceptStat, data, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 0 {
			t.Fatalf("accept_stat: got %d, want 0", acceptStat)
		}

		// Parse linked list and count entries
		count := 0
		foundNFS := false
		offset := 0
		for offset < len(data) {
			if offset+4 > len(data) {
				break
			}
			disc := binary.BigEndian.Uint32(data[offset : offset+4])
			offset += 4
			if disc == 0 {
				break // End of list
			}
			if offset+16 > len(data) {
				t.Fatal("truncated mapping entry in DUMP")
			}
			prog := binary.BigEndian.Uint32(data[offset : offset+4])
			vers := binary.BigEndian.Uint32(data[offset+4 : offset+8])
			prot := binary.BigEndian.Uint32(data[offset+8 : offset+12])
			port := binary.BigEndian.Uint32(data[offset+12 : offset+16])
			offset += 16
			count++

			if prog == 100003 && vers == 3 && prot == types.ProtoTCP && port == 12049 {
				foundNFS = true
			}
		}

		if count != 1 {
			t.Errorf("DUMP entry count: got %d, want 1", count)
		}
		if !foundNFS {
			t.Error("DUMP: NFS v3 TCP mapping not found")
		}
	})

	t.Run("NULL", func(t *testing.T) {
		msg := buildRPCCallMsg(0x10000004, types.ProgramPortmap, types.PortmapVersion2, types.ProcNull, nil)
		reply := sendTCPRPCMsg(t, tcpAddr, msg)

		acceptStat, data, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 0 {
			t.Fatalf("accept_stat: got %d, want 0", acceptStat)
		}
		if len(data) != 0 {
			t.Errorf("NULL should return empty data, got %d bytes", len(data))
		}
	})

	t.Run("CALLIT_returns_PROC_UNAVAIL", func(t *testing.T) {
		msg := buildRPCCallMsg(0x10000005, types.ProgramPortmap, types.PortmapVersion2, types.ProcCallit, nil)
		reply := sendTCPRPCMsg(t, tcpAddr, msg)

		acceptStat, _, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 3 { // PROC_UNAVAIL
			t.Errorf("CALLIT accept_stat: got %d, want 3 (PROC_UNAVAIL)", acceptStat)
		}
	})
}

// ============================================================================
// TestPortmapperIntegrationUDP: Full UDP integration test
// ============================================================================

func TestPortmapperIntegrationUDP(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register MOUNT on UDP port 12049
	registry.Set(&xdr.Mapping{Prog: 100005, Vers: 3, Prot: types.ProtoUDP, Port: 12049})

	udpAddr := srv.UDPAddr()

	t.Run("GETPORT_over_UDP", func(t *testing.T) {
		args := encodeGetportArgs(100005, 3, types.ProtoUDP)
		msg := buildRPCCallMsg(0x20000001, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
		reply := sendUDPRPCMsg(t, udpAddr, msg)

		acceptStat, data, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 0 {
			t.Fatalf("accept_stat: got %d, want 0", acceptStat)
		}
		if len(data) < 4 {
			t.Fatalf("reply data too short: %d bytes", len(data))
		}
		port := binary.BigEndian.Uint32(data[0:4])
		if port != 12049 {
			t.Errorf("GETPORT UDP: got port %d, want 12049", port)
		}
	})

	t.Run("NULL_over_UDP", func(t *testing.T) {
		msg := buildRPCCallMsg(0x20000002, types.ProgramPortmap, types.PortmapVersion2, types.ProcNull, nil)
		reply := sendUDPRPCMsg(t, udpAddr, msg)

		acceptStat, data, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 0 {
			t.Fatalf("accept_stat: got %d, want 0", acceptStat)
		}
		if len(data) != 0 {
			t.Errorf("NULL should return empty data, got %d bytes", len(data))
		}
	})
}

// ============================================================================
// TestPortmapperFullServiceRegistry: Verify RegisterDittoFSServices
// ============================================================================

func TestPortmapperFullServiceRegistry(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register all DittoFS services on port 12049
	registry.RegisterDittoFSServices(12049)

	tcpAddr := srv.Addr()

	// Define all expected service registrations
	type serviceQuery struct {
		name string
		prog uint32
		vers uint32
		prot uint32
	}

	// DittoFS NFS adapter is TCP-only, so only TCP mappings are registered
	services := []serviceQuery{
		{"NFS_v3_TCP", 100003, 3, types.ProtoTCP},
		{"NFS_v4_TCP", 100003, 4, types.ProtoTCP},
		{"MOUNT_v3_TCP", 100005, 3, types.ProtoTCP},
		{"NLM_v4_TCP", 100021, 4, types.ProtoTCP},
		{"NSM_v1_TCP", 100024, 1, types.ProtoTCP},
	}

	for _, svc := range services {
		t.Run(svc.name, func(t *testing.T) {
			args := encodeGetportArgs(svc.prog, svc.vers, svc.prot)
			msg := buildRPCCallMsg(0x30000000, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)
			reply := sendTCPRPCMsg(t, tcpAddr, msg)

			acceptStat, data, err := parseRPCReply(reply)
			if err != nil {
				t.Fatalf("parseRPCReply: %v", err)
			}
			if acceptStat != 0 {
				t.Fatalf("accept_stat: got %d, want 0", acceptStat)
			}
			if len(data) < 4 {
				t.Fatalf("reply data too short: %d bytes", len(data))
			}
			port := binary.BigEndian.Uint32(data[0:4])
			if port != 12049 {
				t.Errorf("GETPORT %s: got port %d, want 12049", svc.name, port)
			}
		})
	}

	// Verify DUMP returns exactly 5 entries (TCP-only)
	t.Run("DUMP_5_entries", func(t *testing.T) {
		msg := buildRPCCallMsg(0x30000001, types.ProgramPortmap, types.PortmapVersion2, types.ProcDump, nil)
		reply := sendTCPRPCMsg(t, tcpAddr, msg)

		acceptStat, data, err := parseRPCReply(reply)
		if err != nil {
			t.Fatalf("parseRPCReply: %v", err)
		}
		if acceptStat != 0 {
			t.Fatalf("accept_stat: got %d, want 0", acceptStat)
		}

		count := 0
		offset := 0
		for offset < len(data) {
			if offset+4 > len(data) {
				break
			}
			disc := binary.BigEndian.Uint32(data[offset : offset+4])
			offset += 4
			if disc == 0 {
				break
			}
			if offset+16 > len(data) {
				t.Fatal("truncated mapping entry")
			}
			offset += 16
			count++
		}

		if count != 5 {
			t.Errorf("DUMP count: got %d, want 5", count)
		}
	})
}

// ============================================================================
// TestPortmapperSetUnsetFlow: Dynamic registration and deregistration
// ============================================================================

func TestPortmapperSetUnsetFlow(t *testing.T) {
	srv, _ := startTestServer(t)

	tcpAddr := srv.Addr()

	// Step 1: SET -- register a custom service (prog=200000, vers=1, prot=TCP, port=9999)
	setArgs := xdr.EncodeMapping(&xdr.Mapping{Prog: 200000, Vers: 1, Prot: types.ProtoTCP, Port: 9999})
	setMsg := buildRPCCallMsg(0x40000001, types.ProgramPortmap, types.PortmapVersion2, types.ProcSet, setArgs)
	setReply := sendTCPRPCMsg(t, tcpAddr, setMsg)

	acceptStat, data, err := parseRPCReply(setReply)
	if err != nil {
		t.Fatalf("SET parseRPCReply: %v", err)
	}
	if acceptStat != 0 {
		t.Fatalf("SET accept_stat: got %d, want 0", acceptStat)
	}
	if len(data) < 4 {
		t.Fatalf("SET data too short: %d bytes", len(data))
	}
	setBool := binary.BigEndian.Uint32(data[0:4])
	if setBool != 1 {
		t.Fatalf("SET result: got %d, want 1 (true)", setBool)
	}

	// Step 2: GETPORT -- verify registered
	getArgs := encodeGetportArgs(200000, 1, types.ProtoTCP)
	getMsg := buildRPCCallMsg(0x40000002, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, getArgs)
	getReply := sendTCPRPCMsg(t, tcpAddr, getMsg)

	acceptStat, data, err = parseRPCReply(getReply)
	if err != nil {
		t.Fatalf("GETPORT parseRPCReply: %v", err)
	}
	if acceptStat != 0 {
		t.Fatalf("GETPORT accept_stat: got %d, want 0", acceptStat)
	}
	port := binary.BigEndian.Uint32(data[0:4])
	if port != 9999 {
		t.Fatalf("GETPORT: got %d, want 9999", port)
	}

	// Step 3: UNSET -- remove the service
	unsetArgs := xdr.EncodeMapping(&xdr.Mapping{Prog: 200000, Vers: 1, Prot: types.ProtoTCP, Port: 0})
	unsetMsg := buildRPCCallMsg(0x40000003, types.ProgramPortmap, types.PortmapVersion2, types.ProcUnset, unsetArgs)
	unsetReply := sendTCPRPCMsg(t, tcpAddr, unsetMsg)

	acceptStat, data, err = parseRPCReply(unsetReply)
	if err != nil {
		t.Fatalf("UNSET parseRPCReply: %v", err)
	}
	if acceptStat != 0 {
		t.Fatalf("UNSET accept_stat: got %d, want 0", acceptStat)
	}
	unsetBool := binary.BigEndian.Uint32(data[0:4])
	if unsetBool != 1 {
		t.Fatalf("UNSET result: got %d, want 1 (true)", unsetBool)
	}

	// Step 4: GETPORT -- verify removed
	getMsg2 := buildRPCCallMsg(0x40000004, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, getArgs)
	getReply2 := sendTCPRPCMsg(t, tcpAddr, getMsg2)

	acceptStat, data, err = parseRPCReply(getReply2)
	if err != nil {
		t.Fatalf("GETPORT2 parseRPCReply: %v", err)
	}
	if acceptStat != 0 {
		t.Fatalf("GETPORT2 accept_stat: got %d, want 0", acceptStat)
	}
	port = binary.BigEndian.Uint32(data[0:4])
	if port != 0 {
		t.Fatalf("GETPORT2: got %d, want 0 (not registered)", port)
	}
}

// ============================================================================
// TestPortmapperConcurrentQueries: Verify thread safety under load
// ============================================================================

func TestPortmapperConcurrentQueries(t *testing.T) {
	srv, registry := startTestServer(t)

	// Register NFS service
	registry.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: types.ProtoTCP, Port: 12049})

	tcpAddr := srv.Addr()

	const numGoroutines = 10
	const queriesPerGoroutine = 100

	var wg sync.WaitGroup
	errors := make(chan error, numGoroutines*queriesPerGoroutine)

	for g := 0; g < numGoroutines; g++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for i := 0; i < queriesPerGoroutine; i++ {
				xid := uint32(goroutineID*queriesPerGoroutine + i + 0x50000000)
				args := encodeGetportArgs(100003, 3, types.ProtoTCP)
				msg := buildRPCCallMsg(xid, types.ProgramPortmap, types.PortmapVersion2, types.ProcGetport, args)

				conn, err := net.DialTimeout("tcp", tcpAddr, 2*time.Second)
				if err != nil {
					errors <- fmt.Errorf("goroutine %d query %d: dial: %w", goroutineID, i, err)
					continue
				}

				if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
					_ = conn.Close()
					errors <- fmt.Errorf("goroutine %d query %d: deadline: %w", goroutineID, i, err)
					continue
				}

				// Send with record marking
				header := make([]byte, 4)
				binary.BigEndian.PutUint32(header, 0x80000000|uint32(len(msg)))
				if _, err := conn.Write(header); err != nil {
					_ = conn.Close()
					errors <- fmt.Errorf("goroutine %d query %d: write header: %w", goroutineID, i, err)
					continue
				}
				if _, err := conn.Write(msg); err != nil {
					_ = conn.Close()
					errors <- fmt.Errorf("goroutine %d query %d: write body: %w", goroutineID, i, err)
					continue
				}

				// Read reply header
				var replyHeader [4]byte
				if _, err := readFull(conn, replyHeader[:]); err != nil {
					_ = conn.Close()
					errors <- fmt.Errorf("goroutine %d query %d: read reply header: %w", goroutineID, i, err)
					continue
				}
				replyLen := binary.BigEndian.Uint32(replyHeader[:]) & 0x7FFFFFFF

				// Read reply body
				replyBody := make([]byte, replyLen)
				if _, err := readFull(conn, replyBody); err != nil {
					_ = conn.Close()
					errors <- fmt.Errorf("goroutine %d query %d: read reply body: %w", goroutineID, i, err)
					continue
				}

				_ = conn.Close()

				// Parse and verify
				acceptStat, data, err := parseRPCReply(replyBody)
				if err != nil {
					errors <- fmt.Errorf("goroutine %d query %d: parse: %w", goroutineID, i, err)
					continue
				}
				if acceptStat != 0 {
					errors <- fmt.Errorf("goroutine %d query %d: accept_stat=%d", goroutineID, i, acceptStat)
					continue
				}
				if len(data) < 4 {
					errors <- fmt.Errorf("goroutine %d query %d: data too short", goroutineID, i)
					continue
				}
				port := binary.BigEndian.Uint32(data[0:4])
				if port != 12049 {
					errors <- fmt.Errorf("goroutine %d query %d: port=%d, want 12049", goroutineID, i, port)
				}
			}
		}(g)
	}

	wg.Wait()
	close(errors)

	var errCount int
	for err := range errors {
		t.Error(err)
		errCount++
		if errCount > 10 {
			t.Fatal("Too many errors, stopping")
		}
	}
}
