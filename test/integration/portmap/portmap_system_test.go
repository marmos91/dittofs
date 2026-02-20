//go:build portmap_system

// Package portmap_test provides integration tests for portmapper registration
// with the system rpcbind.
//
// These tests verify that DittoFS can register its services with the system
// portmapper (rpcbind) using the SET procedure, and that standard tools like
// rpcinfo can discover the registered services.
//
// Run with: go test -tags=portmap_system -v ./test/integration/portmap/
// Requires: System rpcbind running on port 111, no existing NFS server registered
package portmap_test

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Well-known RPC program numbers.
const (
	ProgPortmapper uint32 = 100000
	ProgNFS        uint32 = 100003
	ProgMount      uint32 = 100005

	ProtoTCP uint32 = 6
	ProtoUDP uint32 = 17

	ProcNull    uint32 = 0
	ProcSet     uint32 = 1
	ProcUnset   uint32 = 2
	ProcGetport uint32 = 3
	ProcDump    uint32 = 4

	SystemPortmapperPort = 111
)

// TestSystemRpcbindRegistration tests registering DittoFS services with the
// system rpcbind. This validates that our portmap client code (SET/UNSET)
// correctly speaks the portmap protocol.
func TestSystemRpcbindRegistration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping system rpcbind test in short mode")
	}

	// Skip if no system rpcbind
	skipIfNoSystemRpcbind(t)

	// Skip if NFS already registered (avoid conflicts with real NFS server)
	skipIfNFSRegistered(t)

	// Use an ephemeral port for our "fake" NFS service
	testPort := uint32(findFreePort(t))

	// Register cleanup FIRST - runs even on panic/t.FailNow
	t.Cleanup(func() {
		// Best-effort cleanup - don't fail test if cleanup fails
		_, _ = portmapUnset(ProgNFS, 3, ProtoTCP)
		_, _ = portmapUnset(ProgMount, 3, ProtoTCP)
	})

	t.Run("SET registers service", func(t *testing.T) {
		// Register NFS v3 TCP
		ok, err := portmapSet(ProgNFS, 3, ProtoTCP, testPort)
		require.NoError(t, err, "SET RPC should succeed")
		assert.True(t, ok, "SET should return true for new registration")

		// Register MOUNT v3 TCP
		ok, err = portmapSet(ProgMount, 3, ProtoTCP, testPort)
		require.NoError(t, err, "SET RPC should succeed")
		assert.True(t, ok, "SET should return true for new registration")
	})

	t.Run("GETPORT returns registered port", func(t *testing.T) {
		port, err := portmapGetPort(ProgNFS, 3, ProtoTCP)
		require.NoError(t, err, "GETPORT should succeed")
		assert.Equal(t, testPort, port, "GETPORT should return registered port")

		port, err = portmapGetPort(ProgMount, 3, ProtoTCP)
		require.NoError(t, err, "GETPORT should succeed")
		assert.Equal(t, testPort, port, "GETPORT should return registered port")
	})

	t.Run("DUMP includes registered services", func(t *testing.T) {
		entries, err := portmapDump()
		require.NoError(t, err, "DUMP should succeed")

		hasNFS := false
		hasMount := false
		for _, e := range entries {
			if e.Program == ProgNFS && e.Version == 3 && e.Protocol == ProtoTCP {
				hasNFS = true
				assert.Equal(t, testPort, e.Port)
			}
			if e.Program == ProgMount && e.Version == 3 && e.Protocol == ProtoTCP {
				hasMount = true
				assert.Equal(t, testPort, e.Port)
			}
		}
		assert.True(t, hasNFS, "DUMP should include NFS registration")
		assert.True(t, hasMount, "DUMP should include MOUNT registration")
	})

	t.Run("rpcinfo sees registered services", func(t *testing.T) {
		if _, err := exec.LookPath("rpcinfo"); err != nil {
			t.Skip("rpcinfo not available")
		}

		// rpcinfo -p should show our registrations
		cmd := exec.Command("rpcinfo", "-p", "127.0.0.1")
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "rpcinfo -p should succeed")

		outputStr := string(output)
		assert.Contains(t, outputStr, "100003", "rpcinfo should show NFS program")
		assert.Contains(t, outputStr, "100005", "rpcinfo should show MOUNT program")
		t.Logf("rpcinfo output:\n%s", outputStr)
	})

	t.Run("UNSET removes registration", func(t *testing.T) {
		ok, err := portmapUnset(ProgNFS, 3, ProtoTCP)
		require.NoError(t, err, "UNSET RPC should succeed")
		assert.True(t, ok, "UNSET should return true for existing registration")

		// Verify it's gone
		port, err := portmapGetPort(ProgNFS, 3, ProtoTCP)
		require.NoError(t, err, "GETPORT should succeed")
		assert.Equal(t, uint32(0), port, "GETPORT should return 0 after UNSET")
	})

	t.Run("duplicate SET returns false", func(t *testing.T) {
		// MOUNT is still registered from earlier
		ok, err := portmapSet(ProgMount, 3, ProtoTCP, testPort+1)
		require.NoError(t, err, "SET RPC should succeed")
		assert.False(t, ok, "SET should return false for duplicate registration")
	})
}

// skipIfNoSystemRpcbind skips the test if no system rpcbind is listening on port 111.
func skipIfNoSystemRpcbind(t *testing.T) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", "127.0.0.1:111", time.Second)
	if err != nil {
		t.Skipf("no system rpcbind on port 111: %v", err)
	}
	_ = conn.Close()
}

