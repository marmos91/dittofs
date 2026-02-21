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

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/internal/protocol/nfs/v4/types"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
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
	cbProgram uint32

	queue chan CallbackRequest
	sm    *StateManager

	slotTable *SlotTable

	stopCh chan struct{}

	nextXID atomic.Uint32

	callbackTimeout time.Duration

	// metrics tracks backchannel callback Prometheus metrics.
	// May be nil; BackchannelMetrics methods are nil-safe.
	metrics *BackchannelMetrics
}

// NewBackchannelSender creates a new BackchannelSender for the given session.
func NewBackchannelSender(
	sessionID types.SessionId4,
	clientID uint64,
	cbProgram uint32,
	slotTable *SlotTable,
	sm *StateManager,
	metrics *BackchannelMetrics,
) *BackchannelSender {
	return &BackchannelSender{
		sessionID:       sessionID,
		clientID:        clientID,
		cbProgram:       cbProgram,
		queue:           make(chan CallbackRequest, backchannelQueueSize),
		sm:              sm,
		slotTable:       slotTable,
		stopCh:          make(chan struct{}),
		callbackTimeout: defaultBackchannelTimeout,
		metrics:         metrics,
	}
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
			bs.metrics.RecordRetry()

			delay := backchannelRetryDelays[attempt-1]
			logger.Debug("BackchannelSender retrying after delay",
				"session_id", bs.sessionID.String(),
				"attempt", attempt+1,
				"delay", delay)

			select {
			case <-ctx.Done():
				bs.metrics.RecordFailure()
				if req.ResultCh != nil {
					req.ResultCh <- ctx.Err()
				}
				return
			case <-bs.stopCh:
				bs.metrics.RecordFailure()
				if req.ResultCh != nil {
					req.ResultCh <- fmt.Errorf("backchannel sender stopped")
				}
				return
			case <-time.After(delay):
			}
		}

		bs.metrics.RecordCallback()
		start := time.Now()

		err := bs.sendCallback(ctx, req)

		bs.metrics.ObserveDuration(time.Since(start))

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
	bs.metrics.RecordFailure()
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
	seqID := bs.nextXID.Add(1)
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
	callMsg := BuildCBRPCCallMessage(xid, bs.cbProgram, types.NFS4_CALLBACK_VERSION, types.CB_PROC_COMPOUND, compoundArgs)

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
		replyCh = pending2.Register(xid)
		if err2 := writer2(framedMsg); err2 != nil {
			pending2.Cancel(xid)
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
