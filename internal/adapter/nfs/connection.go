// connection.go provides shared RPC framing utilities used by both pkg/adapter/nfs
// (the NFSConnection layer) and potentially other components that need to parse
// RPC record-marking protocol frames.
//
// These functions were extracted from pkg/adapter/nfs/connection.go to enable
// reuse across version-specific connection handling code.
package nfs

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
	"github.com/marmos91/dittofs/internal/adapter/nfs/v4/state"
	"github.com/marmos91/dittofs/internal/adapter/pool"
	"github.com/marmos91/dittofs/internal/bytesize"
	"github.com/marmos91/dittofs/internal/logger"
)

// MaxFragmentSize is the maximum allowed RPC fragment size.
// Must be larger than advertised MAXREAD/MAXWRITE (1MB) to accommodate
// RPC headers + NFS compound headers (~200 bytes overhead per request).
const MaxFragmentSize = (1 << 20) + (1 << 18) // 1MB + 256KB headroom

// FragmentHeader represents a parsed RPC record-marking fragment header.
//
// The fragment header is 4 bytes:
//   - Bit 31: Last fragment flag (1 = last, 0 = more fragments)
//   - Bits 0-30: Fragment length in bytes
type FragmentHeader struct {
	IsLast bool
	Length uint32
}

// ReadFragmentHeader reads and parses the 4-byte RPC fragment header from the reader.
//
// Returns the parsed header or an error if reading fails. EOF errors are returned
// directly (not wrapped) to allow callers to detect normal client disconnect.
func ReadFragmentHeader(r io.Reader) (*FragmentHeader, error) {
	var buf [4]byte
	_, err := io.ReadFull(r, buf[:])
	if err != nil {
		return nil, err
	}

	header := binary.BigEndian.Uint32(buf[:])
	return &FragmentHeader{
		IsLast: (header & 0x80000000) != 0,
		Length: header & 0x7FFFFFFF,
	}, nil
}

// ValidateFragmentSize checks that the fragment length does not exceed MaxFragmentSize.
// This prevents memory exhaustion from malicious or corrupt fragment headers.
func ValidateFragmentSize(length uint32, clientAddr string) error {
	if length > MaxFragmentSize {
		logger.Warn("Fragment size exceeds maximum",
			"size", bytesize.ByteSize(length),
			"max", bytesize.ByteSize(MaxFragmentSize),
			"address", clientAddr)
		return fmt.Errorf("fragment too large: %d bytes", length)
	}
	return nil
}

// ReadRPCMessage reads an RPC message of the specified length using the buffer pool.
//
// The caller is responsible for returning the buffer to the pool via pool.Put()
// after processing is complete.
func ReadRPCMessage(r io.Reader, length uint32) ([]byte, error) {
	// Get buffer from pool
	message := pool.GetUint32(length)

	// Read directly into pooled buffer
	_, err := io.ReadFull(r, message)
	if err != nil {
		// Return buffer to pool on error
		pool.Put(message)
		return nil, fmt.Errorf("read message: %w", err)
	}

	return message, nil
}

// ReadRPCRecord reads a complete RPC record from the reader, reassembling
// multiple record-marking fragments (RFC 5531 §11) until the last-fragment
// flag is set.
//
// firstHeader is the already-read header of the first fragment (whose size the
// caller has validated). Subsequent fragment headers are read and validated
// here, with the running total bounded by MaxFragmentSize so a stream of
// not-last fragments cannot exhaust memory.
//
// The common case — a single last fragment — returns a single pooled buffer
// with no extra copy. The returned buffer is from the pool; the caller must
// return it via pool.Put() after processing.
func ReadRPCRecord(r io.Reader, firstHeader *FragmentHeader, clientAddr string) ([]byte, error) {
	// Fast path: a single, final fragment (the overwhelmingly common case).
	if firstHeader.IsLast {
		return ReadRPCMessage(r, firstHeader.Length)
	}

	// Slow path: accumulate fragments until the last-fragment flag is seen.
	// The first fragment's buffer is pooled; subsequent fragments are read into
	// scratch buffers and appended.
	message, err := ReadRPCMessage(r, firstHeader.Length)
	if err != nil {
		return nil, err
	}

	total := uint32(len(message))
	for {
		header, herr := ReadFragmentHeader(r)
		if herr != nil {
			pool.Put(message)
			return nil, fmt.Errorf("read continuation fragment header: %w", herr)
		}

		// Bound the cumulative record size, not just each fragment, so a flood
		// of not-last fragments cannot drive unbounded growth.
		if header.Length > MaxFragmentSize-total {
			pool.Put(message)
			logger.Warn("RPC record exceeds maximum after reassembly",
				"accumulated", bytesize.ByteSize(total),
				"fragment", bytesize.ByteSize(header.Length),
				"max", bytesize.ByteSize(MaxFragmentSize),
				"address", clientAddr)
			return nil, fmt.Errorf("reassembled RPC record too large: %d + %d bytes", total, header.Length)
		}

		if header.Length > 0 {
			frag := make([]byte, header.Length)
			if _, rerr := io.ReadFull(r, frag); rerr != nil {
				pool.Put(message)
				return nil, fmt.Errorf("read continuation fragment: %w", rerr)
			}
			message = append(message, frag...)
			total += header.Length
		}

		if header.IsLast {
			return message, nil
		}
	}
}

// DemuxBackchannelReply checks if an RPC message is a backchannel REPLY (msg_type=1)
// rather than a CALL, and routes it to the pending callback replies handler.
//
// NFSv4.1 multiplexes fore-channel requests and backchannel replies on the same
// TCP connection. The first 8 bytes are XID (4 bytes) + msg_type (4 bytes).
//
// Returns true if the message was a backchannel reply and was handled (or dropped).
// Returns false if the message is a normal CALL that should be processed normally.
//
// When pending is nil (no backchannel bound), always returns false.
func DemuxBackchannelReply(message []byte, connectionID uint64, pending *state.PendingCBReplies) bool {
	if len(message) < 8 || pending == nil {
		return false
	}

	msgType := binary.BigEndian.Uint32(message[4:8])
	if msgType != rpc.RPCReply {
		return false
	}

	xid := binary.BigEndian.Uint32(message[0:4])

	// Copy the message bytes for delivery since the buffer is pooled
	replyBytes := make([]byte, len(message))
	copy(replyBytes, message)
	pool.Put(message) // Return pooled buffer

	if pending.Deliver(xid, replyBytes) {
		logger.Debug("Backchannel REPLY routed",
			"xid", fmt.Sprintf("0x%x", xid),
			"conn_id", connectionID)
	} else {
		logger.Debug("Backchannel REPLY for unknown XID (dropped)",
			"xid", fmt.Sprintf("0x%x", xid),
			"conn_id", connectionID)
	}

	return true
}
