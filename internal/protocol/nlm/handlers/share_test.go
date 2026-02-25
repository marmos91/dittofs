package handlers

import (
	"bytes"
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nlm/types"
	nlm_xdr "github.com/marmos91/dittofs/internal/protocol/nlm/xdr"
)

func newTestHandler() *Handler {
	return &Handler{}
}

func newTestContext() *NLMHandlerContext {
	return &NLMHandlerContext{
		Context:    context.Background(),
		ClientAddr: "192.168.1.100:12345",
		AuthFlavor: 1, // AUTH_UNIX
	}
}

// encodeShareArgs builds XDR bytes for an NLM4ShareArgs.
func encodeShareArgs(t *testing.T, args *types.NLM4ShareArgs) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	if err := nlm_xdr.EncodeNLM4ShareArgs(buf, args); err != nil {
		t.Fatalf("failed to encode share args: %v", err)
	}
	return buf.Bytes()
}

// ============================================================================
// Handler Tests
// ============================================================================

func TestShare_AlwaysGranted(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	req := &ShareRequest{
		Cookie:     []byte{0x01, 0x02},
		CallerName: "windows-client",
		FH:         []byte{0xAA, 0xBB, 0xCC},
		OH:         []byte{0xDD, 0xEE},
		Mode:       types.FSH4ModeReadWrite,
		Access:     types.FSH4DenyNone,
		Reclaim:    false,
	}

	resp, err := h.Share(ctx, req)
	if err != nil {
		t.Fatalf("Share returned error: %v", err)
	}
	if resp.Status != types.NLM4Granted {
		t.Errorf("expected NLM4Granted (%d), got %d", types.NLM4Granted, resp.Status)
	}
	if !bytes.Equal(resp.Cookie, req.Cookie) {
		t.Errorf("cookie not echoed: got %v, want %v", resp.Cookie, req.Cookie)
	}
	if resp.Sequence != 0 {
		t.Errorf("expected sequence 0, got %d", resp.Sequence)
	}
}

func TestShare_ReadMode(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	req := &ShareRequest{
		Cookie:     []byte{0xFF},
		CallerName: "client1",
		FH:         []byte{0x01},
		OH:         []byte{0x02},
		Mode:       types.FSH4ModeRead,
		Access:     types.FSH4DenyNone,
	}

	resp, err := h.Share(ctx, req)
	if err != nil {
		t.Fatalf("Share returned error: %v", err)
	}
	if resp.Status != types.NLM4Granted {
		t.Errorf("expected NLM4Granted, got %d", resp.Status)
	}
}

func TestShare_WriteMode(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	req := &ShareRequest{
		Cookie:     []byte{0xAB},
		CallerName: "client2",
		FH:         []byte{0x03},
		OH:         []byte{0x04},
		Mode:       types.FSH4ModeWrite,
		Access:     types.FSH4DenyRead,
	}

	resp, err := h.Share(ctx, req)
	if err != nil {
		t.Fatalf("Share returned error: %v", err)
	}
	if resp.Status != types.NLM4Granted {
		t.Errorf("expected NLM4Granted, got %d", resp.Status)
	}
}

func TestShare_Reclaim(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	req := &ShareRequest{
		Cookie:     []byte{0x10},
		CallerName: "recovering-client",
		FH:         []byte{0x20},
		OH:         []byte{0x30},
		Mode:       types.FSH4ModeReadWrite,
		Access:     types.FSH4DenyNone,
		Reclaim:    true,
	}

	resp, err := h.Share(ctx, req)
	if err != nil {
		t.Fatalf("Share returned error: %v", err)
	}
	if resp.Status != types.NLM4Granted {
		t.Errorf("expected NLM4Granted, got %d", resp.Status)
	}
}

func TestShare_EmptyCookie(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	req := &ShareRequest{
		Cookie:     nil,
		CallerName: "client",
		FH:         []byte{0x01},
		OH:         []byte{0x02},
		Mode:       types.FSH4ModeRead,
		Access:     types.FSH4DenyNone,
	}

	resp, err := h.Share(ctx, req)
	if err != nil {
		t.Fatalf("Share returned error: %v", err)
	}
	if resp.Status != types.NLM4Granted {
		t.Errorf("expected NLM4Granted, got %d", resp.Status)
	}
	if resp.Cookie != nil {
		t.Errorf("expected nil cookie, got %v", resp.Cookie)
	}
}

