package rpc

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/pkg/auth/sid"
)

// buildTestAlterContext creates an alter_context PDU for the LSA interface. Its
// body layout is identical to a bind, so it reuses buildTestBindRequest and only
// rewrites the PDU type byte.
func buildTestAlterContext(callID uint32) []byte {
	buf := buildTestBindRequest(callID)
	buf[2] = PDUAlterContext
	return buf
}

// newTestPipe creates a PipeState backed by a deterministic LSA handler.
func newTestPipe() *PipeState {
	return NewPipeState("lsarpc", newTestLSAHandler())
}

// drainResponse runs ProcessRead and fails the test if no bytes are buffered —
// the exact starvation symptom of #1607 (a WRITE that produces no response for
// the client's subsequent READ to drain).
func drainResponse(t *testing.T, p *PipeState) []byte {
	t.Helper()
	data := p.ProcessRead(65536)
	if len(data) == 0 {
		t.Fatal("ProcessRead returned no data: the WRITE enqueued no response " +
			"(the parked async READ would starve forever)")
	}
	return data
}

// TestPipeState_BindThenLookupSids2 drives the full WRITE->dispatch->buffer path
// end to end: a bind followed by a LookupSids2 request must each enqueue a
// response for the client's READ.
func TestPipeState_BindThenLookupSids2(t *testing.T) {
	p := newTestPipe()

	if err := p.ProcessWrite(buildTestBindRequest(1)); err != nil {
		t.Fatalf("ProcessWrite(bind): %v", err)
	}
	bindAck := drainResponse(t, p)
	if hdr, err := ParseHeader(bindAck); err != nil {
		t.Fatalf("ParseHeader(bindAck): %v", err)
	} else if hdr.PacketType != PDUBindAck {
		t.Fatalf("bind response type = %d, want %d (BindAck)", hdr.PacketType, PDUBindAck)
	}

	stub := buildLookupSids2StubData([]*sid.SID{sid.WellKnownEveryone})
	if err := p.ProcessWrite(buildTestRequest(2, OpLsarLookupSids2, stub)); err != nil {
		t.Fatalf("ProcessWrite(lookupsids2): %v", err)
	}
	resp := drainResponse(t, p)
	hdr, err := ParseHeader(resp)
	if err != nil {
		t.Fatalf("ParseHeader(response): %v", err)
	}
	if hdr.PacketType != PDUResponse {
		t.Fatalf("lookup response type = %d, want %d (Response)", hdr.PacketType, PDUResponse)
	}
	if hdr.CallID != 2 {
		t.Fatalf("lookup response CallID = %d, want 2", hdr.CallID)
	}
}

// TestPipeState_RequestBeforeBind_ReturnsFault verifies a request that arrives
// before a successful bind no longer silently drops (which stranded the READ);
// it now yields a DCE/RPC FAULT so the paired READ always completes.
func TestPipeState_RequestBeforeBind_ReturnsFault(t *testing.T) {
	p := newTestPipe()

	stub := buildLookupSids2StubData([]*sid.SID{sid.WellKnownEveryone})
	if err := p.ProcessWrite(buildTestRequest(7, OpLsarLookupSids2, stub)); err != nil {
		t.Fatalf("ProcessWrite(unbound request): %v", err)
	}

	resp := drainResponse(t, p)
	hdr, err := ParseHeader(resp)
	if err != nil {
		t.Fatalf("ParseHeader(fault): %v", err)
	}
	if hdr.PacketType != PDUFault {
		t.Fatalf("unbound-request response type = %d, want %d (Fault)", hdr.PacketType, PDUFault)
	}
	if hdr.CallID != 7 {
		t.Fatalf("fault CallID = %d, want 7", hdr.CallID)
	}
	if status := binary.LittleEndian.Uint32(resp[24:28]); status != NcaSProtoError {
		t.Fatalf("fault status = 0x%x, want 0x%x (nca_s_proto_error)", status, NcaSProtoError)
	}
}

