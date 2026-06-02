package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/smb/types"
)

// Handler/wire-level coverage for lone-surrogate filenames (smb2.charset.Testing
// test_surrogate, #740). The codec round-trip and the metadata-service
// CREATE/lookup/ReadDirectory round-trip already live in converters_test.go
// (TestLoneSurrogateNames_CreateQueryRoundTrip). These tests add the one hop
// that test does not exercise: the live h.Create / h.QueryDirectory handlers,
// including the UTF-16LE wire decode (DecodeCreateRequest) and the
// QUERY_DIRECTORY wire encoder. A strict UTF-16/UTF-8 hop reintroduced anywhere
// between the wire and the store would surface here as a non-success CREATE
// status or a mangled/aliased name in the directory buffer.
//
// Note: the smbtorture "Testing partial surrogate" subcase cannot reach a real
// server — smbtorture converts the UTF-16 name to CH_UNIX *client-side* before
// sending, and its iconv rejects lone surrogates with EILSEQ. Samba's own
// selftest/knownfail marks "smb2.charset.*.Testing partial surrogate" as
// "currently broken" for exactly this reason. These tests therefore assert the
// behaviour DittoFS controls: a lone-surrogate name that does arrive on the wire
// is created and listed back byte-for-byte.

// surrogateWTF8 returns the 3-byte WTF-8 encoding of a single surrogate code
// unit (D800-DFFF), mirroring how decodeUTF16LE preserves an unpaired surrogate
// that arrived on the wire.
func surrogateWTF8(cp uint16) string {
	return string([]byte{
		byte(0xE0 | (cp >> 12)),
		byte(0x80 | ((cp >> 6) & 0x3F)),
		byte(0x80 | (cp & 0x3F)),
	})
}

// TestCreate_LoneSurrogate_WireRoundTrip drives lone-surrogate names through the
// full request path: UTF-16LE wire bytes -> DecodeCreateRequest -> h.Create ->
// metadata store. The wire decode must reproduce the WTF-8 name exactly, and the
// handler's create + name-validation chain must accept it (STATUS_SUCCESS). The
// full pair and lone low surrogate are covered so a regression that only mangles
// one form is caught.
func TestCreate_LoneSurrogate_WireRoundTrip(t *testing.T) {
	h, smbCtx, _ := setupStreamsDisabledShare(t, false)

	cases := []struct {
		name  string
		fname string
	}{
		{"lone_high", surrogateWTF8(0xD800)},
		{"lone_low", surrogateWTF8(0xDC00)},
		{"full_pair", "\U00010000"}, // 0xD800 0xDC00
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := buildCreateRequestBody(tc.fname, types.FileCreate, 0)
			req, err := DecodeCreateRequest(body)
			if err != nil {
				t.Fatalf("DecodeCreateRequest: %v", err)
			}
			if req.FileName != tc.fname {
				t.Fatalf("wire decode altered name: got % x want % x", req.FileName, tc.fname)
			}
			resp, err := h.Create(smbCtx, req)
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if resp.Status != types.StatusSuccess {
				t.Fatalf("Create(% x): status = 0x%08x, want STATUS_SUCCESS", tc.fname, uint32(resp.Status))
			}
		})
	}
}

// TestQueryDirectory_LoneSurrogate_WireEncode confirms the QUERY_DIRECTORY wire
// encoder emits the lone surrogates as their exact original UTF-16 code units. A
// directory holding the lone high surrogate, the lone low surrogate, and the
// well-formed pair must list all three back as distinct names whose FileName
// bytes decode to the original input. The stdlib utf16.Decode would map both
// lone surrogates to U+FFFD, aliasing them and dropping an entry.
func TestQueryDirectory_LoneSurrogate_WireEncode(t *testing.T) {
	high := surrogateWTF8(0xD800)
	low := surrogateWTF8(0xDC00)
	pair := "\U00010000"

	h, open, _, smbCtx := setupQueryDirTest(t, []string{high, low, pair})

	resp := callQuery(t, h, smbCtx, open.FileID, "*", uint8(types.SMB2RestartScans))
	if resp.Status != types.StatusSuccess {
		t.Fatalf("QueryDirectory status = 0x%08x", uint32(resp.Status))
	}

	names := queryDirNames(resp.Data)
	for _, want := range []string{high, low, pair} {
		if !names[want] {
			t.Errorf("name % x missing from directory listing; got %d non-dot entries", want, len(names))
		}
	}
}

// queryDirNames walks a FILE_ID_BOTH_DIR_INFORMATION buffer and decodes each
// entry's FileName back to a Go string via the surrogate-safe codec, skipping
// "." and "..".
func queryDirNames(buf []byte) map[string]bool {
	out := map[string]bool{}
	off := 0
	for off+104 <= len(buf) {
		next := binary.LittleEndian.Uint32(buf[off : off+4])
		nameLen := int(binary.LittleEndian.Uint32(buf[off+60 : off+64]))
		start := off + 104
		if start+nameLen <= len(buf) {
			if name := decodeUTF16LE(buf[start : start+nameLen]); name != "." && name != ".." {
				out[name] = true
			}
		}
		if next == 0 {
			break
		}
		off += int(next)
	}
	return out
}
