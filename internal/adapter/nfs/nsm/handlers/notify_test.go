package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/nsm/xdr"
	xdrcore "github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/pkg/metadata/lock"
)

// newTestHandler builds a Handler backed by a real ConnectionTracker.
func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	tracker := lock.NewConnectionTracker(lock.DefaultConnectionTrackerConfig())
	t.Cleanup(tracker.Close)
	return NewHandler(HandlerConfig{
		Tracker:    tracker,
		ServerName: "test-server",
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
	h := newTestHandler(t)
	// No SM_MON for "ghost.example".
	notify(t, h, "10.0.0.9:55000", "ghost.example", 5)

	if _, ok := recordedState(h, "ghost.example"); ok {
		t.Fatal("unmonitored NOTIFY must not be acted on (no state recorded)")
	}
}

func TestNotify_SourceAddrMismatch_Rejected(t *testing.T) {
	h := newTestHandler(t)
	// We monitor peer.example as seen from 10.0.0.5.
	monitor(t, h, "10.0.0.5:601", "peer.example", "peer.example")

	// NOTIFY for peer.example arrives from a DIFFERENT source IP.
	notify(t, h, "10.0.0.66:55000", "peer.example", 5)

	if _, ok := recordedState(h, "peer.example"); ok {
		t.Fatal("source-addr mismatch NOTIFY must be rejected (no state recorded)")
	}
}

func TestNotify_MonitoredHostRightAddr_Accepted(t *testing.T) {
	h := newTestHandler(t)
	monitor(t, h, "10.0.0.5:601", "peer.example", "peer.example")

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
	h := newTestHandler(t)
	monitor(t, h, "192.168.1.10:700", "host-a", "host-a")

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
	h := newTestHandler(t)
	monitor(t, h, "10.0.0.5:601", "peer.example", "peer.example")

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
	h := newTestHandler(t)

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

// Replay that passes H16 but fails H17 must remain a no-op (defence in depth):
// the monotonicity gate sits AFTER the address gate, so a monitored host
// replaying an old state still does nothing.
func TestNotify_MonitoredReplay_NoOp(t *testing.T) {
	h := newTestHandler(t)
	monitor(t, h, "10.0.0.5:601", "peer.example", "peer.example")

	notify(t, h, "10.0.0.5:40001", "peer.example", 9)
	notify(t, h, "10.0.0.5:40002", "peer.example", 9) // duplicate

	if got, _ := recordedState(h, "peer.example"); got != 9 {
		t.Fatalf("monitored replay must be a no-op; got %d", got)
	}
}
