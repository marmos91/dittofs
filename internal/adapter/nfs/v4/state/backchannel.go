// Package state -- NFSv4.1 backchannel sender for multiplexed callbacks.
//
// BackchannelSender sends CB_SEQUENCE + CB_RECALL (and other callback ops)
// over existing TCP connections (back-bound via BIND_CONN_TO_SESSION or
// CREATE_SESSION). This avoids dial-out, making callbacks work through
// NAT/firewalls.
//
// Per RFC 8881 Section 2.10.3.1: "The server sends callback requests
// over back channel connections bound to the client's sessions."

package state

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/types"
	xdr "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// Constants
// ============================================================================

const (
	// defaultBackchannelTimeout is the timeout waiting for a callback reply.
	defaultBackchannelTimeout = 10 * time.Second

	// backchannelQueueSize is the capacity of the callback request queue.
	backchannelQueueSize = 64

	// backchannelRetryDelays defines exponential backoff delays for retry.
	// 3 attempts with 5s/10s/20s delays.
	backchannelMaxRetries = 3
)

var backchannelRetryDelays = [backchannelMaxRetries]time.Duration{
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
}

// ============================================================================
// ConnWriter -- callback for writing to a connection by ID
// ============================================================================

// ConnWriter writes data to a connection. The implementation must acquire
// the connection's writeMu to prevent interleaving with fore-channel replies.
type ConnWriter func(data []byte) error

// ============================================================================
// CallbackRequest -- a single callback to be sent via backchannel
// ============================================================================

// CallbackRequest represents a callback operation to be sent to a v4.1 client
// via the backchannel. The sender goroutine processes these from the queue.
type CallbackRequest struct {
	// OpCode is the callback operation code (e.g., OP_CB_RECALL).
	OpCode uint32

	// Payload is the pre-encoded callback operation (e.g., CB_RECALL args).
	Payload []byte

	// ResultCh receives the result of the callback send. Buffered (capacity 1).
	ResultCh chan error
}

// ============================================================================
// PendingCBReplies -- XID-keyed response routing
// ============================================================================

// PendingCBReplies routes backchannel REPLY messages to the goroutine that
// sent the corresponding CALL. When a backchannel CALL is sent, the XID is
// registered here. When the read loop receives a REPLY (msg_type=1), it
// delivers the bytes to the waiter via the XID-keyed channel.
type PendingCBReplies struct {
	mu      sync.Mutex
	waiters map[uint32]chan []byte
}

// NewPendingCBReplies creates a new PendingCBReplies instance.
func NewPendingCBReplies() *PendingCBReplies {
	return &PendingCBReplies{
		waiters: make(map[uint32]chan []byte),
	}
}

// Register registers an XID and returns a channel that will receive the reply.
// The returned channel has capacity 1 to prevent blocking the read loop.
func (p *PendingCBReplies) Register(xid uint32) chan []byte {
	ch := make(chan []byte, 1)
	p.mu.Lock()
	p.waiters[xid] = ch
	p.mu.Unlock()
	return ch
}

// Deliver delivers a reply to the waiter for the given XID.
// Returns true if a waiter was found and the reply was delivered.
func (p *PendingCBReplies) Deliver(xid uint32, reply []byte) bool {
	p.mu.Lock()
	ch, ok := p.waiters[xid]
	if ok {
		delete(p.waiters, xid)
	}
	p.mu.Unlock()
	if !ok {
		return false
	}
	ch <- reply
	return true
}

// Cancel removes a waiter for the given XID without delivering a reply.
// Used on timeout or error to clean up resources.
func (p *PendingCBReplies) Cancel(xid uint32) {
	p.mu.Lock()
	delete(p.waiters, xid)
	p.mu.Unlock()
}

// ============================================================================
// BackchannelSender -- goroutine that sends callbacks via backchannel
// ============================================================================

