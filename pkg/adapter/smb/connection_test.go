package smb

import (
	"encoding/binary"
	"io"
	"net"
	"testing"

	smb "github.com/marmos91/dittofs/internal/adapter/smb"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// =============================================================================
// Test Helper Functions
// =============================================================================

// newTestConnection creates a Connection with a minimal Adapter
// for unit testing. Uses net.Pipe() for a connected pair of net.Conn.
func newTestConnection(conn net.Conn) *Connection {
	adapter := New(Config{})

	return NewConnection(adapter, conn)
}

// =============================================================================
// TrackSession / UntrackSession Tests
// =============================================================================

func TestConnection_TrackUntrackSession(t *testing.T) {
	server, client := net.Pipe()
	defer func() { _ = server.Close() }()
	defer func() { _ = client.Close() }()

	c := newTestConnection(server)

	t.Run("TrackSession", func(t *testing.T) {
		c.TrackSession(100)

		c.sessionsMu.Lock()
		_, exists := c.sessions[100]
		c.sessionsMu.Unlock()

		if !exists {
			t.Error("Session 100 should be tracked")
		}
	})

	t.Run("TrackMultipleSessions", func(t *testing.T) {
		c.TrackSession(200)
		c.TrackSession(300)

		c.sessionsMu.Lock()
		count := len(c.sessions)
		c.sessionsMu.Unlock()

		// Should have 100, 200, 300
		if count != 3 {
			t.Errorf("Expected 3 tracked sessions, got %d", count)
		}
	})

	t.Run("UntrackSession", func(t *testing.T) {
		c.UntrackSession(200)

		c.sessionsMu.Lock()
		_, exists := c.sessions[200]
		count := len(c.sessions)
		c.sessionsMu.Unlock()

		if exists {
			t.Error("Session 200 should be untracked")
		}
		if count != 2 {
			t.Errorf("Expected 2 tracked sessions after untrack, got %d", count)
		}
	})

	t.Run("UntrackNonExistentSession", func(t *testing.T) {
		// Should not panic
		c.UntrackSession(99999)

		c.sessionsMu.Lock()
		count := len(c.sessions)
		c.sessionsMu.Unlock()

		if count != 2 {
			t.Errorf("Expected 2 tracked sessions (unchanged), got %d", count)
		}
	})

	t.Run("TrackDuplicateSession", func(t *testing.T) {
		c.TrackSession(100) // Already tracked

		c.sessionsMu.Lock()
		count := len(c.sessions)
		c.sessionsMu.Unlock()

		// Should still be 2 (100 and 300)
		if count != 2 {
			t.Errorf("Expected 2 tracked sessions (no duplicates), got %d", count)
		}
	})
}

// =============================================================================
// WriteNetBIOSFrame Tests (via internal/adapter/smb.WriteNetBIOSFrame)
// =============================================================================

func TestConnection_WriteNetBIOSFrame(t *testing.T) {
	t.Run("WritesCorrectFrameFormat", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		payload := []byte("hello SMB")

		// Write in a goroutine since net.Pipe is synchronous
		errCh := make(chan error, 1)
		go func() {
			errCh <- smb.WriteNetBIOSFrame(c.conn, &c.writeMu, c.server.config.Timeouts.Write, payload)
		}()

		// Read the frame from the other side
		frame := make([]byte, 4+len(payload))
		_, err := io.ReadFull(client, frame)
		if err != nil {
			t.Fatalf("Failed to read frame: %v", err)
		}

		if writeErr := <-errCh; writeErr != nil {
			t.Fatalf("WriteNetBIOSFrame error: %v", writeErr)
		}

		if frame[0] != 0x00 {
			t.Errorf("NetBIOS type = 0x%02x, expected 0x00 (session message)", frame[0])
		}

		length := uint32(frame[1])<<16 | uint32(frame[2])<<8 | uint32(frame[3])
		if length != uint32(len(payload)) {
			t.Errorf("NetBIOS length = %d, expected %d", length, len(payload))
		}

		if string(frame[4:]) != "hello SMB" {
			t.Errorf("Payload = %q, expected %q", string(frame[4:]), "hello SMB")
		}
	})

	t.Run("WritesEmptyPayload", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		errCh := make(chan error, 1)
		go func() {
			errCh <- smb.WriteNetBIOSFrame(c.conn, &c.writeMu, c.server.config.Timeouts.Write, []byte{})
		}()

		frame := make([]byte, 4)
		_, err := io.ReadFull(client, frame)
		if err != nil {
			t.Fatalf("Failed to read frame: %v", err)
		}

		if writeErr := <-errCh; writeErr != nil {
			t.Fatalf("WriteNetBIOSFrame error: %v", writeErr)
		}

		length := uint32(frame[1])<<16 | uint32(frame[2])<<8 | uint32(frame[3])
		if length != 0 {
			t.Errorf("NetBIOS length = %d, expected 0", length)
		}
	})

	t.Run("WritesLargePayload", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		payload := make([]byte, 65536)
		for i := range payload {
			payload[i] = byte(i % 256)
		}

		errCh := make(chan error, 1)
		go func() {
			errCh <- smb.WriteNetBIOSFrame(c.conn, &c.writeMu, c.server.config.Timeouts.Write, payload)
		}()

		frame := make([]byte, 4+len(payload))
		_, err := io.ReadFull(client, frame)
		if err != nil {
			t.Fatalf("Failed to read frame: %v", err)
		}

		if writeErr := <-errCh; writeErr != nil {
			t.Fatalf("WriteNetBIOSFrame error: %v", writeErr)
		}

		length := uint32(frame[1])<<16 | uint32(frame[2])<<8 | uint32(frame[3])
		if length != uint32(len(payload)) {
			t.Errorf("NetBIOS length = %d, expected %d", length, len(payload))
		}

		if frame[4] != 0 || frame[5] != 1 {
			t.Error("Payload content mismatch")
		}
	})
}

