package smb

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/smb/encryption"
	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// TestSendMessage_EncryptedShareLeavesTreeConnectClear pins the Windows
// failure: with session encryption off and the share flag on, the
// TREE_CONNECT response must stay readable. It is the message that carries
// the share encryption flag. Later commands on that tree stay encrypted,
// and a session that already requires encryption still encrypts the open.
func TestSendMessage_EncryptedShareLeavesTreeConnectClear(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() {
		_ = server.Close()
		_ = client.Close()
	})

	mgr := session.NewDefaultManager()
	h := handlers.NewHandlerWithSessionManager(mgr)
	sess := session.NewSession(0x42, "192.168.64.3:1", false, "alice", "CORP.TEST")
	cs := sess.GetCryptoState()
	cs.EncryptionKey = bytes.Repeat([]byte{0x11}, 16)
	cs.DecryptionKey = bytes.Repeat([]byte{0x22}, 16)
	if err := cs.CreateEncryptors(types.CipherAES128GCM); err != nil {
		t.Fatal(err)
	}
	mgr.StoreSession(sess)

	const treeID = uint32(7)
	h.StoreTree(&handlers.TreeConnection{
		TreeID:      treeID,
		SessionID:   sess.SessionID,
		ShareName:   "/alice",
		EncryptData: true,
	})

	connInfo := &ConnInfo{
		Conn:    server,
		WriteMu: &LockedWriter{},
		Handler: h,
		EncryptionMiddleware: encryption.NewEncryptionMiddleware(func(id uint64) (encryption.EncryptableSession, bool) {
			s, ok := h.GetSession(id)
			if !ok {
				return nil, false
			}
			return s, true
		}),
	}

	// net.Pipe blocks the writer until a reader is waiting, so the read starts
	// before sendMessage.
	roundTrip := func(command types.Command) uint32 {
		t.Helper()
		got := make(chan uint32, 1)
		errc := make(chan error, 1)
		go func() {
			nb := make([]byte, 4)
			if _, err := io.ReadFull(client, nb); err != nil {
				errc <- err
				return
			}
			n := int(nb[1])<<16 | int(nb[2])<<8 | int(nb[3])
			body := make([]byte, n)
			if _, err := io.ReadFull(client, body); err != nil {
				errc <- err
				return
			}
			if len(body) < 4 {
				errc <- io.ErrUnexpectedEOF
				return
			}
			got <- binary.LittleEndian.Uint32(body[:4])
		}()

		hdr := &header.SMB2Header{
			Command:   command,
			Status:    types.StatusSuccess,
			SessionID: sess.SessionID,
			TreeID:    treeID,
		}
		if err := sendMessage(hdr, []byte{0x09, 0x00}, connInfo, false, false, nil); err != nil {
			t.Fatal(err)
		}
		select {
		case id := <-got:
			return id
		case err := <-errc:
			t.Fatal(err)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the response")
		}
		return 0
	}

	if got := roundTrip(types.SMB2TreeConnect); got != types.SMB2ProtocolID {
		t.Fatalf("TREE_CONNECT protocol = %#x, want clear SMB2 %#x", got, types.SMB2ProtocolID)
	}

	if got := roundTrip(types.SMB2Read); got != header.TransformProtocolID {
		t.Fatalf("READ protocol = %#x, want encrypted transform %#x", got, header.TransformProtocolID)
	}

	cs.EncryptData = true
	if got := roundTrip(types.SMB2TreeConnect); got != header.TransformProtocolID {
		t.Fatalf("required-session TREE_CONNECT protocol = %#x, want encrypted transform %#x", got, header.TransformProtocolID)
	}
}