// BackchannelSender processes callback requests for a v4.1 session and sends
// them over back-bound TCP connections. It runs as a goroutine, receiving
// CallbackRequests from its queue, encoding CB_COMPOUND (CB_SEQUENCE + op),
// and writing them to the connection's TCP stream.
type BackchannelSender struct {
	sessionID types.SessionId4
	clientID  uint64

	// cbProgram is the callback RPC program number. It is read by the Run
	// goroutine (sendCallback) and updated by BackchannelCtl via
	// StateManager.UpdateBackchannelParams, so it is accessed atomically to
	// avoid a data race between the two goroutines.
	cbProgram atomic.Uint32

	queue chan CallbackRequest
	sm    *StateManager

	slotTable *SlotTable

	stopCh chan struct{}

	nextXID atomic.Uint32

	// nextCBSeqID is the per-slot CB_SEQUENCE seqID counter (RFC 8881
	// §2.10.6.1). It is independent of nextXID: the backchannel uses a single
	// slot, so a single monotonic counter incrementing by exactly 1 per send
	// is correct. Zero-value starts at 0, giving first seqID=1 on first Add(1).
	nextCBSeqID atomic.Uint32

	callbackTimeout time.Duration
}

// NewBackchannelSender creates a new BackchannelSender for the given session.
func NewBackchannelSender(
	sessionID types.SessionId4,
	clientID uint64,
	cbProgram uint32,
	slotTable *SlotTable,
	sm *StateManager,
) *BackchannelSender {
	bs := &BackchannelSender{
		sessionID:       sessionID,
		clientID:        clientID,
		queue:           make(chan CallbackRequest, backchannelQueueSize),
		sm:              sm,
		slotTable:       slotTable,
		stopCh:          make(chan struct{}),
		callbackTimeout: defaultBackchannelTimeout,
	}
	bs.cbProgram.Store(cbProgram)
	return bs
}

// Run is the main loop for the BackchannelSender goroutine.
// It processes callback requests from the queue until stopped or context is cancelled.
func (bs *BackchannelSender) Run(ctx context.Context) {
	logger.Debug("BackchannelSender started",
		"session_id", bs.sessionID.String(),
		"client_id", fmt.Sprintf("0x%x", bs.clientID))

	for {
		select {
		case <-ctx.Done():
			logger.Debug("BackchannelSender stopped (context cancelled)",
				"session_id", bs.sessionID.String())
			return
		case <-bs.stopCh:
			logger.Debug("BackchannelSender stopped",
				"session_id", bs.sessionID.String())
			return
		case req := <-bs.queue:
			bs.sendCallbackWithRetry(ctx, req)
		}
	}
}

// Stop signals the BackchannelSender to stop.
func (bs *BackchannelSender) Stop() {
	select {
	case <-bs.stopCh:
		// Already stopped
	default:
		close(bs.stopCh)
	}
}

// Enqueue adds a callback request to the queue. Returns false if the queue is
// full (non-blocking send).
func (bs *BackchannelSender) Enqueue(req CallbackRequest) bool {
	select {
	case bs.queue <- req:
		return true
	default:
		return false
	}
}

// sendCallbackWithRetry sends a callback with exponential backoff retry.
func (bs *BackchannelSender) sendCallbackWithRetry(ctx context.Context, req CallbackRequest) {
	var lastErr error

	for attempt := 0; attempt < backchannelMaxRetries; attempt++ {
		if attempt > 0 {
			delay := backchannelRetryDelays[attempt-1]
			logger.Debug("BackchannelSender retrying after delay",
				"session_id", bs.sessionID.String(),
				"attempt", attempt+1,
				"delay", delay)

			select {
			case <-ctx.Done():
				if req.ResultCh != nil {
					req.ResultCh <- ctx.Err()
				}
				return
			case <-bs.stopCh:
				if req.ResultCh != nil {
					req.ResultCh <- fmt.Errorf("backchannel sender stopped")
				}
				return
			case <-time.After(delay):
			}
		}

		err := bs.sendCallback(ctx, req)
		if err == nil {
			if req.ResultCh != nil {
				req.ResultCh <- nil
			}
			return
		}
		lastErr = err
		logger.Warn("BackchannelSender callback failed",
			"session_id", bs.sessionID.String(),
			"attempt", attempt+1,
			"error", err)
	}

	// All retries exhausted
	if req.ResultCh != nil {
		req.ResultCh <- fmt.Errorf("backchannel callback failed after %d attempts: %w",
			backchannelMaxRetries, lastErr)
	}

	// Mark backchannel fault on persistent failure
	bs.sm.setBackchannelFault(bs.clientID, true)
}

