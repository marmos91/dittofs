package handlers

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
	xdrcore "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// recordedCall captures one SM_NOTIFY callback fired by the relay.
type recordedCall struct {
	addr   string
	status *types.Status
	proc   uint32
	prog   uint32
	vers   uint32
}

// fakeDispatcher records every callback the handler relays and can be told to
// fail, so tests exercise dispatch without real sockets.
type fakeDispatcher struct {
	mu    sync.Mutex
	calls []recordedCall
	// failAddrs maps a callback hostname to the error Send should return.
	failAddrs map[string]error
}

func (f *fakeDispatcher) Send(_ context.Context, addr string, status *types.Status, proc, prog, vers uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, recordedCall{addr: addr, status: status, proc: proc, prog: prog, vers: vers})
	if err, ok := f.failAddrs[addr]; ok {
		return err
	}
	return nil
}

func (f *fakeDispatcher) recorded() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]recordedCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// newTestHandler builds a Handler backed by a real ConnectionTracker and the
// given dispatcher (nil disables the relay).
func newTestHandler(t *testing.T, dispatcher notifyDispatcher) *Handler {
	t.Helper()
	tracker := lock.NewConnectionTracker(lock.DefaultConnectionTrackerConfig())
	t.Cleanup(tracker.Close)
	return NewHandler(HandlerConfig{
		Tracker:    tracker,
		ServerName: "test-server",
		Dispatcher: dispatcher,
	})
}

// monitor registers a monitored host via SM_MON so that subsequent NOTIFYs for
// monName arriving from clientAddr pass the H16 gate. clientAddr is the RPC
// source address ("host:port"); callbackHost is the my_id.my_name.
func monitor(t *testing.T, h *Handler, clientAddr, monName, callbackHost string) {
	t.Helper()
	mon := &types.Mon{
		MonID: types.MonID{
			MonName: monName,
			MyID: types.MyID{
				MyName: callbackHost,
				MyProg: 100021,
				MyVers: 4,
				MyProc: 23,
			},
		},
	}
	data := encodeMon(t, mon)
	ctx := &NSMHandlerContext{Context: context.Background(), ClientAddr: clientAddr}
	if _, err := h.Mon(ctx, data); err != nil {
		t.Fatalf("Mon failed: %v", err)
	}
}

// encodeMon hand-encodes a mon argument matching xdr.DecodeMon's layout:
// mon_id { mon_name<>, my_id { my_name<>, prog, vers, proc } }, priv[16].
func encodeMon(t *testing.T, mon *types.Mon) []byte {
	t.Helper()
	var buf bytes.Buffer
	must(t, xdrcore.WriteXDRString(&buf, mon.MonID.MonName))
	must(t, xdrcore.WriteXDRString(&buf, mon.MonID.MyID.MyName))
	must(t, xdrcore.WriteUint32(&buf, mon.MonID.MyID.MyProg))
	must(t, xdrcore.WriteUint32(&buf, mon.MonID.MyID.MyVers))
	must(t, xdrcore.WriteUint32(&buf, mon.MonID.MyID.MyProc))
	buf.Write(mon.Priv[:]) // fixed opaque[16], no length prefix
	return buf.Bytes()
}

// encodeStatChge hand-encodes a stat_chge argument: mon_name<>, state.
func encodeStatChge(t *testing.T, monName string, state int32) []byte {
	t.Helper()
	var buf bytes.Buffer
	must(t, xdrcore.WriteXDRString(&buf, monName))
	must(t, xdrcore.WriteInt32(&buf, state))
	return buf.Bytes()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("encode error: %v", err)
	}
}

// notify drives a single SM_NOTIFY through the handler from srcAddr.
func notify(t *testing.T, h *Handler, srcAddr, monName string, state int32) {
	t.Helper()
	data := encodeStatChge(t, monName, state)
	ctx := &NSMHandlerContext{Context: context.Background(), ClientAddr: srcAddr}
	if _, err := h.Notify(ctx, data); err != nil {
		t.Fatalf("Notify returned error: %v", err)
	}
}

// recordedState returns the last-seen peer state for monName, or (0,false) if
// the handler never recorded one (i.e. no NOTIFY was ever acted on for it).
func recordedState(h *Handler, monName string) (int32, bool) {
	h.peerStateMu.Lock()
	defer h.peerStateMu.Unlock()
	v, ok := h.peerState[monName]
	return v, ok
}