// skipIfNFSRegistered skips the test if NFS is already registered with system rpcbind.
func skipIfNFSRegistered(t *testing.T) {
	t.Helper()
	port, err := portmapGetPort(ProgNFS, 3, ProtoTCP)
	if err != nil {
		t.Skipf("cannot query system rpcbind: %v", err)
	}
	if port != 0 {
		t.Skipf("NFS already registered with system rpcbind (port %d), skipping to avoid conflict", port)
	}
}

// findFreePort finds an available TCP port.
func findFreePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "should find free port")
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// PortmapEntry represents a single portmap mapping.
type PortmapEntry struct {
	Program  uint32
	Version  uint32
	Protocol uint32
	Port     uint32
}

// portmapSet registers a program/version/protocol/port with system rpcbind.
// Returns (true, nil) on success, (false, nil) if registration already exists.
func portmapSet(prog, vers, prot, port uint32) (bool, error) {
	args := make([]byte, 16)
	binary.BigEndian.PutUint32(args[0:4], prog)
	binary.BigEndian.PutUint32(args[4:8], vers)
	binary.BigEndian.PutUint32(args[8:12], prot)
	binary.BigEndian.PutUint32(args[12:16], port)

	resp, err := portmapCall(ProcSet, args)
	if err != nil {
		return false, err
	}
	if len(resp) < 4 {
		return false, fmt.Errorf("SET response too short: %d bytes", len(resp))
	}
	return binary.BigEndian.Uint32(resp[0:4]) != 0, nil
}

// portmapUnset removes a program/version/protocol registration from system rpcbind.
// Returns (true, nil) if removed, (false, nil) if not found.
func portmapUnset(prog, vers, prot uint32) (bool, error) {
	args := make([]byte, 16)
	binary.BigEndian.PutUint32(args[0:4], prog)
	binary.BigEndian.PutUint32(args[4:8], vers)
	binary.BigEndian.PutUint32(args[8:12], prot)
	// port field ignored for UNSET

	resp, err := portmapCall(ProcUnset, args)
	if err != nil {
		return false, err
	}
	if len(resp) < 4 {
		return false, fmt.Errorf("UNSET response too short: %d bytes", len(resp))
	}
	return binary.BigEndian.Uint32(resp[0:4]) != 0, nil
}

// portmapGetPort queries system rpcbind for the port of a program/version/protocol.
// Returns 0 if not registered.
func portmapGetPort(prog, vers, prot uint32) (uint32, error) {
	args := make([]byte, 16)
	binary.BigEndian.PutUint32(args[0:4], prog)
	binary.BigEndian.PutUint32(args[4:8], vers)
	binary.BigEndian.PutUint32(args[8:12], prot)

	resp, err := portmapCall(ProcGetport, args)
	if err != nil {
		return 0, err
	}
	if len(resp) < 4 {
		return 0, fmt.Errorf("GETPORT response too short: %d bytes", len(resp))
	}
	return binary.BigEndian.Uint32(resp[0:4]), nil
}

// portmapDump retrieves all registrations from system rpcbind.
func portmapDump() ([]PortmapEntry, error) {
	resp, err := portmapCall(ProcDump, nil)
	if err != nil {
		return nil, err
	}

	var entries []PortmapEntry
	offset := 0

	for offset+4 <= len(resp) {
		valueFollows := binary.BigEndian.Uint32(resp[offset : offset+4])
		offset += 4

		if valueFollows == 0 {
			break
		}

		if offset+16 > len(resp) {
			return nil, fmt.Errorf("truncated DUMP entry at offset %d", offset)
		}

		entry := PortmapEntry{
			Program:  binary.BigEndian.Uint32(resp[offset : offset+4]),
			Version:  binary.BigEndian.Uint32(resp[offset+4 : offset+8]),
			Protocol: binary.BigEndian.Uint32(resp[offset+8 : offset+12]),
			Port:     binary.BigEndian.Uint32(resp[offset+12 : offset+16]),
		}
		entries = append(entries, entry)
		offset += 16
	}

	return entries, nil
}

// portmapCall sends an RPC call to system rpcbind and returns the procedure result.
func portmapCall(proc uint32, args []byte) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", "127.0.0.1:111", 2*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	// Build RPC CALL: prog=100000, vers=2, proc=proc
	rpcCall := buildRPCCall(100000, 2, proc, args)

	// TCP record marking
	fragHeader := make([]byte, 4)
	binary.BigEndian.PutUint32(fragHeader, uint32(len(rpcCall))|0x80000000)

	if _, err := conn.Write(append(fragHeader, rpcCall...)); err != nil {
		return nil, fmt.Errorf("write: %w", err)
	}

	// Read response
	respHeader := make([]byte, 4)
	if _, err := io.ReadFull(conn, respHeader); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	fragLen := binary.BigEndian.Uint32(respHeader) & 0x7FFFFFFF

	respBody := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, respBody); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	// Parse RPC reply header (24 bytes minimum)
	if len(respBody) < 24 {
		return nil, fmt.Errorf("response too short: %d bytes", len(respBody))
	}

	// Check accept_stat (offset 20-24)
	acceptStat := binary.BigEndian.Uint32(respBody[20:24])
	if acceptStat != 0 {
		return nil, fmt.Errorf("RPC rejected: accept_stat=%d", acceptStat)
	}

	// Return procedure result (after 24-byte header)
	return respBody[24:], nil
}

// buildRPCCall constructs a raw RPC CALL message.
func buildRPCCall(prog, vers, proc uint32, args []byte) []byte {
	header := make([]byte, 40)
	binary.BigEndian.PutUint32(header[0:4], 1)  // xid
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