// sendCallback is the core send logic for a single callback attempt.
func (bs *BackchannelSender) sendCallback(ctx context.Context, req CallbackRequest) error {
	// 1. Allocate slot 0 with monotonic seqid (simplified EOS)
	seqID := bs.nextCBSeqID.Add(1)
	slotID := uint32(0)
	highestSlotID := uint32(0)
	if bs.slotTable != nil {
		highestSlotID = bs.slotTable.MaxSlots() - 1
	}

	// 2. Encode CB_SEQUENCE operation
	cbSeqOp := encodeCBSequenceOp(bs.sessionID, seqID, slotID, highestSlotID)

	// 3. Build CB_COMPOUND: CB_SEQUENCE + req.Payload
	compoundArgs := encodeCBCompoundV41([][]byte{cbSeqOp, req.Payload})

	// 4. Build RPC CALL message
	xid := bs.nextXID.Add(1)
	callMsg := BuildCBRPCCallMessage(xid, bs.cbProgram.Load(), types.NFS4_CALLBACK_VERSION, types.CB_PROC_COMPOUND, compoundArgs)

	// 5. Add record marking
	framedMsg := AddCBRecordMark(callMsg, true)

	// 6. Find a back-bound connection (0 = no exclusion)
	connID, writer, pending, ok := bs.sm.getBackBoundConnWriter(bs.sessionID, 0)
	if !ok {
		return fmt.Errorf("no back-bound connection for session %s", bs.sessionID.String())
	}

	// 7. Register XID with PendingCBReplies
	replyCh := pending.Register(xid)

	// 8. Write framed message (no lock held -- ConnWriter acquires writeMu internally)
	if err := writer(framedMsg); err != nil {
		pending.Cancel(xid)
		logger.Debug("BackchannelSender write failed, trying alternate connection",
			"session_id", bs.sessionID.String(),
			"conn_id", connID,
			"error", err)

		// Retry on another back-bound connection
		connID2, writer2, pending2, ok2 := bs.sm.getBackBoundConnWriter(bs.sessionID, connID)
		if !ok2 {
			return fmt.Errorf("write to back-bound connection %d failed and no alternate: %w", connID, err)
		}
		// Update pending to the new connection's PendingCBReplies so the
		// timeout path below cancels the correct waiter.
		pending = pending2
		replyCh = pending.Register(xid)
		if err2 := writer2(framedMsg); err2 != nil {
			pending.Cancel(xid)
			return fmt.Errorf("write to alternate connection %d also failed: %w", connID2, err2)
		}
	}

	// 9. Wait for reply with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, bs.callbackTimeout)
	defer cancel()

	select {
	case <-timeoutCtx.Done():
		pending.Cancel(xid)
		return fmt.Errorf("backchannel callback timed out after %s", bs.callbackTimeout)
	case replyBytes := <-replyCh:
		// 10. Validate CB_COMPOUND reply
		if err := ValidateCBReply(replyBytes); err != nil {
			return fmt.Errorf("backchannel callback reply validation failed: %w", err)
		}
		// Success -- clear backchannel fault
		bs.sm.setBackchannelFault(bs.clientID, false)
		return nil
	}
}

// ============================================================================
// CB_COMPOUND v4.1 Encoding
// ============================================================================

// encodeCBCompoundV41 encodes CB_COMPOUND4args for NFSv4.1.
//
// Wire format per RFC 8881 Section 20.2:
//
//	utf8str_cs  tag;           -- empty tag
//	uint32      minorversion;  -- 1 for NFSv4.1
//	uint32      callback_ident;-- 0 for v4.1 (not used, session-based)
//	nfs_cb_argop4 argarray<>;  -- pre-encoded operations
func encodeCBCompoundV41(ops [][]byte) []byte {
	var buf bytes.Buffer

	// tag: empty utf8str_cs (XDR opaque with length 0)
	_ = xdr.WriteXDROpaque(&buf, nil)

	// minorversion: 1
	_ = xdr.WriteUint32(&buf, 1)

	// callback_ident: 0 (not used for v4.1)
	_ = xdr.WriteUint32(&buf, 0)

	// argarray: count + operations
	_ = xdr.WriteUint32(&buf, uint32(len(ops)))
	for _, op := range ops {
		_, _ = buf.Write(op)
	}

	return buf.Bytes()
}