// sanity check that the hand-rolled encoder round-trips through the decoder.
func TestEncodeStatChge_RoundTrips(t *testing.T) {
	data := encodeStatChge(t, "peer.example", 7)
	got, err := xdr.DecodeStatChge(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.MonName != "peer.example" || got.State != 7 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

// ---------------------------------------------------------------------------
// H16 — monitored-list membership + source-address gate
// ---------------------------------------------------------------------------

func TestNotify_UnmonitoredHost_Ignored(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})
	// No SM_MON for "ghost.example".
	notify(t, h, "10.0.0.9:55000", "ghost.example", 5)

	if _, ok := recordedState(h, "ghost.example"); ok {
		t.Fatal("unmonitored NOTIFY must not be acted on (no state recorded)")
	}
}

func TestNotify_SourceAddrMismatch_Rejected(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})
	// We monitor peer.example as seen from 10.0.0.5.
	monitor(t, h, "10.0.0.5:601", "peer.example", "203.0.113.5")

	// NOTIFY for peer.example arrives from a DIFFERENT source IP.
	notify(t, h, "10.0.0.66:55000", "peer.example", 5)

	if _, ok := recordedState(h, "peer.example"); ok {
		t.Fatal("source-addr mismatch NOTIFY must be rejected (no state recorded)")
	}
}

func TestNotify_MonitoredHostRightAddr_Accepted(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})
	monitor(t, h, "10.0.0.5:601", "peer.example", "203.0.113.5")

	// Legitimate NOTIFY: monitored mon_name, matching source IP (ephemeral
	// NOTIFY port differs from the SM_MON port — only the IP must match).
	notify(t, h, "10.0.0.5:55000", "peer.example", 5)

	got, ok := recordedState(h, "peer.example")
	if !ok {
		t.Fatal("legitimate NOTIFY must pass the gate and record state")
	}
	if got != 5 {
		t.Fatalf("expected recorded state 5, got %d", got)
	}
}

// isMonitoredFromSource focused unit coverage.
func TestIsMonitoredFromSource(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})
	monitor(t, h, "192.168.1.10:700", "host-a", "203.0.113.6")

	if !h.isMonitoredFromSource("host-a", "192.168.1.10:40000") {
		t.Error("monitored host + matching IP should pass")
	}
	if h.isMonitoredFromSource("host-a", "192.168.1.11:40000") {
		t.Error("monitored host + wrong IP must fail")
	}
	if h.isMonitoredFromSource("host-b", "192.168.1.10:40000") {
		t.Error("unmonitored mon_name must fail")
	}
	if h.isMonitoredFromSource("host-a", "") {
		t.Error("empty source addr must fail")
	}
}

// ---------------------------------------------------------------------------
// H17 — state-number monotonicity
// ---------------------------------------------------------------------------

func TestNotify_Monotonicity_OnlyHigherActs(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})
	monitor(t, h, "10.0.0.5:601", "peer.example", "203.0.113.5")

	// First NOTIFY at state 5 -> accepted, recorded.
	notify(t, h, "10.0.0.5:40001", "peer.example", 5)
	if got, _ := recordedState(h, "peer.example"); got != 5 {
		t.Fatalf("after first NOTIFY want state 5, got %d", got)
	}

	// Replay of the SAME state 5 -> ignored, stored state unchanged.
	notify(t, h, "10.0.0.5:40002", "peer.example", 5)
	if got, _ := recordedState(h, "peer.example"); got != 5 {
		t.Fatalf("replay must not change stored state; got %d", got)
	}

	// Lower state 3 -> ignored.
	notify(t, h, "10.0.0.5:40003", "peer.example", 3)
	if got, _ := recordedState(h, "peer.example"); got != 5 {
		t.Fatalf("decreasing state must not change stored state; got %d", got)
	}

	// Strictly higher state 6 -> accepted, stored advances.
	notify(t, h, "10.0.0.5:40004", "peer.example", 6)
	if got, _ := recordedState(h, "peer.example"); got != 6 {
		t.Fatalf("increasing state must advance stored state; got %d", got)
	}
}

func TestAdmitPeerState(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})

	if !h.admitPeerState("p", 1) {
		t.Error("first state should be admitted")
	}
	if h.admitPeerState("p", 1) {
		t.Error("equal state (replay) must be rejected")
	}
	if h.admitPeerState("p", 0) {
		t.Error("lower state must be rejected")
	}
	if !h.admitPeerState("p", 2) {
		t.Error("higher state must be admitted")
	}
	// Independent key.
	if !h.admitPeerState("q", 1) {
		t.Error("first state for a different key must be admitted")
	}
}

