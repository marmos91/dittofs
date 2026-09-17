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
	"errors"
	"fmt"
	"sort"
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

// nextCallbackXID mints the XID of every callback the server sends. A
// connection's reply demultiplexer is keyed on XID alone while a sender belongs
// to one session, and one connection may carry several sessions, so a counter
// per sender would let two of them register the same XID and strand the
// first one's waiter.
var nextCallbackXID atomic.Uint32

var backchannelRetryDelays = [backchannelMaxRetries]time.Duration{
	5 * time.Second,
	10 * time.Second,
	20 * time.Second,
}

// ============================================================================
// ConnWriter -- callback for writing to a connection by ID
// ============================================================================

// ConnWriter writes data to a connection. The implementation must acquire
// the connection's writeMu to prevent interleaving with fore-channel replies,
// and must bound the write by CallbackWriteTimeout.
type ConnWriter func(data []byte) error

// CallbackWriteTimeout is the budget one ConnWriter call may spend on the
// socket. A callback write holds the connection's write lock, so an unbounded
// one blocks every fore-channel reply behind it and the connection close that
// waits on those replies. worstCaseSendDuration charges each attempt for it,
// so a writer that runs longer than this expires the recall watchdog while its
// own callback is still on the wire.
const CallbackWriteTimeout = defaultBackchannelTimeout

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

	// Dequeue is the handshake around the moment the sender picks this request
	// up, or nil for callers that do not need it. Sends are serialised, so a
	// request can sit in the queue behind other callbacks for their whole retry
	// schedules; a caller that bounds its own queue wait needs a single atomic
	// point where "the sender took it" and "the caller gave up" are decided,
	// rather than two checks that can disagree.
	Dequeue *callbackDequeue
}

// callbackDequeue is the one decision point between a sender picking a queued
// request up and the caller abandoning it.
type callbackDequeue struct {
	mu        sync.Mutex
	taken     bool
	abandoned bool
	// started is closed once, when the request is taken, so a caller can wait
	// for the dequeue without polling.
	started chan struct{}
}

func newCallbackDequeue() *callbackDequeue {
	return &callbackDequeue{started: make(chan struct{})}
}

// take marks the request as picked up. It reports false when the caller gave up
// on the queue wait first, in which case the sender must drop the request
// rather than deliver a callback the caller has moved past.
func (d *callbackDequeue) take() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.abandoned {
		return false
	}
	d.taken = true
	close(d.started)
	return true
}

// abandon gives up on the queue wait. It reports false when the sender already
// took the request, in which case the caller must wait for the send's result
// rather than treat the recall as never attempted.
func (d *callbackDequeue) abandon() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.taken {
		return false
	}
	d.abandoned = true
	return true
}

// ============================================================================
// PendingCBReplies -- XID-keyed response routing
// ============================================================================

// PendingCBReplies routes backchannel REPLY messages to the goroutine that
// sent the corresponding CALL. When a backchannel CALL is sent, the XID is
// registered here. When the read loop receives a REPLY (msg_type=1), it
// delivers the bytes to the waiter via the XID-keyed channel.
// cbWaiter is one registered callback reply: the channel the reply lands on,
// and the session whose sender is waiting for it.
//
// The session is recorded because the table is held per connection while a
// waiter belongs to a session, and several sessions can share one connection.
// Cancelling the waiters of a session that is going away is then addressable at
// all; a connection-wide release can only honestly answer for the connection
// itself, which is why FailAll is not enough. Deliver still routes on the XID
// alone — the session is read only by CancelSession.
type cbWaiter struct {
	ch      chan []byte
	session types.SessionId4
}

type PendingCBReplies struct {
	mu      sync.Mutex
	waiters map[uint32]cbWaiter
	// closed marks the connection behind this table as gone. It is what makes
	// a late Register safe: a sender reads the table under the connection lock
	// and registers after releasing it, so a teardown can land in between and
	// would otherwise leave that sender waiting on a demultiplexer nothing
	// writes to any more.
	closed bool
}

// FailAll releases every waiter and refuses future ones. Called when the
// connection carrying them dies: the replies they are waiting for can no longer
// arrive, and a closed channel reaches the waiter now rather than leaving it to
// discover the loss when its own timeout expires. A sender blocked there is
// holding up the recall behind it, which is how a dead connection turns into a
// revoked delegation on a client that another session could still have reached.
func (p *PendingCBReplies) FailAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for xid, w := range p.waiters {
		close(w.ch)
		delete(p.waiters, xid)
	}
}

// NewPendingCBReplies creates a new PendingCBReplies instance.
func NewPendingCBReplies() *PendingCBReplies {
	return &PendingCBReplies{
		waiters: make(map[uint32]cbWaiter),
	}
}