// TestPipeState_AlterContext_Answered verifies an alter_context PDU (previously
// dropped by the default arm of dispatchRPC, starving the client's READ) is now
// answered with an alter_context_resp.
func TestPipeState_AlterContext_Answered(t *testing.T) {
	p := newTestPipe()

	if err := p.ProcessWrite(buildTestAlterContext(9)); err != nil {
		t.Fatalf("ProcessWrite(alter_context): %v", err)
	}
	resp := drainResponse(t, p)
	hdr, err := ParseHeader(resp)
	if err != nil {
		t.Fatalf("ParseHeader(alter_context_resp): %v", err)
	}
	if hdr.PacketType != PDUAlterContextR {
		t.Fatalf("alter_context response type = %d, want %d (AlterContextResp)", hdr.PacketType, PDUAlterContextR)
	}
	if hdr.CallID != 9 {
		t.Fatalf("alter_context response CallID = %d, want 9", hdr.CallID)
	}
	if !p.Bound {
		t.Fatal("alter_context should mark the pipe bound")
	}
}

// TestPipeState_CoalescedPDUs_BothDispatched verifies that when a client packs a
// bind and a request into a single WRITE, both PDUs are dispatched and both
// responses are enqueued (previously only the first PDU was handled and the
// trailing request was silently discarded, hanging the READ).
func TestPipeState_CoalescedPDUs_BothDispatched(t *testing.T) {
	p := newTestPipe()

	bind := buildTestBindRequest(11)
	stub := buildLookupSids2StubData([]*sid.SID{sid.WellKnownEveryone})
	req := buildTestRequest(12, OpLsarLookupSids2, stub)

	coalesced := append(append([]byte(nil), bind...), req...)
	if err := p.ProcessWrite(coalesced); err != nil {
		t.Fatalf("ProcessWrite(coalesced): %v", err)
	}

	all := drainResponse(t, p)

	// First response: bind_ack.
	hdr1, err := ParseHeader(all)
	if err != nil {
		t.Fatalf("ParseHeader(first): %v", err)
	}
	if hdr1.PacketType != PDUBindAck {
		t.Fatalf("first response type = %d, want %d (BindAck)", hdr1.PacketType, PDUBindAck)
	}
	// Second response: the request response follows immediately.
	off := int(hdr1.FragLength)
	if off <= 0 || off >= len(all) {
		t.Fatalf("no second response after bind_ack (fragLen=%d, total=%d): the coalesced request was dropped", off, len(all))
	}
	hdr2, err := ParseHeader(all[off:])
	if err != nil {
		t.Fatalf("ParseHeader(second): %v", err)
	}
	if hdr2.PacketType != PDUResponse {
		t.Fatalf("second response type = %d, want %d (Response)", hdr2.PacketType, PDUResponse)
	}
	if hdr2.CallID != 12 {
		t.Fatalf("second response CallID = %d, want 12", hdr2.CallID)
	}
}

// TestPipeState_TrailingPadding_Ignored verifies a WRITE that carries a complete
// bind PDU followed by trailing zero padding dispatches only the bind and does
// not fault on the padding.
func TestPipeState_TrailingPadding_Ignored(t *testing.T) {
	p := newTestPipe()

	padded := append(buildTestBindRequest(3), make([]byte, 32)...)
	if err := p.ProcessWrite(padded); err != nil {
		t.Fatalf("ProcessWrite(padded bind): %v", err)
	}
	resp := drainResponse(t, p)
	hdr, err := ParseHeader(resp)
	if err != nil {
		t.Fatalf("ParseHeader: %v", err)
	}
	if hdr.PacketType != PDUBindAck {
		t.Fatalf("response type = %d, want %d (BindAck)", hdr.PacketType, PDUBindAck)
	}
	// Exactly one response — nothing else buffered from the padding.
	if extra := p.ProcessRead(65536); len(extra) != 0 {
		t.Fatalf("unexpected %d extra response bytes buffered from trailing padding", len(extra))
	}
}
