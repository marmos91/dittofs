package state

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
)

// buildMockCBNullReplyBody builds the RPC reply a client sends for CB_NULL.
// Unlike a CB_COMPOUND reply it carries no results at all: procedure 0 returns
// void, so the message ends at accept_stat.
func buildMockCBNullReplyBody(xid uint32) []byte {
	var reply bytes.Buffer
	_ = binary.Write(&reply, binary.BigEndian, xid)
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCReply))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCMsgAccepted))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.AuthNull))
	_ = binary.Write(&reply, binary.BigEndian, uint32(0))
	_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCSuccess))
	return reply.Bytes()
}

// readCBCall reads one record-marked RPC call off conn and returns its XID and
// procedure number.
func readCBCall(conn net.Conn) (xid, proc uint32, err error) {
	var headerBuf [4]byte
	if _, err = io.ReadFull(conn, headerBuf[:]); err != nil {
		return 0, 0, fmt.Errorf("read header: %w", err)
	}
	fragLen := binary.BigEndian.Uint32(headerBuf[:]) & 0x7FFFFFFF
	body := make([]byte, fragLen)
	if _, err = io.ReadFull(conn, body); err != nil {
		return 0, 0, fmt.Errorf("read body: %w", err)
	}
	if len(body) < 24 {
		return 0, 0, fmt.Errorf("call too short: %d bytes", len(body))
	}
	// xid, msg_type, rpcvers, prog, vers, proc
	return binary.BigEndian.Uint32(body[0:4]), binary.BigEndian.Uint32(body[20:24]), nil
}

// bindBackchannel wires a net.Pipe to the session as a back-bound connection
// and returns the client end plus the reply router for it.
func bindBackchannel(t *testing.T, sm *StateManager, sessionID types.SessionId4, connID uint64) (net.Conn, *PendingCBReplies) {
	t.Helper()

	clientConn, serverConn := net.Pipe()
	t.Cleanup(func() { _ = clientConn.Close() })
	t.Cleanup(func() { _ = serverConn.Close() })

	pending := sm.RegisterConnWriter(connID, func(data []byte) error {
		_, err := serverConn.Write(data)
		return err
	})
	if _, err := sm.BindConnToSession(connID, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}
	return clientConn, pending
}

// TestProbeCallbackPath_AnsweredCBNullEnablesDelegation is the positive half of
// the guard: a client that answers CB_NULL over its back channel gets
// CBPathUp, and an OPEN on an uncontended file is then offered a delegation.
func TestProbeCallbackPath_AnsweredCBNullEnablesDelegation(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()

	clientConn, pending := bindBackchannel(t, sm, sessionID, 2000)

	gotProc := make(chan uint32, 1)
	go func() {
		xid, proc, err := readCBCall(clientConn)
		if err != nil {
			gotProc <- 0xFFFFFFFF
			return
		}
		gotProc <- proc
		pending.Deliver(xid, buildMockCBNullReplyBody(xid))
	}()

	if err := sender.probeCallbackPath(context.Background()); err != nil {
		t.Fatalf("probeCallbackPath on a live back channel: %v", err)
	}

	if proc := <-gotProc; proc != types.CB_PROC_NULL {
		t.Errorf("probe sent callback procedure %d, want CB_PROC_NULL (%d)", proc, types.CB_PROC_NULL)
	}

	sm.setCBPathUp(sender.clientID, true)

	sm.mu.RLock()
	record := sm.clientRecordLocked(sender.clientID)
	sm.mu.RUnlock()
	if record == nil {
		t.Fatal("v4.1 client record not reachable through clientRecordLocked")
	}
	if !record.CBPathUp {
		t.Fatal("CBPathUp not set on the v4.1 client record")
	}

	delegType, granted := sm.ShouldGrantDelegation(sender.clientID, []byte("probe-file"), types.OPEN4_SHARE_ACCESS_READ)
	if !granted {
		t.Fatal("no delegation offered to a v4.1 client with a verified callback path")
	}
	if delegType != types.OPEN_DELEGATE_READ {
		t.Errorf("delegation type = %d, want OPEN_DELEGATE_READ", delegType)
	}
}