// Register registers an XID and returns a channel that will receive the reply.
// The returned channel has capacity 1 to prevent blocking the read loop.
//
// After FailAll the channel comes back already closed rather than joining a
// table no reply can reach, so a caller that registers just too late fails at
// once instead of waiting out its timeout.
func (p *PendingCBReplies) Register(xid uint32, session types.SessionId4) (chan []byte, bool) {
	ch := make(chan []byte, 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		// The connection was retired between the caller picking it and getting
		// here. The closed channel keeps a caller that ignores the flag from
		// blocking forever, but the flag is what matters: a reply read off a
		// closed channel is indistinguishable from a client that answered with
		// nothing, and a caller that writes anyway is writing to a dead socket
		// and would score the result against the client.
		close(ch)
		return ch, false
	}
	p.waiters[xid] = cbWaiter{ch: ch, session: session}
	return ch, true
}

// Deliver delivers a reply to the waiter for the given XID.
// Returns true if a waiter was found and the reply was delivered.
//
// The send stays under the mutex. Releasing it first and sending afterwards
// races FailAll closing the same channel, and a send on a closed channel panics
// in the read loop that called this. It cannot block: the channel has capacity
// one and the XID is removed from the table here, so there is never a second
// sender for it.
func (p *PendingCBReplies) Deliver(xid uint32, reply []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	w, ok := p.waiters[xid]
	if !ok {
		return false
	}
	delete(p.waiters, xid)
	w.ch <- reply
	return true
}

// Cancel removes a waiter for the given XID without delivering a reply.
// Used on timeout or error to clean up resources.
// Cancel removes a waiter and reports whether the table had already been
// retired. That answer is the difference between two outcomes a caller must not
// confuse: a client that went quiet, and a socket on this side that went away.
// FailAll closes the waiters and marks the table closed under this same mutex,
// so asking here settles both as one observation rather than leaving a window
// between the check and the removal.
func (p *PendingCBReplies) Cancel(xid uint32) (retired bool) {
	p.mu.Lock()
	delete(p.waiters, xid)
	retired = p.closed
	p.mu.Unlock()
	return retired
}

// CancelSession releases every waiter belonging to session, without delivering
// a reply. Called when the session loses the connection this table belongs to —
// teardown, or a rebind away from the back channel — so a sender waiting on it
// fails now rather than after its own timeout.
//
// A connection-wide release cannot reach these: several sessions can share one
// connection, so FailAll only runs when the connection itself dies, and a
// session that is destroyed or loses its back binding on a live connection
// leaves its waiters armed with nothing able to answer them. The channel is
// closed rather than left empty, so the waiter reads the same "no reply is
// coming" signal FailAll gives and does not score the silence against the
// client.
func (p *PendingCBReplies) CancelSession(session types.SessionId4) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for xid, w := range p.waiters {
		if w.session == session {
			close(w.ch)
			delete(p.waiters, xid)
		}
	}
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

	// params is the callback program number and the pre-encoded credential
	// every callback on this session carries, held as one value. Both are read
	// by the Run goroutine (sendCallback) and rewritten by BACKCHANNEL_CTL via
	// StateManager.UpdateBackchannelParams, so the pointer is accessed
	// atomically to avoid a data race between the two goroutines.
	//
	// One value rather than two, because the client negotiates them together. A
	// callback that loaded them separately while BACKCHANNEL_CTL was rewriting
	// them could pair the new program with the old credential, sending the
	// client a combination it never agreed to and drawing a rejection that looks
	// like a dead back channel.
	params atomic.Pointer[cbParams]

	// paramsGen numbers the publications of params. A probe records the number
	// it ran against and drops its verdict if it is no longer current: probes
	// are asynchronous and BACKCHANNEL_CTL starts a new one without being able
	// to stop the old, so without this an earlier probe can finish later and
	// publish a verdict about parameters the session no longer has.
	paramsGen atomic.Uint64

	// probeMu guards the one-CB_NULL-per-session admission below. A mutex, not
	// a pair of atomics: the decision to stop and the release of probeRunning
	// have to be one step, and two lock-free flags cannot make them one. Two
	// attempts at that handoff traded an overlap race for a lost-wakeup race —
	// release-then-check lets a caller start a second probe alongside this one,
	// check-then-release lets a request arrive between the check and the release
	// and be consumed by nobody, leaving the new parameters unprobed and
	// delegations off until something else happens to run a probe. This is one
	// probe per BACKCHANNEL_CTL, so the lock costs nothing worth having.
	probeMu      sync.Mutex
	probeRunning bool // a probe is running; a second caller queues instead
	probeQueued  bool // someone asked while one ran; the runner re-runs for them

	queue chan CallbackRequest
	sm    *StateManager

	slotTable *SlotTable

	stopCh chan struct{}

	// nextCBSeqID is the per-slot CB_SEQUENCE seqID counter (RFC 8881
	// §2.10.6.1). It is independent of nextCallbackXID, the package-level RPC
	// XID counter: the backchannel uses a single slot, so a single monotonic
	// counter incrementing by exactly 1 per send is correct. Zero-value starts
	// at 0, giving first seqID=1 on first Add(1).
	nextCBSeqID atomic.Uint32

	callbackTimeout time.Duration
}