// TestAdmitPeerState_Wraparound exercises RFC 1982 serial-number arithmetic.
// A state counter wrapping past MaxInt32 must be admitted (it is a legitimate
// forward advance), not rejected by the old signed-int32 comparison.
func TestAdmitPeerState_Wraparound(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})

	// Seed the stored state at math.MaxInt32 (0x7FFFFFFF).
	maxI32 := int32(math.MaxInt32)
	h.peerStateMu.Lock()
	h.peerState["wrap"] = maxI32
	h.peerStateMu.Unlock()

	// The next legitimate state is MaxInt32 + 1 = MinInt32 (0x80000000 as uint32).
	// Signed comparison: MinInt32 < MaxInt32 → old code wrongly rejects.
	// RFC 1982: diff = uint32(MinInt32) - uint32(MaxInt32) = 1 → admitted.
	// Compute via uint32 wrap to avoid an int32 constant-overflow at compile time.
	next := int32(uint32(maxI32) + 1) // = math.MinInt32
	if !h.admitPeerState("wrap", next) {
		t.Fatal("state wrap MaxInt32 → MinInt32 must be admitted (RFC 1982); old signed comparison would reject it")
	}
	if got, _ := recordedState(h, "wrap"); got != next {
		t.Fatalf("stored state after wrap: want %d, got %d", next, got)
	}

	// A state that is 2^31 ahead or more (half the uint32 space) is "backwards":
	// diff = uint32(MaxInt32) - uint32(MinInt32) = 0xFFFFFFFF → high bit set → stale.
	if h.admitPeerState("wrap", maxI32) {
		t.Fatal("MaxInt32 after MinInt32 is retrograde; must be rejected")
	}

	// Exact replay of next must be rejected.
	if h.admitPeerState("wrap", next) {
		t.Fatal("exact replay must be rejected")
	}
}

// TestAdmitPeerState_Wraparound_NegativeStored covers the scenario where the
// stored value is already negative (i.e. we are past the wrap point) and a
// further legitimate increment arrives.
func TestAdmitPeerState_Wraparound_NegativeStored(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})

	// Stored = MinInt32 + 1 (one past the wrap point).
	stored := int32(math.MinInt32 + 1)
	h.peerStateMu.Lock()
	h.peerState["neg"] = stored
	h.peerStateMu.Unlock()

	// Next = MinInt32 + 2: RFC 1982 diff = 1 → admitted.
	next := stored + 1
	if !h.admitPeerState("neg", next) {
		t.Fatalf("state advance from %d to %d must be admitted", stored, next)
	}

	// Previous = MinInt32: RFC 1982 diff = 0xFFFFFFFF → stale → rejected.
	prev := int32(math.MinInt32)
	if h.admitPeerState("neg", prev) {
		t.Fatalf("retrograde state %d after %d must be rejected", prev, stored)
	}
}

// Replay that passes H16 but fails H17 must remain a no-op (defence in depth):
// the monotonicity gate sits AFTER the address gate, so a monitored host
// replaying an old state still does nothing.
func TestNotify_MonitoredReplay_NoOp(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})
	monitor(t, h, "10.0.0.5:601", "peer.example", "203.0.113.5")

	notify(t, h, "10.0.0.5:40001", "peer.example", 9)
	notify(t, h, "10.0.0.5:40002", "peer.example", 9) // duplicate

	if got, _ := recordedState(h, "peer.example"); got != 9 {
		t.Fatalf("monitored replay must be a no-op; got %d", got)
	}
}

// ---------------------------------------------------------------------------
// Relay dispatch (#218)
// ---------------------------------------------------------------------------

// monitorWithCallback registers a monitor with an explicit callback host and
// priv, so dispatch targets and payloads can be asserted.
func monitorWithCallback(t *testing.T, h *Handler, clientAddr, monName, callbackHost string, priv [16]byte) {
	t.Helper()
	mon := &types.Mon{
		MonID: types.MonID{
			MonName: monName,
			MyID: types.MyID{
				MyName: callbackHost,
				MyProg: 100021,
				MyVers: 4,
				MyProc: 23,
			},
		},
		Priv: priv,
	}
	data := encodeMon(t, mon)
	ctx := &NSMHandlerContext{Context: context.Background(), ClientAddr: clientAddr}
	if _, err := h.Mon(ctx, data); err != nil {
		t.Fatalf("Mon failed: %v", err)
	}
}