func TestUnshare_AlwaysGranted(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	req := &ShareRequest{
		Cookie:     []byte{0x01, 0x02},
		CallerName: "windows-client",
		FH:         []byte{0xAA, 0xBB, 0xCC},
		OH:         []byte{0xDD, 0xEE},
		Mode:       types.FSH4ModeReadWrite,
		Access:     types.FSH4DenyNone,
	}

	resp, err := h.Unshare(ctx, req)
	if err != nil {
		t.Fatalf("Unshare returned error: %v", err)
	}
	if resp.Status != types.NLM4Granted {
		t.Errorf("expected NLM4Granted (%d), got %d", types.NLM4Granted, resp.Status)
	}
	if !bytes.Equal(resp.Cookie, req.Cookie) {
		t.Errorf("cookie not echoed: got %v, want %v", resp.Cookie, req.Cookie)
	}
	if resp.Sequence != 0 {
		t.Errorf("expected sequence 0, got %d", resp.Sequence)
	}
}

func TestUnshare_WithoutPriorShare(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	req := &ShareRequest{
		Cookie:     []byte{0x99},
		CallerName: "unknown-client",
		FH:         []byte{0x50},
		OH:         []byte{0x60},
		Mode:       types.FSH4ModeRead,
		Access:     types.FSH4DenyNone,
	}

	resp, err := h.Unshare(ctx, req)
	if err != nil {
		t.Fatalf("Unshare returned error: %v", err)
	}
	if resp.Status != types.NLM4Granted {
		t.Errorf("expected NLM4Granted, got %d", resp.Status)
	}
}

// ============================================================================
// Decode / Encode Tests
// ============================================================================

func TestDecodeShareRequest_Valid(t *testing.T) {
	args := &types.NLM4ShareArgs{
		Cookie:     []byte{0x01, 0x02, 0x03},
		CallerName: "test-host",
		FH:         []byte{0xAA, 0xBB, 0xCC, 0xDD},
		OH:         []byte{0xEE, 0xFF},
		Mode:       types.FSH4ModeReadWrite,
		Access:     types.FSH4DenyNone,
		Reclaim:    false,
	}

	data := encodeShareArgs(t, args)

	req, err := DecodeShareRequest(data)
	if err != nil {
		t.Fatalf("DecodeShareRequest failed: %v", err)
	}

	if !bytes.Equal(req.Cookie, args.Cookie) {
		t.Errorf("cookie: got %v, want %v", req.Cookie, args.Cookie)
	}
	if req.CallerName != args.CallerName {
		t.Errorf("caller_name: got %q, want %q", req.CallerName, args.CallerName)
	}
	if !bytes.Equal(req.FH, args.FH) {
		t.Errorf("fh: got %v, want %v", req.FH, args.FH)
	}
	if !bytes.Equal(req.OH, args.OH) {
		t.Errorf("oh: got %v, want %v", req.OH, args.OH)
	}
	if req.Mode != args.Mode {
		t.Errorf("mode: got %d, want %d", req.Mode, args.Mode)
	}
	if req.Access != args.Access {
		t.Errorf("access: got %d, want %d", req.Access, args.Access)
	}
	if req.Reclaim != args.Reclaim {
		t.Errorf("reclaim: got %v, want %v", req.Reclaim, args.Reclaim)
	}
}

func TestDecodeShareRequest_Reclaim(t *testing.T) {
	args := &types.NLM4ShareArgs{
		Cookie:     []byte{0x10},
		CallerName: "reclaim-host",
		FH:         []byte{0x01},
		OH:         []byte{0x02},
		Mode:       types.FSH4ModeRead,
		Access:     types.FSH4DenyRead,
		Reclaim:    true,
	}

	data := encodeShareArgs(t, args)

	req, err := DecodeShareRequest(data)
	if err != nil {
		t.Fatalf("DecodeShareRequest failed: %v", err)
	}

	if !req.Reclaim {
		t.Error("expected reclaim=true")
	}
	if req.Mode != types.FSH4ModeRead {
		t.Errorf("mode: got %d, want %d", req.Mode, types.FSH4ModeRead)
	}
	if req.Access != types.FSH4DenyRead {
		t.Errorf("access: got %d, want %d", req.Access, types.FSH4DenyRead)
	}
}

func TestDecodeShareRequest_EmptyData(t *testing.T) {
	_, err := DecodeShareRequest([]byte{})
	if err == nil {
		t.Error("expected error for empty data")
	}
}

func TestDecodeShareRequest_Truncated(t *testing.T) {
	// Just a cookie length with no payload
	_, err := DecodeShareRequest([]byte{0x00, 0x00, 0x00, 0x04, 0x01, 0x02})
	if err == nil {
		t.Error("expected error for truncated data")
	}
}