// encodeCBSequenceOp encodes the CB_SEQUENCE operation args.
//
// This is the first operation in every v4.1 CB_COMPOUND. It carries
// the session ID, sequence ID, and slot ID for exactly-once semantics.
func encodeCBSequenceOp(sessionID types.SessionId4, seqID, slotID, highestSlotID uint32) []byte {
	var buf bytes.Buffer

	// argop: OP_CB_SEQUENCE
	_ = xdr.WriteUint32(&buf, types.OP_CB_SEQUENCE)

	// Encode CB_SEQUENCE4args using the types encoder
	args := types.CbSequenceArgs{
		SessionID:     sessionID,
		SequenceID:    seqID,
		SlotID:        slotID,
		HighestSlotID: highestSlotID,
	}
	_ = args.Encode(&buf)

	return buf.Bytes()
}

// ============================================================================
// Callback Path Liveness (CB_NULL over the back channel)
// ============================================================================

// probeCallbackPath sends a CB_NULL over the session's back channel and reports
// whether the client answered it.
//
// CB_NULL is RPC procedure 0, not a CB_COMPOUND operation, so it carries no
// CB_SEQUENCE and consumes no back-channel slot. The reply demultiplexes by XID
// like every other back-channel reply.
//
// This is the v4.1 counterpart of SendCBNull, which dials out to the address a
// v4.0 client supplied in SETCLIENTID. A v4.1 client supplies no address: its
// callbacks travel back over a connection it opened, so the probe writes to a
// back-bound connection instead of dialing.
//
// Every step can fail, and each failure means the same thing to the caller --
// the server cannot reach this client's callback service:
//   - no connection has been bound for the back channel
//   - the write to it fails
//   - no reply arrives within the callback timeout
//   - the reply is not an accepted, successful RPC
//
// A failed write ends the probe rather than falling back to a second
// back-bound connection the way sendCallback does. That is stricter than a
// real CB_RECALL, so a client whose recalls would have landed on its second
// connection is judged unreachable and gets no delegation. Withholding one is
// the safe direction: the cost is a client that caches less, where the reverse
// is a delegation the server cannot recall.
func (bs *BackchannelSender) probeCallbackPath(ctx context.Context) error {
	xid := bs.nextXID.Add(1)
	callMsg := BuildCBRPCCallMessage(xid, bs.cbProgram.Load(), types.NFS4_CALLBACK_VERSION, types.CB_PROC_NULL, nil)
	framedMsg := AddCBRecordMark(callMsg, true)

	connID, writer, pending, ok := bs.sm.getBackBoundConnWriter(bs.sessionID, 0)
	if !ok {
		return fmt.Errorf("no back-bound connection for session %s", bs.sessionID.String())
	}

	replyCh := pending.Register(xid)
	if err := writer(framedMsg); err != nil {
		pending.Cancel(xid)
		return fmt.Errorf("write CB_NULL to back-bound connection %d: %w", connID, err)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, bs.callbackTimeout)
	defer cancel()

	select {
	case <-timeoutCtx.Done():
		pending.Cancel(xid)
		return fmt.Errorf("CB_NULL timed out after %s", bs.callbackTimeout)
	case <-bs.stopCh:
		pending.Cancel(xid)
		return fmt.Errorf("backchannel sender stopped")
	case replyBytes := <-replyCh:
		if err := ValidateCBReply(replyBytes); err != nil {
			return fmt.Errorf("CB_NULL reply: %w", err)
		}
		return nil
	}
}

// probeV41CallbackPath runs probeCallbackPath and records the verdict on the
// client record, so ShouldGrantDelegation can read callback liveness the same
// way for both minor versions.
//
// It runs asynchronously rather than inside CREATE_SESSION: a client that has
// just asked for a back channel is not required to be serving callbacks by the
// time its reply is written, and making every mount wait for a callback
// round-trip would pay for a delegation the client may never ask for.
//
// The verdict is a snapshot, not a subscription. It goes stale when the client
// stops answering, which is why a failed CB_RECALL clears CBPathUp again.
func (sm *StateManager) probeV41CallbackPath(ctx context.Context, bs *BackchannelSender) {
	err := bs.probeCallbackPath(ctx)
	sm.setCBPathUp(bs.clientID, err == nil)

	if err != nil {
		logger.Info("CB_NULL failed, delegations stay disabled for client",
			"client_id", fmt.Sprintf("0x%x", bs.clientID),
			"session_id", bs.sessionID.String(),
			"error", err)
		return
	}

	logger.Info("CB_NULL succeeded, delegations enabled for client",
		"client_id", fmt.Sprintf("0x%x", bs.clientID),
		"session_id", bs.sessionID.String())
}