// TestProbeCallbackPath_RefusesWhenBackchannelUnreachable is the half that
// matters. A guard that has never refused anything is unverified, so each way
// the back channel can be unusable is exercised and asserted to leave CBPathUp
// false and delegations withheld.
func TestProbeCallbackPath_RefusesWhenBackchannelUnreachable(t *testing.T) {
	tests := []struct {
		name string
		// arrange makes the back channel unusable in one specific way.
		arrange func(t *testing.T, sm *StateManager, sessionID types.SessionId4, sender *BackchannelSender)
	}{
		{
			name: "no connection bound for the back channel",
			arrange: func(*testing.T, *StateManager, types.SessionId4, *BackchannelSender) {
				// Nothing bound: the session has back-channel slots from
				// CREATE_SESSION but no connection to write them over.
			},
		},
		{
			name: "write to the bound connection fails",
			arrange: func(t *testing.T, sm *StateManager, sessionID types.SessionId4, _ *BackchannelSender) {
				sm.RegisterConnWriter(3000, func([]byte) error {
					return fmt.Errorf("connection reset by peer")
				})
				if _, err := sm.BindConnToSession(3000, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
					t.Fatalf("BindConnToSession: %v", err)
				}
			},
		},
		{
			name: "client never answers CB_NULL",
			arrange: func(t *testing.T, sm *StateManager, sessionID types.SessionId4, sender *BackchannelSender) {
				clientConn, _ := bindBackchannel(t, sm, sessionID, 4000)
				// Drain the call so the write succeeds, then stay silent.
				go func() { _, _, _ = readCBCall(clientConn) }()
				sender.callbackTimeout = 150 * time.Millisecond
			},
		},
		{
			name: "client rejects the callback RPC",
			arrange: func(t *testing.T, sm *StateManager, sessionID types.SessionId4, _ *BackchannelSender) {
				clientConn, pending := bindBackchannel(t, sm, sessionID, 5000)
				go func() {
					xid, _, err := readCBCall(clientConn)
					if err != nil {
						return
					}
					var reply bytes.Buffer
					_ = binary.Write(&reply, binary.BigEndian, xid)
					_ = binary.Write(&reply, binary.BigEndian, uint32(rpc.RPCReply))
					// reply_stat = MSG_DENIED (1)
					_ = binary.Write(&reply, binary.BigEndian, uint32(1))
					pending.Deliver(xid, reply.Bytes())
				}()
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sender, sm, sessionID := createTestBackchannelSender(t)
			defer sm.Shutdown()

			// Prove the flag is not merely left at its zero value: set it, so
			// only an observed refusal can bring it back down.
			sm.setCBPathUp(sender.clientID, true)

			tc.arrange(t, sm, sessionID, sender)

			sm.probeV41CallbackPath(context.Background(), sender)

			sm.mu.RLock()
			record := sm.clientRecordLocked(sender.clientID)
			sm.mu.RUnlock()
			if record == nil {
				t.Fatal("v4.1 client record disappeared")
			}
			if record.CBPathUp {
				t.Fatal("CBPathUp stayed true after the probe could not reach the client")
			}

			if _, granted := sm.ShouldGrantDelegation(
				sender.clientID, []byte("unreachable-file"), types.OPEN4_SHARE_ACCESS_READ,
			); granted {
				t.Fatal("delegation granted to a client whose callback path is down")
			}
		})
	}
}

