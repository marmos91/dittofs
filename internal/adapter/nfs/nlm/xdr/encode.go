package xdr

import (
	"bytes"
	"fmt"
	"math"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
	"github.com/marmos91/dittofs/internal/logger"
)

// ============================================================================
// NLM Response Encoders
//
// The `wide` parameter selects byte-range offset/length wire width:
//   - wide == true  -> 64-bit (NLM v4).
//   - wide == false -> 32-bit (NLM v1/v3, e.g. macOS NFSv3 clients).
// ============================================================================

// encodeNLMOffset writes a byte-range offset/length using the version's wire
// width. Values are widened to uint64 internally.
//
// A v1/v3 client's own locks always fit in 32 bits, but the holder field of a
// TEST response can reflect a conflicting v4 lock whose 64-bit range exceeds 32
// bits. NLM v1/v3 cannot represent that, so the narrow form saturates to the
// 32-bit max rather than wrap-truncating — wrapping could fold a large offset
// down to a small, wrong one (e.g. 1<<32 -> 0) and misreport the conflict. The
// nlm_stat (DENIED) the client acts on is unaffected; only the reported holder
// bounds are clamped.
func encodeNLMOffset(buf *bytes.Buffer, v uint64, wide bool) error {
	if wide {
		return xdr.WriteUint64(buf, v)
	}
	if v > math.MaxUint32 {
		logger.Debug("NLM v1/v3: clamping 64-bit byte-range field to 32-bit max", "value", v)
		v = math.MaxUint32
	}
	return xdr.WriteUint32(buf, uint32(v))
}

// EncodeNLM4Res encodes an NLM4Res response structure to XDR format.
//
// Wire format:
//
//	cookie: [length:uint32][data:bytes][padding]
//	stat:   [uint32]
//
// Used by: NLM_LOCK, NLM_UNLOCK, NLM_CANCEL, NLM_GRANTED responses
func EncodeNLM4Res(buf *bytes.Buffer, res *types.NLM4Res) error {
	if res == nil {
		return fmt.Errorf("NLM4Res is nil")
	}

	// Encode cookie (opaque)
	if err := xdr.WriteXDROpaque(buf, res.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	// Encode stat (uint32)
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode stat: %w", err)
	}

	return nil
}

// EncodeNLM4TestRes encodes an NLM4TestRes response structure to XDR format.
//
// Wire format:
//
//	cookie: [length:uint32][data:bytes][padding]
//	stat:   [uint32]
//	if stat == NLM4_DENIED:
//	    holder: [nlm4_holder structure]
//
// If Status is NLM4Denied, Holder must be non-nil and will be encoded.
// If Status is anything else, Holder is ignored (not encoded).
func EncodeNLM4TestRes(buf *bytes.Buffer, res *types.NLM4TestRes, wide bool) error {
	if res == nil {
		return fmt.Errorf("NLM4TestRes is nil")
	}

	// Encode cookie (opaque)
	if err := xdr.WriteXDROpaque(buf, res.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	// Encode stat (uint32)
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode stat: %w", err)
	}

	// If denied, encode holder information
	if res.Status == types.NLM4Denied {
		if res.Holder == nil {
			return fmt.Errorf("NLM4TestRes.Holder is nil but status is DENIED")
		}
		if err := EncodeNLM4Holder(buf, res.Holder, wide); err != nil {
			return fmt.Errorf("encode holder: %w", err)
		}
	}

	return nil
}

// EncodeNLM4Holder encodes an NLM4Holder structure to XDR format.
//
// Wire format:
//
//	exclusive: [uint32] (0=false, 1=true)
//	svid:      [int32]
//	oh:        [length:uint32][data:bytes][padding]
//	l_offset:  [uint64]
//	l_len:     [uint64]
func EncodeNLM4Holder(buf *bytes.Buffer, holder *types.NLM4Holder, wide bool) error {
	if holder == nil {
		return fmt.Errorf("NLM4Holder is nil")
	}

	// Encode exclusive (bool)
	if err := xdr.WriteBool(buf, holder.Exclusive); err != nil {
		return fmt.Errorf("encode exclusive: %w", err)
	}

	// Encode svid (int32)
	if err := xdr.WriteInt32(buf, holder.Svid); err != nil {
		return fmt.Errorf("encode svid: %w", err)
	}

	// Encode oh (opaque)
	if err := xdr.WriteXDROpaque(buf, holder.OH); err != nil {
		return fmt.Errorf("encode oh: %w", err)
	}

	// Encode l_offset (64-bit for v4, 32-bit for v1/v3)
	if err := encodeNLMOffset(buf, holder.Offset, wide); err != nil {
		return fmt.Errorf("encode l_offset: %w", err)
	}

	// Encode l_len (64-bit for v4, 32-bit for v1/v3)
	if err := encodeNLMOffset(buf, holder.Length, wide); err != nil {
		return fmt.Errorf("encode l_len: %w", err)
	}

	return nil
}