// A NOTIFY passing both gates relays a callback to every local monitor of that
// mon_name, each carrying its own priv and the rebooted peer's mon_name/state.
func TestNotify_DispatchesToAllMonitors(t *testing.T) {
	fd := &fakeDispatcher{}
	h := newTestHandler(t, fd)

	privA := [16]byte{1, 2, 3}
	privB := [16]byte{9, 9, 9}
	// Callback hosts must be valid (non-loopback, non-link-local) IP literals
	// per the SSRF guard.
	const clientA = "203.0.113.10"
	const clientB = "203.0.113.11"
	const clientC = "203.0.113.12"
	// Two distinct local clients both monitor peer.example from the same host.
	monitorWithCallback(t, h, "10.0.0.5:601", "peer.example", clientA, privA)
	monitorWithCallback(t, h, "10.0.0.5:602", "peer.example", clientB, privB)
	// A third client monitors a different host — must NOT be notified.
	monitorWithCallback(t, h, "10.0.0.5:603", "other.example", clientC, [16]byte{7})

	notify(t, h, "10.0.0.5:55000", "peer.example", 5)

	calls := fd.recorded()
	if len(calls) != 2 {
		t.Fatalf("expected 2 callbacks, got %d", len(calls))
	}

	byAddr := map[string]recordedCall{}
	for _, c := range calls {
		byAddr[c.addr] = c
		if c.status.MonName != "peer.example" {
			t.Errorf("callback to %s: mon_name = %q, want peer.example", c.addr, c.status.MonName)
		}
		if c.status.State != 5 {
			t.Errorf("callback to %s: state = %d, want 5", c.addr, c.status.State)
		}
		if c.proc != 23 || c.prog != 100021 || c.vers != 4 {
			t.Errorf("callback to %s: prog/vers/proc = %d/%d/%d, want 100021/4/23", c.addr, c.prog, c.vers, c.proc)
		}
	}
	if _, ok := byAddr[clientC]; ok {
		t.Fatal("client-c monitors a different host and must not receive a callback")
	}
	if got := byAddr[clientA].status.Priv; got != privA {
		t.Errorf("client-a priv = %v, want %v", got, privA)
	}
	if got := byAddr[clientB].status.Priv; got != privB {
		t.Errorf("client-b priv = %v, want %v", got, privB)
	}
}

// A NOTIFY that fails the H16 gate must produce zero dispatch (no side effects).
func TestNotify_GateH16Fail_NoDispatch(t *testing.T) {
	fd := &fakeDispatcher{}
	h := newTestHandler(t, fd)
	monitorWithCallback(t, h, "10.0.0.5:601", "peer.example", "203.0.113.10", [16]byte{})

	// Wrong source IP -> H16 fails.
	notify(t, h, "10.0.0.66:55000", "peer.example", 5)

	if calls := fd.recorded(); len(calls) != 0 {
		t.Fatalf("H16 failure must not dispatch; got %d callbacks", len(calls))
	}
}

// A NOTIFY that fails the H17 gate (replay/stale) must produce zero new
// dispatch beyond the first admitted notification.
func TestNotify_GateH17Fail_NoDispatch(t *testing.T) {
	fd := &fakeDispatcher{}
	h := newTestHandler(t, fd)
	monitorWithCallback(t, h, "10.0.0.5:601", "peer.example", "203.0.113.10", [16]byte{})

	notify(t, h, "10.0.0.5:40001", "peer.example", 5) // admitted -> 1 callback
	notify(t, h, "10.0.0.5:40002", "peer.example", 5) // replay -> no callback
	notify(t, h, "10.0.0.5:40003", "peer.example", 3) // stale -> no callback

	if calls := fd.recorded(); len(calls) != 1 {
		t.Fatalf("only the admitted NOTIFY may dispatch; got %d callbacks", len(calls))
	}
}

// A failing callback is accounted for but does not abort the remaining ones.
func TestNotify_CallbackFailure_DoesNotAbortOthers(t *testing.T) {
	fd := &fakeDispatcher{failAddrs: map[string]error{"203.0.113.10": errors.New("dial timeout")}}
	h := newTestHandler(t, fd)
	monitorWithCallback(t, h, "10.0.0.5:601", "peer.example", "203.0.113.10", [16]byte{})
	monitorWithCallback(t, h, "10.0.0.5:602", "peer.example", "203.0.113.11", [16]byte{})

	notify(t, h, "10.0.0.5:55000", "peer.example", 5)

	calls := fd.recorded()
	if len(calls) != 2 {
		t.Fatalf("both monitors must be attempted despite one failure; got %d", len(calls))
	}
	// Gate state must still advance even though one callback failed.
	if got, ok := recordedState(h, "peer.example"); !ok || got != 5 {
		t.Fatalf("peer state must advance to 5; got %d (recorded=%v)", got, ok)
	}
}

// With no dispatcher configured the relay is a no-op, but the gates still run
// and state is still recorded.
func TestNotify_NilDispatcher_NoOpButGatesRun(t *testing.T) {
	h := newTestHandler(t, &fakeDispatcher{})
	h.dispatcher = nil // explicitly disable the relay (same-package access)
	monitorWithCallback(t, h, "10.0.0.5:601", "peer.example", "203.0.113.10", [16]byte{})

	notify(t, h, "10.0.0.5:55000", "peer.example", 5)

	if got, ok := recordedState(h, "peer.example"); !ok || got != 5 {
		t.Fatalf("gates must still run without a dispatcher; state=%d ok=%v", got, ok)
	}
}
