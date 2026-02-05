package xdr

import (
	"bytes"
	"fmt"

	"github.com/marmos91/dittofs/internal/protocol/nlm"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// NLM v4 Response Encoders
// ============================================================================

// EncodeNLM4Res encodes an NLM4Res response structure to XDR format.
//
// Wire format:
//
//	cookie: [length:uint32][data:bytes][padding]
//	stat:   [uint32]
//
// Used by: NLM_LOCK, NLM_UNLOCK, NLM_CANCEL, NLM_GRANTED responses
func EncodeNLM4Res(buf *bytes.Buffer, res *nlm.NLM4Res) error {
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
func EncodeNLM4TestRes(buf *bytes.Buffer, res *nlm.NLM4TestRes) error {
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
	if res.Status == nlm.NLM4Denied {
		if res.Holder == nil {
			return fmt.Errorf("NLM4TestRes.Holder is nil but status is DENIED")
		}
		if err := EncodeNLM4Holder(buf, res.Holder); err != nil {
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
func EncodeNLM4Holder(buf *bytes.Buffer, holder *nlm.NLM4Holder) error {
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

	// Encode l_offset (uint64)
	if err := xdr.WriteUint64(buf, holder.Offset); err != nil {
		return fmt.Errorf("encode l_offset: %w", err)
	}

	// Encode l_len (uint64)
	if err := xdr.WriteUint64(buf, holder.Length); err != nil {
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
func EncodeNLM4GrantedArgs(buf *bytes.Buffer, args *nlm.NLM4GrantedArgs) error {
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
	if err := EncodeNLM4Lock(buf, &args.Lock); err != nil {
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
func EncodeNLM4Lock(buf *bytes.Buffer, lock *nlm.NLM4Lock) error {
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

	// Encode l_offset (uint64)
	if err := xdr.WriteUint64(buf, lock.Offset); err != nil {
		return fmt.Errorf("encode l_offset: %w", err)
	}

	// Encode l_len (uint64)
	if err := xdr.WriteUint64(buf, lock.Length); err != nil {
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
func EncodeNLM4ShareRes(buf *bytes.Buffer, res *nlm.NLM4ShareRes) error {
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

// EncodeNLM4LockArgs encodes NLM_LOCK request arguments to XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	block:     [uint32] (0=false, 1=true)
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
//	reclaim:   [uint32] (0=false, 1=true)
//	state:     [int32]
//
// Primarily used for testing decode functions.
func EncodeNLM4LockArgs(buf *bytes.Buffer, args *nlm.NLM4LockArgs) error {
	if args == nil {
		return fmt.Errorf("NLM4LockArgs is nil")
	}

	// Encode cookie (opaque)
	if err := xdr.WriteXDROpaque(buf, args.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	// Encode block (bool)
	if err := xdr.WriteBool(buf, args.Block); err != nil {
		return fmt.Errorf("encode block: %w", err)
	}

	// Encode exclusive (bool)
	if err := xdr.WriteBool(buf, args.Exclusive); err != nil {
		return fmt.Errorf("encode exclusive: %w", err)
	}

	// Encode alock (nlm4_lock)
	if err := EncodeNLM4Lock(buf, &args.Lock); err != nil {
		return fmt.Errorf("encode alock: %w", err)
	}

	// Encode reclaim (bool)
	if err := xdr.WriteBool(buf, args.Reclaim); err != nil {
		return fmt.Errorf("encode reclaim: %w", err)
	}

	// Encode state (int32)
	if err := xdr.WriteInt32(buf, args.State); err != nil {
		return fmt.Errorf("encode state: %w", err)
	}

	return nil
}

// EncodeNLM4UnlockArgs encodes NLM_UNLOCK request arguments to XDR format.
//
// Wire format:
//
//	cookie: [length:uint32][data:bytes][padding]
//	alock:  [nlm4_lock structure]
func EncodeNLM4UnlockArgs(buf *bytes.Buffer, args *nlm.NLM4UnlockArgs) error {
	if args == nil {
		return fmt.Errorf("NLM4UnlockArgs is nil")
	}

	// Encode cookie (opaque)
	if err := xdr.WriteXDROpaque(buf, args.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	// Encode alock (nlm4_lock)
	if err := EncodeNLM4Lock(buf, &args.Lock); err != nil {
		return fmt.Errorf("encode alock: %w", err)
	}

	return nil
}

// EncodeNLM4TestArgs encodes NLM_TEST request arguments to XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
func EncodeNLM4TestArgs(buf *bytes.Buffer, args *nlm.NLM4TestArgs) error {
	if args == nil {
		return fmt.Errorf("NLM4TestArgs is nil")
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
	if err := EncodeNLM4Lock(buf, &args.Lock); err != nil {
		return fmt.Errorf("encode alock: %w", err)
	}

	return nil
}

// EncodeNLM4CancelArgs encodes NLM_CANCEL request arguments to XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	block:     [uint32] (0=false, 1=true)
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
func EncodeNLM4CancelArgs(buf *bytes.Buffer, args *nlm.NLM4CancelArgs) error {
	if args == nil {
		return fmt.Errorf("NLM4CancelArgs is nil")
	}

	// Encode cookie (opaque)
	if err := xdr.WriteXDROpaque(buf, args.Cookie); err != nil {
		return fmt.Errorf("encode cookie: %w", err)
	}

	// Encode block (bool)
	if err := xdr.WriteBool(buf, args.Block); err != nil {
		return fmt.Errorf("encode block: %w", err)
	}

	// Encode exclusive (bool)
	if err := xdr.WriteBool(buf, args.Exclusive); err != nil {
		return fmt.Errorf("encode exclusive: %w", err)
	}

	// Encode alock (nlm4_lock)
	if err := EncodeNLM4Lock(buf, &args.Lock); err != nil {
		return fmt.Errorf("encode alock: %w", err)
	}

	return nil
}