// EncodeNLM4GrantedArgs encodes NLM_GRANTED callback arguments to XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
//
// Used when server calls back to client to notify that a blocked lock
// has been granted.
func EncodeNLM4GrantedArgs(buf *bytes.Buffer, args *types.NLM4GrantedArgs, wide bool) error {
	if args == nil {
		return fmt.Errorf("NLM4GrantedArgs is nil")
	}

	// Encode cookie (opaque)
	if err := xdr.WriteXDROpaque(buf, args.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	// Encode exclusive (bool)
	if err := xdr.WriteBool(buf, args.Exclusive); err != nil {
		return fmt.Errorf("encode exclusive: %w", err)
	}

	// Encode alock (nlm4_lock)
	if err := EncodeNLM4Lock(buf, &args.Lock, wide); err != nil {
		return fmt.Errorf("encode alock: %w", err)
	}

	return nil
}

// EncodeNLM4Lock encodes an NLM4Lock structure to XDR format.
//
// Wire format:
//
//	caller_name: [length:uint32][data:bytes][padding]
//	fh:          [length:uint32][data:bytes][padding]
//	oh:          [length:uint32][data:bytes][padding]
//	svid:        [int32]
//	l_offset:    [uint64]
//	l_len:       [uint64]
func EncodeNLM4Lock(buf *bytes.Buffer, lock *types.NLM4Lock, wide bool) error {
	if lock == nil {
		return fmt.Errorf("NLM4Lock is nil")
	}

	// Encode caller_name (string)
	if err := xdr.WriteXDRString(buf, lock.CallerName); err != nil {
		return fmt.Errorf("encode caller_name: %w", err)
	}

	// Encode fh (opaque)
	if err := xdr.WriteXDROpaque(buf, lock.FH); err != nil {
		return fmt.Errorf("encode fh: %w", err)
	}

	// Encode oh (opaque)
	if err := xdr.WriteXDROpaque(buf, lock.OH); err != nil {
		return fmt.Errorf("encode oh: %w", err)
	}

	// Encode svid (int32)
	if err := xdr.WriteInt32(buf, lock.Svid); err != nil {
		return fmt.Errorf("encode svid: %w", err)
	}

	// Encode l_offset (64-bit for v4, 32-bit for v1/v3)
	if err := encodeNLMOffset(buf, lock.Offset, wide); err != nil {
		return fmt.Errorf("encode l_offset: %w", err)
	}

	// Encode l_len (64-bit for v4, 32-bit for v1/v3)
	if err := encodeNLMOffset(buf, lock.Length, wide); err != nil {
		return fmt.Errorf("encode l_len: %w", err)
	}

	return nil
}

// EncodeNLM4ShareRes encodes an NLM4ShareRes response structure to XDR format.
//
// Wire format:
//
//	cookie:   [length:uint32][data:bytes][padding]
//	stat:     [uint32]
//	sequence: [int32]
func EncodeNLM4ShareRes(buf *bytes.Buffer, res *types.NLM4ShareRes) error {
	if res == nil {
		return fmt.Errorf("NLM4ShareRes is nil")
	}

	// Encode cookie (opaque)
	if err := xdr.WriteXDROpaque(buf, res.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	// Encode stat (uint32)
	if err := xdr.WriteUint32(buf, res.Status); err != nil {
		return fmt.Errorf("encode stat: %w", err)
	}

	// Encode sequence (int32)
	if err := xdr.WriteInt32(buf, res.Sequence); err != nil {
		return fmt.Errorf("encode sequence: %w", err)
	}

	return nil
}

// ============================================================================
// NLM v4 Request Encoders (for testing and callbacks)
// ============================================================================

// EncodeNLM4ShareArgs encodes NLM_SHARE/NLM_UNSHARE request arguments to XDR format.
//
// Wire format:
//
//	cookie:      [length:uint32][data:bytes][padding]
//	caller_name: [length:uint32][data:bytes][padding]
//	fh:          [length:uint32][data:bytes][padding]
//	oh:          [length:uint32][data:bytes][padding]
//	mode:        [uint32]
//	access:      [uint32]
//	reclaim:     [uint32] (0=false, 1=true)
//
// Primarily used for testing decode functions.
func EncodeNLM4ShareArgs(buf *bytes.Buffer, args *types.NLM4ShareArgs) error {
	if args == nil {
		return fmt.Errorf("NLM4ShareArgs is nil")
	}

	if err := xdr.WriteXDROpaque(buf, args.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	if err := xdr.WriteXDRString(buf, args.CallerName); err != nil {
		return fmt.Errorf("encode caller_name: %w", err)
	}

	if err := xdr.WriteXDROpaque(buf, args.FH); err != nil {
		return fmt.Errorf("encode fh: %w", err)
	}

	if err := xdr.WriteXDROpaque(buf, args.OH); err != nil {
		return fmt.Errorf("encode oh: %w", err)
	}

	if err := xdr.WriteUint32(buf, args.Mode); err != nil {
		return fmt.Errorf("encode mode: %w", err)
	}

	if err := xdr.WriteUint32(buf, args.Access); err != nil {
		return fmt.Errorf("encode access: %w", err)
	}

	if err := xdr.WriteBool(buf, args.Reclaim); err != nil {
		return fmt.Errorf("encode reclaim: %w", err)
	}

	return nil
}
