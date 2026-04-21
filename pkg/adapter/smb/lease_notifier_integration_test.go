package smb

import (
	"encoding/binary"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	smb "github.com/marmos91/dittofs/internal/adapter/smb"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
)

// newPipeConnInfo builds a ConnInfo backed by a net.Pipe so SendFrame runs
// through the real WriteNetBIOSFrame path. Returns the ConnInfo and the
// client end of the pipe (for reading the wire bytes).
func newPipeConnInfo(connID uint64) (*smb.ConnInfo, net.Conn) {
	server, client := net.Pipe()
	ci := &smb.ConnInfo{
		ConnID:       connID,
		Conn:         server,
		WriteMu:      &smb.LockedWriter{},
		WriteTimeout: 2 * time.Second,
	}
	return ci, client
}

// readNetBIOSFrame reads one full NetBIOS-framed SMB2 message from client.
// Returns the SMB2 payload (framing stripped).
func readNetBIOSFrame(t *testing.T, client net.Conn) []byte {
	t.Helper()
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(client, hdr); err != nil {
		t.Fatalf("read netbios header: %v", err)
	}
	length := uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
	payload := make([]byte, length)
	if _, err := io.ReadFull(client, payload); err != nil {
		t.Fatalf("read netbios payload (len=%d): %v", length, err)
	}
	return payload
}

// TestSendLeaseBreak_IntegrationPrimaryGetsBreak verifies the full wire
// path: a lease break on a session with one registered primary channel
// results in a NetBIOS-framed SMB2 OPLOCK_BREAK message arriving on the
// primary's TCP connection, with the correct MessageID sentinel and
// SessionID-zero header per MS-SMB2 2.2.23.2.
func TestSendLeaseBreak_IntegrationPrimaryGetsBreak(t *testing.T) {
	const sessionID uint64 = 42
	sess := session.NewSession(sessionID, "127.0.0.1:445", false, "user", "")
	primaryCI, primaryClient := newPipeConnInfo(1)
	defer primaryClient.Close()
	defer primaryCI.Conn.Close()

	sessionConns := &sync.Map{}
	sessionConns.Store(sessionID, primaryCI)
	sess.AddChannel(&session.Channel{ConnID: 1, Transport: primaryCI})

	n := &transportNotifier{
		sessionConns:  sessionConns,
		oplockFileIDs: &sync.Map{},
		handler:       &fakeSessionLookup{sessions: map[uint64]*session.Session{sessionID: sess}},
	}

	var leaseKey [16]byte
	for i := range leaseKey {
		leaseKey[i] = byte(i + 1)
	}

	// SendLeaseBreak must not block on the synchronous pipe write, so drive
	// it on a goroutine and read from the client side.
	errCh := make(chan error, 1)
	go func() {
		errCh <- n.SendLeaseBreak(sessionID, leaseKey, 0x07, 0x01, 1)
	}()

	payload := readNetBIOSFrame(t, primaryClient)
	if err := <-errCh; err != nil {
		t.Fatalf("SendLeaseBreak: %v", err)
	}

	// SMB2 header is 64 bytes. Validate the unsolicited-notification fields
	// per MS-SMB2 §2.2.23.2.
	if len(payload) < 64 {
		t.Fatalf("short payload: %d bytes", len(payload))
	}
	if string(payload[0:4]) != "\xfeSMB" {
		t.Errorf("missing SMB2 protocol ID")
	}
	messageID := binary.LittleEndian.Uint64(payload[24:32])
	if messageID != 0xFFFFFFFFFFFFFFFF {
		t.Errorf("MessageID: got 0x%x, want 0xFFFFFFFFFFFFFFFF", messageID)
	}
	hdrSessionID := binary.LittleEndian.Uint64(payload[40:48])
	if hdrSessionID != 0 {
		t.Errorf("SessionID in header: got 0x%x, want 0 (MS-SMB2 2.2.23.2)", hdrSessionID)
	}
}

// TestSendLeaseBreak_IntegrationFailsOverToSecondaryOnClosedPrimary
// verifies the documented ordering: primary first, secondary on
// write-failure failover.
func TestSendLeaseBreak_IntegrationFailsOverToSecondaryOnClosedPrimary(t *testing.T) {
	const sessionID uint64 = 42
	sess := session.NewSession(sessionID, "127.0.0.1:445", false, "user", "")

	primaryCI, primaryClient := newPipeConnInfo(1)
	secondaryCI, secondaryClient := newPipeConnInfo(2)
	defer secondaryClient.Close()
	defer secondaryCI.Conn.Close()

	// Close the primary pipe so SendFrame on it fails.
	primaryCI.Conn.Close()
	primaryClient.Close()

	sessionConns := &sync.Map{}
	sessionConns.Store(sessionID, primaryCI)
	sess.AddChannel(&session.Channel{ConnID: 1, Transport: primaryCI})
	sess.AddChannel(&session.Channel{ConnID: 2, Transport: secondaryCI})

	n := &transportNotifier{
		sessionConns:  sessionConns,
		oplockFileIDs: &sync.Map{},
		handler:       &fakeSessionLookup{sessions: map[uint64]*session.Session{sessionID: sess}},
	}

	var leaseKey [16]byte
	errCh := make(chan error, 1)
	go func() {
		errCh <- n.SendLeaseBreak(sessionID, leaseKey, 0, 0, 0)
	}()

	// Secondary must receive the break after primary write fails.
	payload := readNetBIOSFrame(t, secondaryClient)
	if err := <-errCh; err != nil {
		t.Fatalf("SendLeaseBreak: %v", err)
	}
	if len(payload) < 64 {
		t.Fatalf("secondary got truncated payload: %d bytes", len(payload))
	}
}
