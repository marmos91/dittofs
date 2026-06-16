package smb

// Tests for the SMB RED (rate/errors/duration) metrics wired into the dispatch
// path (PR-2b of #1188). They drive ProcessSingleRequest with a real
// *metrics.Metrics sink on the ConnInfo and assert the emitted series carry
// bounded labels (protocol=smb, lower-cased op, ok|error status).

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/marmos91/dittofs/internal/adapter/smb/handlers"
	"github.com/marmos91/dittofs/internal/adapter/smb/header"
	"github.com/marmos91/dittofs/internal/adapter/smb/session"
	"github.com/marmos91/dittofs/internal/adapter/smb/types"
	"github.com/marmos91/dittofs/pkg/metrics"
)

// newMetricsConnInfo builds a ConnInfo whose send path routes through
// WriteNetBIOSFrame (SessionID 0 keeps the plain-wire branch) and whose Metrics
// sink is a real, owned registry the test can scrape.
func newMetricsConnInfo(t *testing.T, conn net.Conn) (*ConnInfo, *metrics.Metrics) {
	t.Helper()
	m := metrics.New("test", "test")
	mgr := session.NewDefaultManager()
	return &ConnInfo{
		Conn:           conn,
		Handler:        handlers.NewHandlerWithSessionManager(mgr),
		SessionManager: mgr,
		WriteMu:        &LockedWriter{},
		WriteTimeout:   2 * time.Second,
		SequenceWindow: NewSequenceWindowForConnection(mgr),
		Metrics:        m,
	}, m
}

// echoBody is the minimal valid SMB2 ECHO request body (StructureSize=4).
func echoBody() []byte { return []byte{0x04, 0x00, 0x00, 0x00} }

func echoHeader(messageID uint64) *header.SMB2Header {
	return &header.SMB2Header{
		StructureSize: header.HeaderSize,
		Command:       types.SMB2Echo,
		Credits:       1,
		CreditCharge:  1,
		MessageID:     messageID,
		SessionID:     0,
		TreeID:        0,
	}
}

func TestProcessSingleRequest_RecordsRedOnSuccess(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	t.Cleanup(cleanup)

	ci, m := newMetricsConnInfo(t, serverConn)

	if err := ProcessSingleRequest(context.Background(), echoHeader(1), echoBody(), nil, ci, false, nil); err != nil {
		t.Fatalf("ProcessSingleRequest(ECHO) returned error: %v", err)
	}

	expected := `
# HELP dittofs_adapter_requests_total Protocol requests handled, by protocol, operation, and status (ok|error).
# TYPE dittofs_adapter_requests_total counter
dittofs_adapter_requests_total{op="echo",protocol="smb",status="ok"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "dittofs_adapter_requests_total"); err != nil {
		t.Fatalf("requests_total mismatch: %v", err)
	}
	if got := testutil.CollectAndCount(m.Registry(), "dittofs_adapter_request_duration_seconds"); got == 0 {
		t.Fatal("expected request_duration_seconds series for smb echo")
	}
}

func TestProcessSingleRequest_RecordsErrorOnUnknownCommand(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	t.Cleanup(cleanup)

	ci, m := newMetricsConnInfo(t, serverConn)
	// An unmapped command code makes prepareDispatch return STATUS_INVALID_PARAMETER
	// before any handler runs; the dispatch-error path records status="error".
	reqHeader := echoHeader(1)
	reqHeader.Command = types.Command(0x7000) // not in DispatchTable

	if err := ProcessSingleRequest(context.Background(), reqHeader, nil, nil, ci, false, nil); err != nil {
		t.Fatalf("ProcessSingleRequest(unknown) returned error: %v", err)
	}

	// Unmapped codes collapse to the single bounded op="unknown" label (not the
	// numeric code) so an unrecognised-command flood cannot blow up cardinality.
	expected := `
# HELP dittofs_adapter_requests_total Protocol requests handled, by protocol, operation, and status (ok|error).
# TYPE dittofs_adapter_requests_total counter
dittofs_adapter_requests_total{op="unknown",protocol="smb",status="error"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "dittofs_adapter_requests_total"); err != nil {
		t.Fatalf("requests_total mismatch: %v", err)
	}
}

func TestProcessSingleRequest_NilMetricsNoPanic(t *testing.T) {
	serverConn, cleanup := newTestConnPair(t)
	t.Cleanup(cleanup)

	mgr := session.NewDefaultManager()
	ci := &ConnInfo{
		Conn:           serverConn,
		Handler:        handlers.NewHandlerWithSessionManager(mgr),
		SessionManager: mgr,
		WriteMu:        &LockedWriter{},
		WriteTimeout:   2 * time.Second,
		SequenceWindow: NewSequenceWindowForConnection(mgr),
		Metrics:        nil,
	}
	if err := ProcessSingleRequest(context.Background(), echoHeader(1), echoBody(), nil, ci, false, nil); err != nil {
		t.Fatalf("ProcessSingleRequest with nil Metrics returned error: %v", err)
	}
}