// =============================================================================
// InjectFileID Tests (via internal/adapter/smb.InjectFileID)
// =============================================================================

func TestConnection_InjectFileID(t *testing.T) {
	var testFileID [16]byte
	for i := range testFileID {
		testFileID[i] = byte(0xA0 + i)
	}

	t.Run("InjectForClose", func(t *testing.T) {
		body := make([]byte, 24)
		binary.LittleEndian.PutUint16(body[0:2], 24)

		result := smb.InjectFileID(types.SMB2Close, body, testFileID)

		var injected [16]byte
		copy(injected[:], result[8:24])
		if injected != testFileID {
			t.Errorf("FileID not injected correctly at offset 8 for CLOSE")
		}

		if body[8] != 0 {
			t.Error("Original body should not be modified")
		}
	})

	t.Run("InjectForRead", func(t *testing.T) {
		body := make([]byte, 49)
		binary.LittleEndian.PutUint16(body[0:2], 49)

		result := smb.InjectFileID(types.SMB2Read, body, testFileID)

		var injected [16]byte
		copy(injected[:], result[16:32])
		if injected != testFileID {
			t.Errorf("FileID not injected correctly at offset 16 for READ")
		}
	})

	t.Run("InjectForWrite", func(t *testing.T) {
		body := make([]byte, 49)
		binary.LittleEndian.PutUint16(body[0:2], 49)

		result := smb.InjectFileID(types.SMB2Write, body, testFileID)

		var injected [16]byte
		copy(injected[:], result[16:32])
		if injected != testFileID {
			t.Errorf("FileID not injected correctly at offset 16 for WRITE")
		}
	})

	t.Run("InjectForQueryInfo", func(t *testing.T) {
		body := make([]byte, 41)
		binary.LittleEndian.PutUint16(body[0:2], 41)

		result := smb.InjectFileID(types.SMB2QueryInfo, body, testFileID)

		var injected [16]byte
		copy(injected[:], result[24:40])
		if injected != testFileID {
			t.Errorf("FileID not injected correctly at offset 24 for QUERY_INFO")
		}
	})

	t.Run("InjectForQueryDirectory", func(t *testing.T) {
		body := make([]byte, 33)
		binary.LittleEndian.PutUint16(body[0:2], 33)

		result := smb.InjectFileID(types.SMB2QueryDirectory, body, testFileID)

		var injected [16]byte
		copy(injected[:], result[8:24])
		if injected != testFileID {
			t.Errorf("FileID not injected correctly at offset 8 for QUERY_DIRECTORY")
		}
	})

	t.Run("InjectForSetInfo", func(t *testing.T) {
		body := make([]byte, 33)
		binary.LittleEndian.PutUint16(body[0:2], 33)

		result := smb.InjectFileID(types.SMB2SetInfo, body, testFileID)

		var injected [16]byte
		copy(injected[:], result[16:32])
		if injected != testFileID {
			t.Errorf("FileID not injected correctly at offset 16 for SET_INFO")
		}
	})

	t.Run("NoInjectionForUnsupportedCommand", func(t *testing.T) {
		body := make([]byte, 40)
		body[0] = 0xFF

		result := smb.InjectFileID(types.SMB2Negotiate, body, testFileID)

		if result[0] != 0xFF {
			t.Error("Body should be unchanged for unsupported command")
		}
	})

	t.Run("BodyTooSmallForInjection", func(t *testing.T) {
		shortBody := make([]byte, 10)

		result := smb.InjectFileID(types.SMB2Close, shortBody, testFileID)

		if len(result) != 10 {
			t.Errorf("Short body should be returned unchanged, got length %d", len(result))
		}
	})
}