// setCBPathUp records the outcome of a callback-path probe on the client
// record, whichever index holds it.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) setCBPathUp(clientID uint64, up bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if record := sm.clientRecordLocked(clientID); record != nil {
		record.CBPathUp = up
	}
}

func (sm *StateManager) SetMaxConnectionsPerSession(max int) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	if max >= 0 {
		sm.maxConnsPerSession = max
	}
}

// SetMaxSessionSlots sets the maximum fore channel slots per session.
// Only positive values are accepted; zero or negative values are ignored.
// Values exceeding DefaultMaxSlots are clamped to prevent advertising more
// slots than NewSlotTable allocates (which would cause NFS4ERR_BADSLOT).

func (sm *StateManager) SetMaxSessionSlots(n int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if n <= 0 {
		return
	}
	if n > int(DefaultMaxSlots) {
		n = int(DefaultMaxSlots)
	}
	sm.foreMaxSlots = uint32(n)
}

// SetMaxSessionsPerClient sets the maximum number of sessions per client.
// Only positive values are accepted; zero or negative values are ignored.

func (sm *StateManager) SetMaxSessionsPerClient(n int) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if n > 0 {
		sm.maxSessionsPerClient = n
	}
}

// ============================================================================
// Backchannel Operations
// ============================================================================

// RegisterConnWriter registers a ConnWriter callback for a back-bound connection.
// Called by the NFS adapter when a connection is bound for back-channel.
// Also creates a PendingCBReplies instance for the connection.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) RegisterConnWriter(connectionID uint64, writer ConnWriter) *PendingCBReplies {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	sm.connWriters[connectionID] = writer
	pending := NewPendingCBReplies()
	sm.cbRepliesByConn[connectionID] = pending
	return pending
}

// UnregisterConnWriter removes the ConnWriter and PendingCBReplies for a connection.
// Called on disconnect cleanup.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) UnregisterConnWriter(connectionID uint64) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	delete(sm.connWriters, connectionID)
	delete(sm.cbRepliesByConn, connectionID)
}

// GetPendingCBReplies returns the PendingCBReplies for a connection, or nil.
//
// Thread-safe: acquires sm.connMu.RLock.

func (sm *StateManager) GetPendingCBReplies(connectionID uint64) *PendingCBReplies {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()
	return sm.cbRepliesByConn[connectionID]
}

// StartBackchannelSender creates and starts a BackchannelSender for a session
// if the session has back-channel slots and no sender exists yet.
// Called lazily on first back-channel bind or first callback enqueue.
//
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) StartBackchannelSender(ctx context.Context, sessionID types.SessionId4) {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessionsByID[sessionID]
	if !exists || session.BackChannelSlots == nil {
		return
	}
	if session.backchannelSender != nil {
		return // Already started
	}

	sender := NewBackchannelSender(
		sessionID,
		session.ClientID,
		session.CbProgram,
		session.BackChannelSlots,
		sm,
	)
	session.backchannelSender = sender

	go sender.Run(ctx)

	// The back channel just became writable, which is the first moment a
	// CB_NULL to this client can succeed or fail for a real reason. Probing
	// here rather than in CreateSession keeps the callback round-trip off the
	// mount path, and this function is the once-per-session gate: it returned
	// above if a sender already existed.
	go sm.probeV41CallbackPath(ctx, sender)

	logger.Info("BackchannelSender started for session",
		"session_id", sessionID.String(),
		"client_id", fmt.Sprintf("0x%x", session.ClientID))
}

// stopBackchannelSender stops the BackchannelSender for a session.
// Called from destroySessionLocked to prevent orphan goroutines.
//
// Caller must hold sm.mu.

