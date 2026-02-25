package xdr

import (
	"bytes"
	"testing"

	"github.com/marmos91/dittofs/internal/protocol/nlm/types"
)

func TestEncodeDecodeShareArgs_Roundtrip(t *testing.T) {
	args := &types.NLM4ShareArgs{
		Cookie:     []byte{0x01, 0x02, 0x03},
		CallerName: "windows-client",
		FH:         []byte{0xAA, 0xBB, 0xCC, 0xDD},
		OH:         []byte{0xEE, 0xFF},
		Mode:       types.FSH4ModeReadWrite,
		Access:     types.FSH4DenyNone,
		Reclaim:    false,
	}

	buf := new(bytes.Buffer)
	if err := EncodeNLM4ShareArgs(buf, args); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeNLM4ShareArgs(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(decoded.Cookie, args.Cookie) {
		t.Errorf("cookie: got %v, want %v", decoded.Cookie, args.Cookie)
	}
	if decoded.CallerName != args.CallerName {
		t.Errorf("caller_name: got %q, want %q", decoded.CallerName, args.CallerName)
	}
	if !bytes.Equal(decoded.FH, args.FH) {
		t.Errorf("fh: got %v, want %v", decoded.FH, args.FH)
	}
	if !bytes.Equal(decoded.OH, args.OH) {
		t.Errorf("oh: got %v, want %v", decoded.OH, args.OH)
	}
	if decoded.Mode != args.Mode {
		t.Errorf("mode: got %d, want %d", decoded.Mode, args.Mode)
	}
	if decoded.Access != args.Access {
		t.Errorf("access: got %d, want %d", decoded.Access, args.Access)
	}
	if decoded.Reclaim != args.Reclaim {
		t.Errorf("reclaim: got %v, want %v", decoded.Reclaim, args.Reclaim)
	}
}

func TestEncodeDecodeShareArgs_Reclaim(t *testing.T) {
	args := &types.NLM4ShareArgs{
		Cookie:     []byte{0xFF},
		CallerName: "host",
		FH:         []byte{0x01},
		OH:         []byte{0x02},
		Mode:       types.FSH4ModeRead,
		Access:     types.FSH4DenyRead,
		Reclaim:    true,
	}

	buf := new(bytes.Buffer)
	if err := EncodeNLM4ShareArgs(buf, args); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeNLM4ShareArgs(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !decoded.Reclaim {
		t.Error("expected reclaim=true")
	}
	if decoded.Access != types.FSH4DenyRead {
		t.Errorf("access: got %d, want %d", decoded.Access, types.FSH4DenyRead)
	}
}

func TestEncodeDecodeShareArgs_EmptyCookie(t *testing.T) {
	args := &types.NLM4ShareArgs{
		Cookie:     nil,
		CallerName: "c",
		FH:         []byte{0x01},
		OH:         []byte{0x02},
		Mode:       0,
		Access:     0,
		Reclaim:    false,
	}

	buf := new(bytes.Buffer)
	if err := EncodeNLM4ShareArgs(buf, args); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeNLM4ShareArgs(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(decoded.Cookie) != 0 {
		t.Errorf("expected empty cookie, got %v", decoded.Cookie)
	}
}

func TestEncodeShareArgs_Nil(t *testing.T) {
	buf := new(bytes.Buffer)
	err := EncodeNLM4ShareArgs(buf, nil)
	if err == nil {
		t.Error("expected error for nil args")
	}
}

func TestDecodeShareArgs_Empty(t *testing.T) {
	_, err := DecodeNLM4ShareArgs(bytes.NewReader([]byte{}))
	if err == nil {
		t.Error("expected error for empty data")
	}
}

func TestDecodeShareArgs_Truncated(t *testing.T) {
	// Cookie length = 4 but only 2 bytes of data
	_, err := DecodeNLM4ShareArgs(bytes.NewReader([]byte{0x00, 0x00, 0x00, 0x04, 0x01, 0x02}))
	if err == nil {
		t.Error("expected error for truncated data")
	}
}

func TestEncodeDecodeShareRes_Roundtrip(t *testing.T) {
	res := &types.NLM4ShareRes{
		Cookie:   []byte{0xDE, 0xAD},
		Status:   types.NLM4Granted,
		Sequence: 0,
	}

	buf := new(bytes.Buffer)
	if err := EncodeNLM4ShareRes(buf, res); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeNLM4ShareRes(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(decoded.Cookie, res.Cookie) {
		t.Errorf("cookie: got %v, want %v", decoded.Cookie, res.Cookie)
	}
	if decoded.Status != res.Status {
		t.Errorf("status: got %d, want %d", decoded.Status, res.Status)
	}
	if decoded.Sequence != res.Sequence {
		t.Errorf("sequence: got %d, want %d", decoded.Sequence, res.Sequence)
	}
}

func TestEncodeDecodeShareRes_FailedStatus(t *testing.T) {
	res := &types.NLM4ShareRes{
		Cookie:   []byte{0x01},
		Status:   types.NLM4Failed,
		Sequence: 42,
	}

	buf := new(bytes.Buffer)
	if err := EncodeNLM4ShareRes(buf, res); err != nil {
		t.Fatalf("encode: %v", err)
	}

	decoded, err := DecodeNLM4ShareRes(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if decoded.Status != types.NLM4Failed {
		t.Errorf("status: got %d, want %d", decoded.Status, types.NLM4Failed)
	}
	if decoded.Sequence != 42 {
		t.Errorf("sequence: got %d, want 42", decoded.Sequence)
	}
}

func TestEncodeShareRes_Nil(t *testing.T) {
	buf := new(bytes.Buffer)
	err := EncodeNLM4ShareRes(buf, nil)
	if err == nil {
		t.Error("expected error for nil response")
	}
}

func TestDecodeShareRes_Empty(t *testing.T) {
	_, err := DecodeNLM4ShareRes(bytes.NewReader([]byte{}))
	if err == nil {
		t.Error("expected error for empty data")
	}
}

func TestDecodeShareRes_Truncated(t *testing.T) {
	// Only cookie, no status or sequence
	buf := new(bytes.Buffer)
	_ = buf.WriteByte(0x00) // cookie len = 0
	_ = buf.WriteByte(0x00)
	_ = buf.WriteByte(0x00)
	_ = buf.WriteByte(0x00)

	_, err := DecodeNLM4ShareRes(bytes.NewReader(buf.Bytes()))
	if err == nil {
		t.Error("expected error for truncated data (missing status and sequence)")
	}
}