// =============================================================================
// MakeErrorBody Tests (via internal/adapter/smb.MakeErrorBody)
// =============================================================================

func TestMakeErrorBody(t *testing.T) {
	body := smb.MakeErrorBody()

	t.Run("HasCorrectLength", func(t *testing.T) {
		if len(body) != 9 {
			t.Errorf("Error body length = %d, expected 9", len(body))
		}
	})

	t.Run("HasCorrectStructureSize", func(t *testing.T) {
		structSize := binary.LittleEndian.Uint16(body[0:2])
		if structSize != 9 {
			t.Errorf("StructureSize = %d, expected 9", structSize)
		}
	})

	t.Run("ErrorContextCountIsZero", func(t *testing.T) {
		if body[2] != 0 {
			t.Errorf("ErrorContextCount = %d, expected 0", body[2])
		}
	})

	t.Run("ReservedIsZero", func(t *testing.T) {
		if body[3] != 0 {
			t.Errorf("Reserved = %d, expected 0", body[3])
		}
	})

	t.Run("ByteCountIsZero", func(t *testing.T) {
		byteCount := binary.LittleEndian.Uint32(body[4:8])
		if byteCount != 0 {
			t.Errorf("ByteCount = %d, expected 0", byteCount)
		}
	})
}

// =============================================================================
// ParseCompoundCommand Tests (via internal/adapter/smb.ParseCompoundCommand)
// =============================================================================

func TestParseCompoundCommand(t *testing.T) {
	t.Run("RejectsTooSmallData", func(t *testing.T) {
		data := make([]byte, 30)

		_, _, _, err := smb.ParseCompoundCommand(data)
		if err == nil {
			t.Error("Expected error for data smaller than header size")
		}
	})

	t.Run("ParsesSingleCommand", func(t *testing.T) {
		data := make([]byte, header.HeaderSize+20)

		binary.LittleEndian.PutUint32(data[0:4], types.SMB2ProtocolID)
		binary.LittleEndian.PutUint16(data[4:6], header.HeaderSize)
		binary.LittleEndian.PutUint16(data[12:14], uint16(types.SMB2Negotiate))
		binary.LittleEndian.PutUint32(data[20:24], 0)
		binary.LittleEndian.PutUint64(data[24:32], 42)

		hdr, body, remaining, err := smb.ParseCompoundCommand(data)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if hdr.Command != types.SMB2Negotiate {
			t.Errorf("Command = %d, expected NEGOTIATE", hdr.Command)
		}

		if hdr.MessageID != 42 {
			t.Errorf("MessageID = %d, expected 42", hdr.MessageID)
		}

		if len(body) != 20 {
			t.Errorf("Body length = %d, expected 20", len(body))
		}

		if len(remaining) != 0 {
			t.Errorf("Remaining should be empty, got %d bytes", len(remaining))
		}
	})

	t.Run("ParsesCompoundWithNextCommand", func(t *testing.T) {
		cmd1Size := header.HeaderSize + 20
		totalSize := cmd1Size + header.HeaderSize + 10
		data := make([]byte, totalSize)

		binary.LittleEndian.PutUint32(data[0:4], types.SMB2ProtocolID)
		binary.LittleEndian.PutUint16(data[4:6], header.HeaderSize)
		binary.LittleEndian.PutUint16(data[12:14], uint16(types.SMB2Negotiate))
		binary.LittleEndian.PutUint32(data[20:24], uint32(cmd1Size))
		binary.LittleEndian.PutUint64(data[24:32], 1)

		binary.LittleEndian.PutUint32(data[cmd1Size:cmd1Size+4], types.SMB2ProtocolID)
		binary.LittleEndian.PutUint16(data[cmd1Size+4:cmd1Size+6], header.HeaderSize)
		binary.LittleEndian.PutUint16(data[cmd1Size+12:cmd1Size+14], uint16(types.SMB2SessionSetup))
		binary.LittleEndian.PutUint32(data[cmd1Size+20:cmd1Size+24], 0)
		binary.LittleEndian.PutUint64(data[cmd1Size+24:cmd1Size+32], 2)

		hdr, body, remaining, err := smb.ParseCompoundCommand(data)
		if err != nil {
			t.Fatalf("Unexpected error: %v", err)
		}

		if hdr.Command != types.SMB2Negotiate {
			t.Errorf("First command = %d, expected NEGOTIATE", hdr.Command)
		}

		if len(body) != 20 {
			t.Errorf("Body length = %d, expected 20", len(body))
		}

		if len(remaining) == 0 {
			t.Fatal("Expected remaining compound data")
		}

		hdr2, body2, remaining2, err := smb.ParseCompoundCommand(remaining)
		if err != nil {
			t.Fatalf("Error parsing second command: %v", err)
		}

		if hdr2.Command != types.SMB2SessionSetup {
			t.Errorf("Second command = %d, expected SESSION_SETUP", hdr2.Command)
		}

		if hdr2.MessageID != 2 {
			t.Errorf("Second MessageID = %d, expected 2", hdr2.MessageID)
		}

		if len(body2) != 10 {
			t.Errorf("Second body length = %d, expected 10", len(body2))
		}

		if len(remaining2) != 0 {
			t.Errorf("Should have no remaining data, got %d bytes", len(remaining2))
		}
	})
}