// NewBackchannelSender creates a new BackchannelSender for the given session.
func NewBackchannelSender(
	sessionID types.SessionId4,
	clientID uint64,
	cbProgram uint32,
	secParms []types.CallbackSecParms4,
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
	bs.setParams(cbProgram, secParms)
	return bs
}

// cbParams is one negotiated pair of callback parameters. A nil cred means the
// client offered nothing usable and the server's own AUTH_SYS credential
// applies.
type cbParams struct {
	program uint32
	cred    []byte
	// generation identifies this publication. See BackchannelSender.paramsGen.
	generation uint64
}

// setParams encodes the credential and publishes it with the program number as
// a single value, so no callback can observe half of an update.
func (bs *BackchannelSender) setParams(program uint32, secParms []types.CallbackSecParms4) {
	gen := bs.paramsGen.Add(1)
	bs.params.Store(&cbParams{program: program, cred: EncodeCallbackCred(secParms), generation: gen})
}

// currentParams returns the callback parameters as one consistent pair.
func (bs *BackchannelSender) currentParams() cbParams {
	if p := bs.params.Load(); p != nil {
		return *p
	}
	return cbParams{}
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
			// Claim the request before any work. A caller that gave up on the
			// queue wait may have moved on from the state this callback
			// describes, so a lost claim drops it rather than recalling a
			// delegation the server has already finished with.
			if req.Dequeue != nil && !req.Dequeue.take() {
				logger.Debug("BackchannelSender dropping abandoned queued request",
					"session_id", bs.sessionID.String())
				continue
			}
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
	// The first failure that actually reached a socket, if any. It outranks a
	// later errCallbackNotAttempted when the two disagree about what happened.
	var firstTransportErr error

	for attempt := 0; attempt < backchannelMaxRetries; attempt++ {
		if attempt > 0 {
			delay := backchannelRetryDelays[attempt-1]
			logger.Debug("BackchannelSender retrying after delay",
				"session_id", bs.sessionID.String(),
				"attempt", attempt+1,
				"delay", delay)

			select {
			case <-ctx.Done():
				// A transport failure already seen outranks this: the callback
				// did reach a socket and fail there, and reporting the
				// cancellation instead would have the recall treat a dead path
				// as one that was never tried.
				if req.ResultCh != nil {
					if firstTransportErr != nil {
						req.ResultCh <- firstTransportErr
					} else {
						req.ResultCh <- fmt.Errorf("%w: %w", errCallbackNotAttempted, ctx.Err())
					}
				}
				return
			case <-bs.stopCh:
				if req.ResultCh != nil {
					if firstTransportErr != nil {
						req.ResultCh <- firstTransportErr
					} else {
						req.ResultCh <- fmt.Errorf("%w: backchannel sender stopped", errCallbackNotAttempted)
					}
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
		if errors.Is(err, errCallbackRejected) {
			// The client answered. Retrying replays a request it already
			// rejected, and the transport is demonstrably fine — so this
			// outranks an earlier transport error rather than being masked by
			// one: a path that carried a reply is not a dead path.
			if req.ResultCh != nil {
				req.ResultCh <- err
			}
			return
		}
		if errors.Is(err, errCallbackNotAttempted) {
			// Nothing reached a socket, so there is nothing a backoff can
			// improve: this session has no back-bound connection and will not
			// grow one while this goroutine sleeps. Retrying held the recall
			// here for the whole backoff before the caller could try the
			// client's next session, and marking a path fault would blame the
			// client for a route that was never attempted. Reported straight
			// through instead — unless an earlier attempt did reach the
			// transport, whose failure is the honest verdict and must not be
			// masked by this one.
			if firstTransportErr != nil {
				lastErr = firstTransportErr
				break
			}
			if req.ResultCh != nil {
				req.ResultCh <- err
			}
			return
		}
		if firstTransportErr == nil {
			firstTransportErr = err
		}
		logger.Warn("BackchannelSender callback failed",
			"session_id", bs.sessionID.String(),
			"attempt", attempt+1,
			"error", err)
	}

	// All retries exhausted. A transport failure seen on any attempt outranks a
	// later local one: the path did fail, and reporting the local error would
	// have the caller treat a dead callback route as though it were never tried.
	if firstTransportErr != nil {
		lastErr = firstTransportErr
	}
	if req.ResultCh != nil {
		req.ResultCh <- fmt.Errorf("backchannel callback failed after %d attempts: %w",
			backchannelMaxRetries, lastErr)
	}

	// Mark backchannel fault on persistent failure
	bs.sm.setBackchannelFault(bs.clientID, true)
}

// worstCaseSendDuration is the longest sendCallbackWithRetry can run before it
// is guaranteed to have reported: every attempt timing out, plus every backoff
// between them.
func (bs *BackchannelSender) worstCaseSendDuration() time.Duration {
	// An attempt spends up to two writes before it waits for the reply: the
	// connection it picked, and the alternate it falls back to when that write
	// fails. Each is bounded by the same budget as the reply wait
	// (CallbackWriteTimeout), so all three are charged at callbackTimeout.
	total := time.Duration(backchannelMaxRetries) * 3 * bs.callbackTimeout
	for i := 0; i < backchannelMaxRetries-1; i++ {
		total += backchannelRetryDelays[i]
	}
	return total
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
	xid := nextCallbackXID.Add(1)
	params := bs.currentParams()
	callMsg := BuildCBRPCCallMessage(xid, params.program, types.NFS4_CALLBACK_VERSION, types.CB_PROC_COMPOUND, compoundArgs, params.cred)

	// 5. Add record marking
	framedMsg := AddCBRecordMark(callMsg, true)

	// 6/7. Find a back-bound connection and register the XID on its reply table.
	connID, writer, pending, replyCh, ok := bs.selectBackBoundWaiter(xid, map[uint64]bool{})
	if !ok {
		return fmt.Errorf("%w: no back-bound connection for session %s",
			errCallbackNotAttempted, bs.sessionID.String())
	}

	// 8. Write framed message (no lock held -- ConnWriter acquires writeMu internally)
	if err := writer(framedMsg); err != nil {
		pending.Cancel(xid)
		logger.Debug("BackchannelSender write failed, trying alternate connection",
			"session_id", bs.sessionID.String(),
			"conn_id", connID,
			"error", err)

		// Retry on another back-bound connection. The selector walks past any
		// further candidates that cannot be registered, so a write failure on
		// the freshest connection does not end the attempt while a usable one
		// remains. The write above did reach a transport and fail there, so the
		// original failure is what propagates if no alternate is left: reporting
		// the retired-alternate case as "never attempted" would have the recall
		// classify the whole send as local and leave CBPathUp standing.
		connID2, writer2, pending2, replyCh2, ok2 := bs.selectBackBoundWaiter(xid, map[uint64]bool{connID: true})
		if !ok2 {
			return fmt.Errorf("write to back-bound connection %d failed and no alternate: %w", connID, err)
		}
		// Update pending to the new connection's PendingCBReplies so the
		// timeout path below cancels the correct waiter.
		pending = pending2
		// Already registered: selectBackBoundWaiter registers the XID on the
		// table it hands back, session-tagged, so the alternate's waiter is
		// routable and addressable by session the moment it is chosen.
		replyCh = replyCh2
		if err2 := writer2(framedMsg); err2 != nil {
			pending.Cancel(xid)
			return fmt.Errorf("write to alternate connection %d also failed: %w", connID2, err2)
		}
	}

	// 9. Wait for reply with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, bs.callbackTimeout)
	defer cancel()

	select {
	case <-bs.stopCh:
		// This session is gone — DESTROY_SESSION or shutdown. Waiting out the
		// callback timeout for a reply nobody will route holds the sender for
		// up to that long after its session was destroyed, and the outcome is
		// local either way. The probe already waits on this; the send did not.
		pending.Cancel(xid)
		return fmt.Errorf("%w: backchannel sender stopped while the callback was in flight",
			errCallbackNotAttempted)

	case <-timeoutCtx.Done():
		// A deadline that fires alongside something on the reply channel is not
		// a timeout. Two cases, and both were being reported as the client
		// failing to answer: a retired table, which is a socket on this side
		// going away, and an actual reply that landed in the same instant —
		// classifying THAT as failed clears CBPathUp and revokes a delegation
		// the client answered for.
		select {
		case replyBytes, open := <-replyCh:
			if !open {
				pending.Cancel(xid)
				return fmt.Errorf("%w: connection %d was retired while the callback was in flight",
					errCallbackNotAttempted, connID)
			}
			if err := ValidateCBReply(replyBytes); err != nil {
				return fmt.Errorf("%w: %w", errCallbackRejected, err)
			}
			bs.sm.setBackchannelFault(bs.clientID, false)
			return nil
		default:
		}
		if pending.Cancel(xid) {
			// The table was retired while this waited. Teardown can land between
			// the reply check above and here, and reporting that as a timeout
			// has the retry loop call it no callback path at all — CBPathUp
			// cleared and a delegation revoked because a socket on this side
			// went away.
			return fmt.Errorf("%w: connection %d was retired while the callback was in flight",
				errCallbackNotAttempted, connID)
		}
		return fmt.Errorf("backchannel callback timed out after %s", bs.callbackTimeout)
	case replyBytes, open := <-replyCh:
		if !open {
			// FailAll closed the waiter: this connection was retired while the
			// callback was in flight. The bytes did reach a transport, so this
			// is not "never attempted" in the literal sense — but what happened
			// to them is unknown, and the one thing it is NOT is evidence that
			// the client stopped answering. Falling through to ValidateCBReply
			// would read the closed channel as a malformed reply, which the
			// retry loop classifies as no callback path at all: CBPathUp
			// cleared and a delegation revoked because a socket on this side
			// went away. The sentinel keeps the outcome local, where it
			// belongs, and leaves the client's verdict to a send that reached
			// a conclusion.
			return fmt.Errorf("%w: connection %d was retired while the callback was in flight",
				errCallbackNotAttempted, connID)
		}
		// 10. Validate CB_COMPOUND reply
		if err := ValidateCBReply(replyBytes); err != nil {
			return fmt.Errorf("%w: %w", errCallbackRejected, err)
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
// errCallbackNotAttempted marks a callback failure that carries no evidence
// about the client: the send never reached the wire for this session, because
// the sender was stopped or cancelled or because this session has no back-bound
// connection at all. A recall must not count one of these against the client's
// callback path — another of its sessions may still carry the recall, and
// clearing CBPathUp on this would withhold delegations from a client that never
// stopped answering.
//
// probeCallbackPath deliberately does not use it: there, a missing back-bound
// connection is the verdict, not an excuse for withholding one.
var errCallbackNotAttempted = errors.New("callback not attempted on this session")

// errCallbackRejected marks a callback the client ANSWERED and the answer was
// not one this server accepts — an RPC or NFS error status, or a reply that
// does not decode. The delegation is as unrecalled as if the send had failed,
// so the caller still revokes; what must not follow is clearing CBPathUp or
// raising a backchannel fault, because the peer demonstrably received the
// callback and replied to it. Retrying does not help either: the same request
// gets the same rejection.
var errCallbackRejected = errors.New("callback rejected by the client")

// selectBackBoundWaiter picks a back-bound connection for this session and
// registers xid on its reply table, returning both so the caller writes and
// waits on the same connection.
//
// A connection can be retired between the lookup and the register — the two do
// not share a hold on connMu — and that says nothing about the client's callback
// path, only that this particular socket went away. Treating it as the answer
// would report a send as never attempted, or a probe as a path failure, while
// another back-bound connection for the same session sat there usable: for a
// recall that means revoking a delegation over a path that was still reachable,
// and for a probe it means withholding delegations until the next parameter
// update, because nothing retries a probe.
//
// So a retired table costs a candidate, and the walk continues until the
// session's back-bound bindings are exhausted. Stopping after two is not safe:
// a session may hold up to maxConnsPerSession bindings, and the freshest ones
// are exactly the ones most likely to be unusable — a binding exists before its
// writer is registered and outlives it once the connection is retired, so both
// ends of that window land on the most recently active candidates. The walk is
// bounded by the binding count: every iteration excludes the connection it was
// handed, so it terminates when the candidates run out.
//
// tried holds the connections already handed out, so a caller that walks more
// than once (a probe retrying after a failed write) does not revisit them. The
// caller owns the map.
func (bs *BackchannelSender) selectBackBoundWaiter(xid uint32, tried map[uint64]bool) (uint64, ConnWriter, *PendingCBReplies, chan []byte, bool) {
	for {
		id, writer, pending, ok := bs.sm.getBackBoundConnWriterExcluding(bs.sessionID, tried)
		if !ok {
			return 0, nil, nil, nil, false
		}
		if replyCh, registered := pending.Register(xid, bs.sessionID); registered {
			return id, writer, pending, replyCh, true
		}
		logger.Debug("BackchannelSender: back-bound connection retired before registration, trying another",
			"session_id", bs.sessionID.String(), "conn_id", id)
		tried[id] = true
	}
}

func (bs *BackchannelSender) probeCallbackPath(ctx context.Context) error {
	xid := nextCallbackXID.Add(1)
	params := bs.currentParams()
	callMsg := BuildCBRPCCallMessage(xid, params.program, types.NFS4_CALLBACK_VERSION, types.CB_PROC_NULL, nil, params.cred)
	framedMsg := AddCBRecordMark(callMsg, true)

	// A verdict here is durable in a way a send's is not: it is published on the
	// client record, and nothing re-probes until the next parameter update. So a
	// socket that went away must not be reported as the client failing to answer
	// — with a second back-bound connection for this session sitting usable, that
	// withholds delegations indefinitely on the strength of the wrong connection.
	// A write failure and a reply table retired mid-wait both cost a candidate;
	// a timeout does not, because a client that took the bytes and said nothing
	// is exactly what this is asking about.
	var lastErr error
	tried := make(map[uint64]bool)
	for {
		connID, writer, pending, replyCh, ok := bs.selectBackBoundWaiter(xid, tried)
		if !ok {
			break
		}
		if err := writer(framedMsg); err != nil {
			pending.Cancel(xid)
			lastErr = fmt.Errorf("write CB_NULL to back-bound connection %d: %w", connID, err)
			tried[connID] = true
			continue
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, bs.callbackTimeout)
		select {
		case <-timeoutCtx.Done():
			cancel()
			// Both can be ready at once, and a select with two ready cases picks
			// uniformly — so arriving here does not mean nothing answered. A
			// retired table means a socket on this side went away, which is not
			// the client's verdict; a reply that landed in the same instant IS
			// the client's verdict, and calling it a timeout publishes "does not
			// answer callbacks" about a client that just did. Nothing re-probes
			// until the next parameter update, so that one stands. Ask first.
			select {
			case replyBytes, open := <-replyCh:
				if !open {
					pending.Cancel(xid)
					lastErr = fmt.Errorf("back-bound connection %d was retired while CB_NULL was in flight", connID)
					tried[connID] = true
					continue
				}
				if err := ValidateCBReply(replyBytes); err != nil {
					return fmt.Errorf("CB_NULL reply: %w", err)
				}
				return nil
			default:
			}
			pending.Cancel(xid)
			return fmt.Errorf("CB_NULL timed out after %s", bs.callbackTimeout)
		case <-bs.stopCh:
			cancel()
			pending.Cancel(xid)
			return fmt.Errorf("backchannel sender stopped")
		case replyBytes, open := <-replyCh:
			cancel()
			if !open {
				// FailAll closed the waiter: the connection was retired while
				// this was waiting on it, which says nothing about the client.
				lastErr = fmt.Errorf("back-bound connection %d was retired while CB_NULL was in flight", connID)
				tried[connID] = true
				continue
			}
			if err := ValidateCBReply(replyBytes); err != nil {
				return fmt.Errorf("CB_NULL reply: %w", err)
			}
			return nil
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return fmt.Errorf("no back-bound connection for session %s", bs.sessionID.String())
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
	// One probe per session at a time. Each BACKCHANNEL_CTL used to launch
	// another while the previous one was still waiting out its callback
	// timeout, so a client that renegotiates repeatedly without ever answering
	// CB_NULL accumulated a goroutine, an XID and a pending waiter per update —
	// and every one of them wrote to the same connection. The generation check
	// below discarded their verdicts but not their cost. The one in flight is
	// kept rather than replaced: it is already waiting, and its verdict is
	// discarded anyway if the parameters move under it.
	bs.probeMu.Lock()
	if bs.probeRunning {
		// Queued, not dropped. The probe already running was started against
		// parameters that have since been replaced, so its verdict will be
		// discarded for a stale generation — and if this one simply returned,
		// nothing would ever evaluate the new parameters and delegations would
		// stay withheld until the next control update that happened to arrive
		// when no probe was running.
		bs.probeQueued = true
		bs.probeMu.Unlock()
		logger.Debug("CB_NULL probe deferred: one is already in flight for this session",
			"client_id", fmt.Sprintf("0x%x", bs.clientID),
			"session_id", bs.sessionID.String())
		return
	}
	bs.probeRunning = true
	bs.probeMu.Unlock()

	for {
		sm.runV41Probe(ctx, bs)

		// Deciding to stop and releasing the flag are one critical section. Any
		// other order leaves a window: release first and a caller starts a
		// second probe beside this one; decide first and a caller's request
		// lands after the decision and is consumed by nobody.
		bs.probeMu.Lock()
		if !bs.probeQueued {
			bs.probeRunning = false
			bs.probeMu.Unlock()
			return
		}
		bs.probeQueued = false
		bs.probeMu.Unlock()

		// A deferred re-run is about parameters that have changed since the
		// caller was turned away, so it reads them fresh; the caller's context
		// is gone, which is why this one is detached.
		ctx = context.Background()
	}
}

// runV41Probe is one CB_NULL round trip and its verdict. The caller owns the
// one-probe-per-session guard.
func (sm *StateManager) runV41Probe(ctx context.Context, bs *BackchannelSender) {
	generation := bs.currentParams().generation
	err := bs.probeCallbackPath(ctx)

	// The parameters this probe ran against may since have been replaced, in
	// which case what it learned is about a configuration the session no longer
	// has. Publishing it would either re-enable delegations on retired
	// parameters or overwrite the verdict of the probe that replaced this one.
	if !sm.setCBPathUpIfCurrent(bs, generation, err == nil) {
		logger.Debug("CB_NULL result discarded: callback parameters changed while the probe was in flight",
			"client_id", fmt.Sprintf("0x%x", bs.clientID),
			"session_id", bs.sessionID.String())
		return
	}

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

// setCBPathUpIfCurrent publishes a probe verdict, but only if the callback
// parameters the probe ran against are still the ones the sender holds. It
// reports whether the verdict was published.
//
// The generation is re-read under sm.mu together with the write rather than
// before it: a caller that checks the generation and then calls setCBPathUp
// leaves a window in which the parameters are replaced between the two, and the
// retired verdict lands on the record anyway.
//
// Thread-safe: acquires sm.mu.Lock.
func (sm *StateManager) setCBPathUpIfCurrent(bs *BackchannelSender, generation uint64, up bool) bool {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if bs.currentParams().generation != generation {
		return false
	}
	if record := sm.clientRecordLocked(bs.clientID); record != nil {
		record.CBPathUp = up
	}
	return true
}

// SetMaxConnectionsPerSession sets the maximum number of connections per session.
// A value of 0 means unlimited (no limit enforced).
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

// EnsureBackchannelWriterForConn registers a back-channel writer for a
// connection only while that connection still has a binding that can carry the
// back channel, and returns the back-capable bindings it found.
//
// The binding check and the registration are one decision, taken under
// sm.connMu. Done separately they race a rebind: a caller that collects the
// bindings, then registers, can land its registration after a concurrent rebind
// dropped the last back-capable binding — re-installing a writer on a
// connection whose callback state was just released, which is the state the
// release exists to prevent. A connection is therefore either back-capable and
// registered, or neither.
//
// writerFn is called only when a registration is needed, so a caller can avoid
// building the writer closure on the common already-registered path.
// Thread-safe: acquires sm.connMu.Lock.
func (sm *StateManager) EnsureBackchannelWriterForConn(connectionID uint64, writerFn func() ConnWriter) []*BoundConnection {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	var backBound []*BoundConnection
	for _, b := range sm.connByID[connectionID] {
		if carriesBackChannel(b.Direction) {
			backBound = append(backBound, b)
		}
	}
	if len(backBound) == 0 {
		return nil
	}

	// Re-register when the state no longer tracks a writer: an unbind or rebind
	// clears it, and the sender must not be left writerless. An existing
	// demultiplexer is reused by RegisterConnWriter, so a concurrent first-time
	// registration cannot strand replies already being waited on.
	if sm.connWriters[connectionID] == nil {
		sm.RegisterConnWriterLocked(connectionID, writerFn())
	}
	return backBound
}

// RegisterConnWriterLocked is RegisterConnWriter for a caller already holding
// sm.connMu. Caller must hold sm.connMu.
func (sm *StateManager) RegisterConnWriterLocked(connectionID uint64, writer ConnWriter) *PendingCBReplies {
	sm.connWriters[connectionID] = writer
	if pending := sm.cbRepliesByConn[connectionID]; pending != nil {
		return pending
	}
	pending := NewPendingCBReplies()
	sm.cbRepliesByConn[connectionID] = pending
	return pending
}

// RegisterConnWriter registers a ConnWriter callback for a back-bound connection.
// Called by the NFS adapter when a connection is bound for back-channel.
// Also creates a PendingCBReplies instance for the connection.
//
// Thread-safe: acquires sm.connMu.Lock.

func (sm *StateManager) RegisterConnWriter(connectionID uint64, writer ConnWriter) *PendingCBReplies {
	sm.connMu.Lock()
	defer sm.connMu.Unlock()

	// COMPOUNDs on one connection are dispatched concurrently, so two of them
	// can reach first-time registration together. Handing the second one a
	// fresh demultiplexer would strand every reply the first is already
	// waiting on, so an existing one is reused (see RegisterConnWriterLocked).
	return sm.RegisterConnWriterLocked(connectionID, writer)
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
	// Every COMPOUND on a back-bound connection reaches here, and all but the
	// first find the sender already running, so settle that under a read lock
	// rather than serializing the whole fore channel behind sm.mu. The write
	// path below re-checks, which is what makes the race between the two
	// harmless.
	sm.mu.RLock()
	started := sm.sessionsByID[sessionID] != nil && sm.sessionsByID[sessionID].backchannelSender != nil
	sm.mu.RUnlock()
	if started {
		return
	}

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
		session.BackchannelSecParms,
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

	// A sender exists per back-bound session, so a client with several sessions
	// has several. Pick one whose session still has a back-bound connection:
	// returning the first sender found would keep choosing a session whose
	// connection has since closed, and every recall through it would fail with
	// no back-bound connection and revoke the delegation while a sibling session
	// of the same client still had a live path to the client.
	//
	// Lock ordering: sm.mu is held here and getBackBoundConnWriter takes
	// connMu, which is the documented direction (manager.go, sm.mu before
	// connMu, never reverse).
	var fallback *BackchannelSender
	for _, session := range sm.sessionsByClientID[clientID] {
		if session.backchannelSender == nil {
			continue
		}
		if fallback == nil {
			fallback = session.backchannelSender
		}
		if _, _, _, ok := sm.getBackBoundConnWriter(session.SessionID, 0); ok {
			return session.backchannelSender
		}
	}
	// No session has a live back binding. Returning a sender anyway keeps the
	// caller on its existing "no back-bound connection" path, which is the
	// honest outcome, rather than the different one a nil sender takes.
	return fallback
}

// getBackBoundConnWriter finds a back-bound connection for the session,
// optionally excluding a specific connection ID (pass 0 for no exclusion).
// Selects the connection with the most recent fore-channel activity.
//
// Lock ordering: acquires sm.connMu.RLock only (no sm.mu needed).

func (sm *StateManager) getBackBoundConnWriter(sessionID types.SessionId4, excludeConnID uint64) (uint64, ConnWriter, *PendingCBReplies, bool) {
	var tried map[uint64]bool
	if excludeConnID != 0 {
		tried = map[uint64]bool{excludeConnID: true}
	}
	return sm.getBackBoundConnWriterExcluding(sessionID, tried)
}

// getBackBoundConnWriterExcluding finds a back-bound connection for the session
// that is not in tried, which the caller owns and must not mutate afterwards.
//
// A set rather than a single excluded ID because the walk that needs it retries:
// a candidate can be handed back and then fail to register, and the caller must
// not be offered it again. Lock ordering: acquires sm.connMu.RLock only.

func (sm *StateManager) getBackBoundConnWriterExcluding(sessionID types.SessionId4, tried map[uint64]bool) (uint64, ConnWriter, *PendingCBReplies, bool) {
	sm.connMu.RLock()
	defer sm.connMu.RUnlock()

	return sm.getBackBoundConnWriterLocked(sessionID, tried)
}

// getBackBoundConnWriterLocked is the common implementation for finding a
// back-bound connection. Caller must hold sm.connMu.RLock.

func (sm *StateManager) getBackBoundConnWriterLocked(sessionID types.SessionId4, tried map[uint64]bool) (uint64, ConnWriter, *PendingCBReplies, bool) {
	// Most recently active FIRST, but not most recently active ONLY. A binding
	// exists before its writer and reply table are registered, and it outlives
	// them when the connection is retired — so the freshest binding is regularly
	// the one that cannot carry a callback. Answering "no path" on that basis,
	// while an older live connection on the same session sits usable, is how a
	// recall ends up revoking a delegation and a probe ends up publishing a down
	// verdict that nothing re-runs.
	//
	// So: rank the candidates and walk them, rather than picking one and
	// testing it.
	candidates := make([]*BoundConnection, 0, len(sm.connBySession[sessionID]))
	for _, b := range sm.connBySession[sessionID] {
		if tried[b.ConnectionID] {
			continue
		}
		if !carriesBackChannel(b.Direction) {
			continue
		}
		candidates = append(candidates, b)
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].LastActivity.After(candidates[j].LastActivity)
	})

	for _, b := range candidates {
		writer, ok := sm.connWriters[b.ConnectionID]
		if !ok {
			continue
		}
		pending := sm.cbRepliesByConn[b.ConnectionID]
		if pending == nil {
			continue
		}
		return b.ConnectionID, writer, pending, true
	}

	return 0, nil, nil, false
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

	// Republish the program and credential as one pair. The sender's Run
	// goroutine reads them without sm.mu, so the pair is atomic.
	if session.backchannelSender != nil {
		session.backchannelSender.setParams(cbProgram, secParms)

		// The verdict on this client's callback path was reached against the
		// parameters that have just been replaced, so it no longer describes
		// anything. Left standing, OPEN keeps granting delegations whose recall
		// would travel on a credential nothing has tried. Cleared and re-probed:
		// delegations pause until the new parameters answer a CB_NULL, rather
		// than pausing forever, which is what clearing alone would do — the
		// probe in StartBackchannelSender fires once per session and this
		// session already has its sender.
		if record := sm.clientRecordLocked(session.ClientID); record != nil {
			record.CBPathUp = false
		}
		// Not the request's context: the probe deliberately outlives the
		// BACKCHANNEL_CTL reply, and cancelling it when the compound finishes
		// would leave the verdict cleared with nothing on the way to restore it.
		go sm.probeV41CallbackPath(context.Background(), session.backchannelSender)
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
			if carriesBackChannel(b.Direction) {
				return true
			}
		}
	}
	return false
}
