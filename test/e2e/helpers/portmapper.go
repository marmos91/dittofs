//go:build e2e

package helpers

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Well-known RPC program numbers.
const (
	ProgPortmapper uint32 = 100000
	ProgNFS        uint32 = 100003
	ProgMount      uint32 = 100005
	ProgNLM        uint32 = 100021
	ProgNSM        uint32 = 100024
)

// PortmapEntry represents a single portmap mapping (prog, vers, prot, port).
type PortmapEntry struct {
	Program  uint32
	Version  uint32
	Protocol uint32 // 6=TCP, 17=UDP
	Port     uint32
}

// ProtoName returns "tcp" or "udp" for the protocol field.
func (e PortmapEntry) ProtoName() string {
	switch e.Protocol {
	case 6:
		return "tcp"
	case 17:
		return "udp"
	default:
		return fmt.Sprintf("proto-%d", e.Protocol)
	}
}

// SkipIfNoRpcinfo skips the test if rpcinfo is not available on the system.
func SkipIfNoRpcinfo(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rpcinfo"); err != nil {
		t.Skip("rpcinfo not available, skipping portmapper test")
	}
}

// SkipIfPortBusy skips the test if the given TCP port is already in use.
func SkipIfPortBusy(t *testing.T, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Skipf("port %d already in use, skipping portmapper test", port)
	}
	_ = ln.Close()
}

// RpcinfoProbe uses `rpcinfo -n <port> -t <host> <prognum> [versnum]` to check
// if a specific RPC program is reachable at the given port.
// Returns nil if the program responds, error otherwise.
func RpcinfoProbe(t *testing.T, host string, port int, prognum uint32, versnum uint32) error {
	t.Helper()

	args := []string{"-n", fmt.Sprintf("%d", port), "-t", host,
		fmt.Sprintf("%d", prognum), fmt.Sprintf("%d", versnum)}
	cmd := exec.Command("rpcinfo", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rpcinfo probe failed: %v\nOutput: %s", err, string(output))
	}
	return nil
}

// IsRpcinfoSystemError returns true if the error indicates a system-level
// rpcinfo failure (e.g., "Broken pipe" from macOS when no system portmapper
// is running on port 111). These errors mean rpcinfo cannot function on this
// system, not that our portmapper is broken.
func IsRpcinfoSystemError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "Broken pipe") ||
		strings.Contains(msg, "Connection refused") ||
		strings.Contains(msg, "Unable to send")
}

// HasSystemRpcbind returns true if a system portmapper is listening on port 111.
// When a system rpcbind is running, the rpcinfo tool will query it first before
// probing the target port, which causes our tests to fail even though our
// embedded portmapper works correctly.
func HasSystemRpcbind() bool {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:111", time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// SkipIfSystemRpcbind skips the test if a system rpcbind is running on port 111.
// This is used for tests that rely on rpcinfo behavior, which queries the system
// portmapper first. For full portmapper testing without system rpcbind, use:
//
//	./test/integration/portmap/run.sh
func SkipIfSystemRpcbind(t *testing.T) {
	t.Helper()
	if HasSystemRpcbind() {
		t.Skip("system rpcbind detected on port 111; rpcinfo queries system portmapper first. " +
			"For full portmapper tests, run: ./test/integration/portmap/run.sh")
	}
}

// PortmapNull sends a NULL (ping) RPC call to the portmapper and checks the response.
func PortmapNull(t *testing.T, host string, port int) error {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Build RPC CALL for portmap NULL: prog=100000, vers=2, proc=0, no args
	rpcCall := buildPortmapRPCCall(0, 100000, 2, 0, nil)

	fragHeader := make([]byte, 4)
	binary.BigEndian.PutUint32(fragHeader, uint32(len(rpcCall))|0x80000000)

	if _, err := conn.Write(append(fragHeader, rpcCall...)); err != nil {
		return fmt.Errorf("write: %w", err)
	}

	respHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, respHeader); err != nil {
		return fmt.Errorf("read header: %w", err)
	}
	fragLen := binary.BigEndian.Uint32(respHeader) & 0x7FFFFFFF

	respBody := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, respBody); err != nil {
		return fmt.Errorf("read body: %w", err)
	}

	if len(respBody) < 24 {
		return fmt.Errorf("response too short: %d bytes", len(respBody))
	}

	acceptStat := binary.BigEndian.Uint32(respBody[20:24])
	if acceptStat != 0 {
		return fmt.Errorf("NULL rejected: accept_stat=%d", acceptStat)
	}

	return nil
}

