package handlers

import "github.com/marmos91/dittofs/internal/adapter/smb/smbenc"

// Apple SMB2 "AAPL" CREATE context [Apple SMB2 extensions, as implemented by
// Samba's vfs_fruit]. macOS clients send an AAPL context on the first CREATE
// after TREE_CONNECT to probe server capabilities. When the server responds
// advertising a UNIX-based volume, macOS enables POSIX semantics — including
// creating symbolic links via FSCTL_SET_REPARSE_POINT (handled in
// set_reparse_point.go) rather than refusing them client-side. See #1179.
const (
	// aaplCreateContextTag is the create-context name macOS uses.
	aaplCreateContextTag = "AAPL"

	// aaplCommandServerQuery is the AAPL sub-command for the capability probe.
	aaplCommandServerQuery uint32 = 1

	// aaplReplyBitmapServerCaps marks the ServerCaps field as present in the
	// reply bitmap.
	aaplReplyBitmapServerCaps uint64 = 0x1

	// aaplServerCapUnixBased advertises a UNIX-based volume. This is the bit
	// macOS keys off to use native POSIX symlinks. We deliberately do NOT set
	// kAAPL_SUPPORTS_READ_DIR_ATTR — that obligates the server to return packed
	// per-entry attributes in FIND responses, which we don't implement.
	aaplServerCapUnixBased uint64 = 0x4
)

// buildAAPLServerQueryResponse parses an inbound AAPL server-query create
// context and returns the response context Data, or nil if the request is not a
// server query we answer.
//
// Request layout (little-endian) [vfs_fruit aapl_server_query]:
//
//	CommandCode(4) Reserved(4) RequestBitmap(8) ClientCapabilities(8)
//
// Response layout:
//
//	CommandCode(4) Reserved(4) ReplyBitmap(8) ServerCapabilities(8)
//
// We reply with only the ServerCaps field present (ReplyBitmap = ServerCaps),
// advertising a UNIX-based volume, and only when the client's RequestBitmap
// actually asked for server caps (bit 0). The full 24-byte request is required;
// truncated/malformed contexts are ignored (nil reply, no AAPL response added).
func buildAAPLServerQueryResponse(data []byte) []byte {
	if len(data) < 24 {
		return nil
	}
	r := smbenc.NewReader(data)
	command := r.ReadUint32()
	r.Skip(4) // Reserved
	requestBitmap := r.ReadUint64()
	if r.Err() != nil || command != aaplCommandServerQuery {
		return nil
	}
	// The only field we can answer is ServerCaps; if the client didn't request
	// it there is nothing useful to return.
	if requestBitmap&aaplReplyBitmapServerCaps == 0 {
		return nil
	}

	w := smbenc.NewWriter(24)
	w.WriteUint32(aaplCommandServerQuery)    // CommandCode
	w.WriteUint32(0)                         // Reserved
	w.WriteUint64(aaplReplyBitmapServerCaps) // ReplyBitmap
	w.WriteUint64(aaplServerCapUnixBased)    // ServerCapabilities
	return w.Bytes()
}