func TestEncodeShareResponse_Valid(t *testing.T) {
	resp := &ShareResponse{
		Cookie:   []byte{0x01, 0x02, 0x03},
		Status:   types.NLM4Granted,
		Sequence: 0,
	}

	data, err := EncodeShareResponse(resp)
	if err != nil {
		t.Fatalf("EncodeShareResponse failed: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("encoded data is empty")
	}

	// Decode the response to verify roundtrip
	r := bytes.NewReader(data)
	decoded, err := nlm_xdr.DecodeNLM4ShareRes(r)
	if err != nil {
		t.Fatalf("failed to decode encoded response: %v", err)
	}
	if !bytes.Equal(decoded.Cookie, resp.Cookie) {
		t.Errorf("cookie roundtrip: got %v, want %v", decoded.Cookie, resp.Cookie)
	}
	if decoded.Status != resp.Status {
		t.Errorf("status roundtrip: got %d, want %d", decoded.Status, resp.Status)
	}
	if decoded.Sequence != resp.Sequence {
		t.Errorf("sequence roundtrip: got %d, want %d", decoded.Sequence, resp.Sequence)
	}
}

func TestEncodeShareResponse_EmptyCookie(t *testing.T) {
	resp := &ShareResponse{
		Cookie:   nil,
		Status:   types.NLM4Granted,
		Sequence: 0,
	}

	data, err := EncodeShareResponse(resp)
	if err != nil {
		t.Fatalf("EncodeShareResponse failed: %v", err)
	}

	r := bytes.NewReader(data)
	decoded, err := nlm_xdr.DecodeNLM4ShareRes(r)
	if err != nil {
		t.Fatalf("failed to decode: %v", err)
	}
	if decoded.Status != types.NLM4Granted {
		t.Errorf("status: got %d, want %d", decoded.Status, types.NLM4Granted)
	}
}

// ============================================================================
// Roundtrip Tests (Decode → Handler → Encode)
// ============================================================================

func TestShareRoundtrip(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	// Encode a share request
	args := &types.NLM4ShareArgs{
		Cookie:     []byte{0xDE, 0xAD},
		CallerName: "windows-pc",
		FH:         []byte{0x01, 0x02, 0x03, 0x04},
		OH:         []byte{0x05, 0x06},
		Mode:       types.FSH4ModeReadWrite,
		Access:     types.FSH4DenyNone,
		Reclaim:    false,
	}
	data := encodeShareArgs(t, args)

	// Decode
	req, err := DecodeShareRequest(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Handle
	resp, err := h.Share(ctx, req)
	if err != nil {
		t.Fatalf("share: %v", err)
	}

	// Encode response
	respData, err := EncodeShareResponse(resp)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}

	// Verify response can be decoded
	r := bytes.NewReader(respData)
	decoded, err := nlm_xdr.DecodeNLM4ShareRes(r)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.Status != types.NLM4Granted {
		t.Errorf("status: got %d, want NLM4Granted", decoded.Status)
	}
	if !bytes.Equal(decoded.Cookie, args.Cookie) {
		t.Errorf("cookie: got %v, want %v", decoded.Cookie, args.Cookie)
	}
}

func TestUnshareRoundtrip(t *testing.T) {
	h := newTestHandler()
	ctx := newTestContext()

	args := &types.NLM4ShareArgs{
		Cookie:     []byte{0xBE, 0xEF},
		CallerName: "windows-pc",
		FH:         []byte{0x10, 0x20},
		OH:         []byte{0x30},
		Mode:       types.FSH4ModeRead,
		Access:     types.FSH4DenyNone,
		Reclaim:    false,
	}
	data := encodeShareArgs(t, args)

	req, err := DecodeShareRequest(data)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	resp, err := h.Unshare(ctx, req)
	if err != nil {
		t.Fatalf("unshare: %v", err)
	}

	respData, err := EncodeShareResponse(resp)
	if err != nil {
		t.Fatalf("encode response: %v", err)
	}

	r := bytes.NewReader(respData)
	decoded, err := nlm_xdr.DecodeNLM4ShareRes(r)
	if err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if decoded.Status != types.NLM4Granted {
		t.Errorf("status: got %d, want NLM4Granted", decoded.Status)
	}
	if !bytes.Equal(decoded.Cookie, args.Cookie) {
		t.Errorf("cookie: got %v, want %v", decoded.Cookie, args.Cookie)
	}
}