// PortmapDump connects to a portmapper at host:port via TCP and executes a
// DUMP (procedure 4) call, returning all registered mappings.
// This is a pure Go implementation that works on any port, unlike `rpcinfo -p`
// which always connects to port 111.
func PortmapDump(t *testing.T, host string, port int) []PortmapEntry {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to portmapper at %s:%d: %v", host, port, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Build RPC CALL for portmap DUMP: prog=100000, vers=2, proc=4, no args
	rpcCall := buildPortmapRPCCall(1, 100000, 2, 4, nil)

	// TCP record marking: 4-byte header with last-fragment bit set
	fragHeader := make([]byte, 4)
	binary.BigEndian.PutUint32(fragHeader, uint32(len(rpcCall))|0x80000000)

	if _, err := conn.Write(append(fragHeader, rpcCall...)); err != nil {
		t.Fatalf("failed to send DUMP request: %v", err)
	}

	// Read response: 4-byte fragment header + reply
	respHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, respHeader); err != nil {
		t.Fatalf("failed to read response header: %v", err)
	}
	fragLen := binary.BigEndian.Uint32(respHeader) & 0x7FFFFFFF

	respBody := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, respBody); err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	// Parse RPC reply: skip standard reply header (24 bytes minimum)
	// xid(4) + msg_type=1(4) + reply_stat=0(4) + verf_flavor(4) + verf_len(4) + accept_stat=0(4)
	if len(respBody) < 24 {
		t.Fatalf("response too short: %d bytes", len(respBody))
	}

	// Check accept_stat
	acceptStat := binary.BigEndian.Uint32(respBody[20:24])
	if acceptStat != 0 {
		t.Fatalf("RPC DUMP rejected: accept_stat=%d", acceptStat)
	}

	// Parse DUMP response: XDR optional-data linked list
	data := respBody[24:]
	return parseDumpResponse(t, data)
}

// PortmapGetPort connects to a portmapper at host:port and looks up the port
// for a given program/version/protocol tuple.
func PortmapGetPort(t *testing.T, host string, port int, prog, vers, prot uint32) uint32 {
	t.Helper()

	conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 2*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to portmapper at %s:%d: %v", host, port, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Build GETPORT args: mapping struct (prog, vers, prot, port=0)
	args := make([]byte, 16)
	binary.BigEndian.PutUint32(args[0:4], prog)
	binary.BigEndian.PutUint32(args[4:8], vers)
	binary.BigEndian.PutUint32(args[8:12], prot)
	// port field is 0 (unused in GETPORT request)

	rpcCall := buildPortmapRPCCall(2, 100000, 2, 3, args)

	fragHeader := make([]byte, 4)
	binary.BigEndian.PutUint32(fragHeader, uint32(len(rpcCall))|0x80000000)

	if _, err := conn.Write(append(fragHeader, rpcCall...)); err != nil {
		t.Fatalf("failed to send GETPORT request: %v", err)
	}

	respHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, respHeader); err != nil {
		t.Fatalf("failed to read response header: %v", err)
	}
	fragLen := binary.BigEndian.Uint32(respHeader) & 0x7FFFFFFF

	respBody := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, respBody); err != nil {
		t.Fatalf("failed to read response body: %v", err)
	}

	if len(respBody) < 28 {
		t.Fatalf("GETPORT response too short: %d bytes", len(respBody))
	}

	acceptStat := binary.BigEndian.Uint32(respBody[20:24])
	if acceptStat != 0 {
		t.Fatalf("RPC GETPORT rejected: accept_stat=%d", acceptStat)
	}

	return binary.BigEndian.Uint32(respBody[24:28])
}

// HasProgram checks if any entry matches the given program number.
func HasProgram(entries []PortmapEntry, program uint32) bool {
	for _, e := range entries {
		if e.Program == program {
			return true
		}
	}
	return false
}

// HasProgramVersion checks if any entry matches the given program and version.
func HasProgramVersion(entries []PortmapEntry, program, version uint32) bool {
	for _, e := range entries {
		if e.Program == program && e.Version == version {
			return true
		}
	}
	return false
}

// buildPortmapRPCCall constructs a raw RPC CALL message.
func buildPortmapRPCCall(xid, prog, vers, proc uint32, args []byte) []byte {
	header := make([]byte, 40)
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
		return append(header, args...)
	}
	return header
}

// parseDumpResponse parses an XDR optional-data linked list of portmap mappings.
func parseDumpResponse(t *testing.T, data []byte) []PortmapEntry {
	t.Helper()

	var entries []PortmapEntry
	offset := 0

	for offset+4 <= len(data) {
		valueFollows := binary.BigEndian.Uint32(data[offset : offset+4])
		offset += 4

		if valueFollows == 0 {
			break // End of list
		}

		if offset+16 > len(data) {
			t.Fatalf("truncated DUMP entry at offset %d", offset)
		}

		entry := PortmapEntry{
			Program:  binary.BigEndian.Uint32(data[offset : offset+4]),
			Version:  binary.BigEndian.Uint32(data[offset+4 : offset+8]),
			Protocol: binary.BigEndian.Uint32(data[offset+8 : offset+12]),
			Port:     binary.BigEndian.Uint32(data[offset+12 : offset+16]),
		}
		entries = append(entries, entry)
		offset += 16
	}

	return entries
}