// =============================================================================
// TrackSessionLifecycle Tests (via internal/adapter/smb.TrackSessionLifecycle)
// =============================================================================

func TestTrackSessionLifecycle(t *testing.T) {
	t.Run("TracksOnSessionSetupSuccess", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		smb.TrackSessionLifecycle(types.SMB2SessionSetup, 0, 42, types.StatusSuccess, c)

		c.sessionsMu.Lock()
		_, exists := c.sessions[42]
		c.sessionsMu.Unlock()

		if !exists {
			t.Error("Session should be tracked after successful SESSION_SETUP")
		}
	})

	t.Run("DoesNotTrackOnMoreProcessingRequired", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		smb.TrackSessionLifecycle(types.SMB2SessionSetup, 0, 42, types.StatusMoreProcessingRequired, c)

		c.sessionsMu.Lock()
		_, exists := c.sessions[42]
		c.sessionsMu.Unlock()

		if exists {
			t.Error("Session should not be tracked during NTLM handshake (MORE_PROCESSING_REQUIRED)")
		}
	})

	t.Run("UntracksOnLogoffSuccess", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		c.TrackSession(42)

		smb.TrackSessionLifecycle(types.SMB2Logoff, 42, 0, types.StatusSuccess, c)

		c.sessionsMu.Lock()
		_, exists := c.sessions[42]
		c.sessionsMu.Unlock()

		if exists {
			t.Error("Session should be untracked after LOGOFF")
		}
	})

	t.Run("UsesReqSessionIDForLogoff", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		c.TrackSession(100)

		smb.TrackSessionLifecycle(types.SMB2Logoff, 100, 0, types.StatusSuccess, c)

		c.sessionsMu.Lock()
		_, exists := c.sessions[100]
		c.sessionsMu.Unlock()

		if exists {
			t.Error("Session 100 should be untracked after LOGOFF")
		}
	})

	t.Run("FallsBackToReqSessionID", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		smb.TrackSessionLifecycle(types.SMB2SessionSetup, 55, 0, types.StatusSuccess, c)

		c.sessionsMu.Lock()
		_, exists := c.sessions[55]
		c.sessionsMu.Unlock()

		if !exists {
			t.Error("Should fall back to reqSessionID when ctxSessionID is 0")
		}
	})

	t.Run("IgnoresOtherCommands", func(t *testing.T) {
		server, client := net.Pipe()
		defer func() { _ = server.Close() }()
		defer func() { _ = client.Close() }()

		c := newTestConnection(server)

		smb.TrackSessionLifecycle(types.SMB2Create, 0, 42, types.StatusSuccess, c)

		c.sessionsMu.Lock()
		count := len(c.sessions)
		c.sessionsMu.Unlock()

		if count != 0 {
			t.Error("Should not track sessions for non-SESSION_SETUP/LOGOFF commands")
		}
	})
}