func (sm *StateManager) stopBackchannelSender(sessionID types.SessionId4) {
	session, exists := sm.sessionsByID[sessionID]
	if !exists {
		return
	}
	if session.backchannelSender != nil {
		session.backchannelSender.Stop()
		session.backchannelSender = nil
	}
}

// getBackchannelSender returns the BackchannelSender for the client's first
// session that has a backchannel. Returns nil if no v4.1 backchannel exists.
//
// Thread-safe: acquires sm.mu.RLock.

func (sm *StateManager) getBackchannelSender(clientID uint64) *BackchannelSender {
	sm.mu.RLock()
	defer sm.mu.RUnlock()

	for _, session := range sm.sessionsByClientID[clientID] {
		if session.backchannelSender != nil {
			return session.backchannelSender
		}
	}
	return nil
}

// getBackBoundConnWriter finds a back-bound connection for the session,
// optionally excluding a specific connection ID (pass 0 for no exclusion).
// Selects the connection with the most recent fore-channel activity.
//
// Lock ordering: acquires sm.connMu.RLock only (no sm.mu needed).

func (sm *StateManager) getBackBoundConnWriter(sessionID types.SessionId4, excludeConnID uint64) (uint64, ConnWriter, *PendingCBReplies, bool) {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	return sm.getBackBoundConnWriterLocked(sessionID, excludeConnID)
}

// getBackBoundConnWriterLocked is the common implementation for finding a
// back-bound connection. Caller must hold sm.connMu.RLock.

func (sm *StateManager) getBackBoundConnWriterLocked(sessionID types.SessionId4, excludeConnID uint64) (uint64, ConnWriter, *PendingCBReplies, bool) {
	bindings := sm.connBySession[sessionID]
	var bestConn *BoundConnection
	var bestTime time.Time

	for _, b := range bindings {
		if b.ConnectionID == excludeConnID {
			continue
		}
		if b.Direction != ConnDirBack && b.Direction != ConnDirBoth {
			continue
		}
		if bestConn == nil || b.LastActivity.After(bestTime) {
			bestConn = b
			bestTime = b.LastActivity
		}
	}

	if bestConn == nil {
		return 0, nil, nil, false
	}

	writer, ok := sm.connWriters[bestConn.ConnectionID]
	if !ok {
		return 0, nil, nil, false
	}
	pending := sm.cbRepliesByConn[bestConn.ConnectionID]
	if pending == nil {
		return 0, nil, nil, false
	}

	return bestConn.ConnectionID, writer, pending, true
}

// UpdateBackchannelParams stores new callback parameters on a session.
// Called by the BACKCHANNEL_CTL handler.
//
// Thread-safe: acquires sm.mu.Lock.

func (sm *StateManager) UpdateBackchannelParams(sessionID types.SessionId4, cbProgram uint32, secParms []types.CallbackSecParms4) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()

	session, exists := sm.sessionsByID[sessionID]
	if !exists {
		return ErrBadSession
	}

	session.CbProgram = cbProgram
	session.BackchannelSecParms = secParms

	// Update the sender's program number if it exists. The sender's Run
	// goroutine reads cbProgram without sm.mu, so the field is atomic.
	if session.backchannelSender != nil {
		session.backchannelSender.cbProgram.Store(cbProgram)
	}

	logger.Info("Backchannel params updated",
		"session_id", sessionID.String(),
		"cb_program", fmt.Sprintf("0x%x", cbProgram),
		"sec_parms_count", len(secParms))

	return nil
}

// setBackchannelFault sets or clears the backchannel fault flag for a client.
// Called by BackchannelSender on send failure/success.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) setBackchannelFault(clientID uint64, fault bool) {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()
	if fault {
		sm.backchannelFaults[clientID] = true
	} else {
		delete(sm.backchannelFaults, clientID)
	}
}

// hasBackBoundConnection returns true if the client has at least one
// back-bound connection across any of its sessions.
//
// Caller must hold sm.mu.RLock and sm.connMu.RLock (or ensure no concurrent access).

func (sm *StateManager) hasBackBoundConnection(clientID uint64) bool {
	for _, session := range sm.sessionsByClientID[clientID] {
		for _, b := range sm.connBySession[session.SessionID] {
			if b.Direction == ConnDirBack || b.Direction == ConnDirBoth {
				return true
			}
		}
	}
	return false
}