// TestSendRecallV41_ClearsCBPathUp checks that a v4.1 recall which cannot be
// delivered takes the callback path back down, the way the v4.0 dial-out path
// already did. Otherwise one grant to a client that then goes away keeps every
// later OPEN eligible for a delegation nobody can recall.
func TestSendRecallV41_ClearsCBPathUp(t *testing.T) {
	sender, sm, sessionID := createTestBackchannelSender(t)
	defer sm.Shutdown()

	// Bound but broken: the write fails on every attempt.
	sm.RegisterConnWriter(6000, func([]byte) error {
		return fmt.Errorf("connection reset by peer")
	})
	if _, err := sm.BindConnToSession(6000, sessionID, types.CDFC4_FORE_OR_BOTH); err != nil {
		t.Fatalf("BindConnToSession: %v", err)
	}

	sm.setCBPathUp(sender.clientID, true)

	// Shorten the retry backoff: the assertion is about what happens once
	// every attempt has failed, not about how long the waits between them are.
	// No test in this package runs in parallel, so nothing else reads this.
	savedDelays := backchannelRetryDelays
	backchannelRetryDelays = [backchannelMaxRetries]time.Duration{
		time.Millisecond, time.Millisecond, time.Millisecond,
	}
	t.Cleanup(func() { backchannelRetryDelays = savedDelays })

	// Run the sender so the queued recall is actually attempted.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sender.Run(ctx)

	deleg := &DelegationState{
		ClientID:   sender.clientID,
		FileHandle: []byte("recall-file"),
		DelegType:  types.OPEN_DELEGATE_READ,
		Stateid:    types.Stateid4{Seqid: 1},
	}

	done := make(chan struct{})
	go func() {
		sm.sendRecallV41(deleg, sender)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("sendRecallV41 did not return")
	}
	deleg.StopRecallTimer()

	sm.mu.RLock()
	record := sm.clientRecordLocked(sender.clientID)
	sm.mu.RUnlock()
	if record.CBPathUp {
		t.Fatal("CBPathUp stayed true after every CB_RECALL attempt failed")
	}
}

// ============================================================================
// OPEN_DELEGATE_NONE_EXT
// ============================================================================

func TestDelegationWantReason(t *testing.T) {
	tests := []struct {
		name        string
		shareAccess uint32
		wantWhy     uint32
		wantRefuse  bool
	}{
		{"no want bits leaves the decision to policy", types.OPEN4_SHARE_ACCESS_READ, 0, false},
		{"no preference leaves the decision to policy", types.OPEN4_SHARE_ACCESS_READ | types.OPEN4_SHARE_ACCESS_WANT_NO_PREFERENCE, 0, false},
		{"read want leaves the decision to policy", types.OPEN4_SHARE_ACCESS_READ | types.OPEN4_SHARE_ACCESS_WANT_READ_DELEG, 0, false},
		{"any want leaves the decision to policy", types.OPEN4_SHARE_ACCESS_BOTH | types.OPEN4_SHARE_ACCESS_WANT_ANY_DELEG, 0, false},
		{"no-deleg is an answer", types.OPEN4_SHARE_ACCESS_READ | types.OPEN4_SHARE_ACCESS_WANT_NO_DELEG, types.WND4_NOT_WANTED, true},
		{"cancel is an answer", types.OPEN4_SHARE_ACCESS_READ | types.OPEN4_SHARE_ACCESS_WANT_CANCEL, types.WND4_CANCELLED, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			why, refuse := DelegationWantReason(tc.shareAccess)
			if refuse != tc.wantRefuse {
				t.Fatalf("refuse = %v, want %v", refuse, tc.wantRefuse)
			}
			if refuse && why != tc.wantWhy {
				t.Errorf("why = %d, want %d", why, tc.wantWhy)
			}
		})
	}
}

func TestEncodeNoDelegationExt(t *testing.T) {
	tests := []struct {
		name string
		why  uint32
		want []uint32
	}{
		{
			name: "void reason is discriminant plus why",
			why:  types.WND4_NOT_WANTED,
			want: []uint32{types.OPEN_DELEGATE_NONE_EXT, types.WND4_NOT_WANTED},
		},
		{
			name: "cancelled is also void",
			why:  types.WND4_CANCELLED,
			want: []uint32{types.OPEN_DELEGATE_NONE_EXT, types.WND4_CANCELLED},
		},
		{
			name: "contention carries the will-push bool",
			why:  types.WND4_CONTENTION,
			want: []uint32{types.OPEN_DELEGATE_NONE_EXT, types.WND4_CONTENTION, 0},
		},
		{
			name: "resource carries the will-signal bool",
			why:  types.WND4_RESOURCE,
			want: []uint32{types.OPEN_DELEGATE_NONE_EXT, types.WND4_RESOURCE, 0},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			EncodeNoDelegationExt(&buf, tc.why)

			got := buf.Bytes()
			if len(got) != len(tc.want)*4 {
				t.Fatalf("encoded %d bytes, want %d: % x", len(got), len(tc.want)*4, got)
			}
			for i, wantWord := range tc.want {
				if word := binary.BigEndian.Uint32(got[i*4 : i*4+4]); word != wantWord {
					t.Errorf("word %d = %d, want %d", i, word, wantWord)
				}
			}
		})
	}
}
