// Package xdr provides XDR encoding and decoding for NLM v4 protocol messages.
//
// This package implements the wire format serialization for NLM (Network Lock
// Manager) protocol as specified by the Open Group. All functions follow
// RFC 4506 XDR encoding rules with 64-bit offsets/lengths per NLM v4.
package xdr

import (
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/protocol/nlm"
	"github.com/marmos91/dittofs/internal/protocol/xdr"
)

// ============================================================================
// NLM v4 Request Decoders
// ============================================================================

// DecodeNLM4Lock decodes an NLM4Lock structure from XDR format.
//
// Wire format:
//
//	caller_name: [length:uint32][data:bytes][padding]
//	fh:          [length:uint32][data:bytes][padding]
//	oh:          [length:uint32][data:bytes][padding]
//	svid:        [int32]
//	l_offset:    [uint64]
//	l_len:       [uint64]
func DecodeNLM4Lock(r io.Reader) (*nlm.NLM4Lock, error) {
	lock := &nlm.NLM4Lock{}

	// Decode caller_name (string)
	callerName, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode caller_name: %w", err)
	}
	if len(callerName) > nlm.LMMaxStrLen {
		return nil, fmt.Errorf("caller_name too long: %d > %d", len(callerName), nlm.LMMaxStrLen)
	}
	lock.CallerName = callerName

	// Decode fh (file handle - opaque)
	fh, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode fh: %w", err)
	}
	lock.FH = fh

	// Decode oh (owner handle - opaque)
	oh, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode oh: %w", err)
	}
	lock.OH = oh

	// Decode svid (int32)
	svid, err := xdr.DecodeInt32(r)
	if err != nil {
		return nil, fmt.Errorf("decode svid: %w", err)
	}
	lock.Svid = svid

	// Decode l_offset (uint64) - NLM v4 uses 64-bit
	offset, err := xdr.DecodeUint64(r)
	if err != nil {
		return nil, fmt.Errorf("decode l_offset: %w", err)
	}
	lock.Offset = offset

	// Decode l_len (uint64) - NLM v4 uses 64-bit
	length, err := xdr.DecodeUint64(r)
	if err != nil {
		return nil, fmt.Errorf("decode l_len: %w", err)
	}
	lock.Length = length

	return lock, nil
}

// DecodeNLM4LockArgs decodes NLM_LOCK request arguments from XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	block:     [uint32] (0=false, 1=true)
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
//	reclaim:   [uint32] (0=false, 1=true)
//	state:     [int32]
func DecodeNLM4LockArgs(r io.Reader) (*nlm.NLM4LockArgs, error) {
	args := &nlm.NLM4LockArgs{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	args.Cookie = cookie

	// Decode block (bool)
	block, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode block: %w", err)
	}
	args.Block = block

	// Decode exclusive (bool)
	exclusive, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode exclusive: %w", err)
	}
	args.Exclusive = exclusive

	// Decode alock (nlm4_lock)
	lock, err := DecodeNLM4Lock(r)
	if err != nil {
		return nil, fmt.Errorf("decode alock: %w", err)
	}
	args.Lock = *lock

	// Decode reclaim (bool)
	reclaim, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode reclaim: %w", err)
	}
	args.Reclaim = reclaim

	// Decode state (int32)
	state, err := xdr.DecodeInt32(r)
	if err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	args.State = state

	return args, nil
}

// DecodeNLM4UnlockArgs decodes NLM_UNLOCK request arguments from XDR format.
//
// Wire format:
//
//	cookie: [length:uint32][data:bytes][padding]
//	alock:  [nlm4_lock structure]
func DecodeNLM4UnlockArgs(r io.Reader) (*nlm.NLM4UnlockArgs, error) {
	args := &nlm.NLM4UnlockArgs{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	args.Cookie = cookie

	// Decode alock (nlm4_lock)
	lock, err := DecodeNLM4Lock(r)
	if err != nil {
		return nil, fmt.Errorf("decode alock: %w", err)
	}
	args.Lock = *lock

	return args, nil
}

// DecodeNLM4TestArgs decodes NLM_TEST request arguments from XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
func DecodeNLM4TestArgs(r io.Reader) (*nlm.NLM4TestArgs, error) {
	args := &nlm.NLM4TestArgs{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	args.Cookie = cookie

	// Decode exclusive (bool)
	exclusive, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode exclusive: %w", err)
	}
	args.Exclusive = exclusive

	// Decode alock (nlm4_lock)
	lock, err := DecodeNLM4Lock(r)
	if err != nil {
		return nil, fmt.Errorf("decode alock: %w", err)
	}
	args.Lock = *lock

	return args, nil
}

// DecodeNLM4CancelArgs decodes NLM_CANCEL request arguments from XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	block:     [uint32] (0=false, 1=true)
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
func DecodeNLM4CancelArgs(r io.Reader) (*nlm.NLM4CancelArgs, error) {
	args := &nlm.NLM4CancelArgs{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	args.Cookie = cookie

	// Decode block (bool)
	block, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode block: %w", err)
	}
	args.Block = block

	// Decode exclusive (bool)
	exclusive, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode exclusive: %w", err)
	}
	args.Exclusive = exclusive

	// Decode alock (nlm4_lock)
	lock, err := DecodeNLM4Lock(r)
	if err != nil {
		return nil, fmt.Errorf("decode alock: %w", err)
	}
	args.Lock = *lock

	return args, nil
}

// DecodeNLM4GrantedArgs decodes NLM_GRANTED callback arguments from XDR format.
//
// Wire format:
//
//	cookie:    [length:uint32][data:bytes][padding]
//	exclusive: [uint32] (0=false, 1=true)
//	alock:     [nlm4_lock structure]
//
// Note: GRANTED uses the same format as TEST args.
func DecodeNLM4GrantedArgs(r io.Reader) (*nlm.NLM4GrantedArgs, error) {
	args := &nlm.NLM4GrantedArgs{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	args.Cookie = cookie

	// Decode exclusive (bool)
	exclusive, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode exclusive: %w", err)
	}
	args.Exclusive = exclusive

	// Decode alock (nlm4_lock)
	lock, err := DecodeNLM4Lock(r)
	if err != nil {
		return nil, fmt.Errorf("decode alock: %w", err)
	}
	args.Lock = *lock

	return args, nil
}

// DecodeNLM4FreeAllArgs decodes NLM_FREE_ALL request arguments from XDR format.
//
// Wire format:
//
//	name:  [length:uint32][data:bytes][padding]
//	state: [int32]
func DecodeNLM4FreeAllArgs(r io.Reader) (*nlm.NLM4FreeAllArgs, error) {
	args := &nlm.NLM4FreeAllArgs{}

	// Decode name (string)
	name, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	if len(name) > nlm.LMMaxStrLen {
		return nil, fmt.Errorf("name too long: %d > %d", len(name), nlm.LMMaxStrLen)
	}
	args.Name = name

	// Decode state (int32)
	state, err := xdr.DecodeInt32(r)
	if err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}
	args.State = state

	return args, nil
}

// DecodeNLM4ShareArgs decodes NLM_SHARE/NLM_UNSHARE arguments from XDR format.
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
func DecodeNLM4ShareArgs(r io.Reader) (*nlm.NLM4ShareArgs, error) {
	args := &nlm.NLM4ShareArgs{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	args.Cookie = cookie

	// Decode caller_name (string)
	callerName, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode caller_name: %w", err)
	}
	if len(callerName) > nlm.LMMaxStrLen {
		return nil, fmt.Errorf("caller_name too long: %d > %d", len(callerName), nlm.LMMaxStrLen)
	}
	args.CallerName = callerName

	// Decode fh (file handle - opaque)
	fh, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode fh: %w", err)
	}
	args.FH = fh

	// Decode oh (owner handle - opaque)
	oh, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode oh: %w", err)
	}
	args.OH = oh

	// Decode mode (uint32)
	mode, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode mode: %w", err)
	}
	args.Mode = mode

	// Decode access (uint32)
	access, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode access: %w", err)
	}
	args.Access = access

	// Decode reclaim (bool)
	reclaim, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode reclaim: %w", err)
	}
	args.Reclaim = reclaim

	return args, nil
}

// DecodeNLM4Holder decodes an NLM4Holder structure from XDR format.
//
// Wire format:
//
//	exclusive: [uint32] (0=false, 1=true)
//	svid:      [int32]
//	oh:        [length:uint32][data:bytes][padding]
//	l_offset:  [uint64]
//	l_len:     [uint64]
func DecodeNLM4Holder(r io.Reader) (*nlm.NLM4Holder, error) {
	holder := &nlm.NLM4Holder{}

	// Decode exclusive (bool)
	exclusive, err := xdr.DecodeBool(r)
	if err != nil {
		return nil, fmt.Errorf("decode exclusive: %w", err)
	}
	holder.Exclusive = exclusive

	// Decode svid (int32)
	svid, err := xdr.DecodeInt32(r)
	if err != nil {
		return nil, fmt.Errorf("decode svid: %w", err)
	}
	holder.Svid = svid

	// Decode oh (owner handle - opaque)
	oh, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode oh: %w", err)
	}
	holder.OH = oh

	// Decode l_offset (uint64)
	offset, err := xdr.DecodeUint64(r)
	if err != nil {
		return nil, fmt.Errorf("decode l_offset: %w", err)
	}
	holder.Offset = offset

	// Decode l_len (uint64)
	length, err := xdr.DecodeUint64(r)
	if err != nil {
		return nil, fmt.Errorf("decode l_len: %w", err)
	}
	holder.Length = length

	return holder, nil
}

// DecodeNLM4Res decodes an NLM4Res response structure from XDR format.
//
// Wire format:
//
//	cookie: [length:uint32][data:bytes][padding]
//	stat:   [uint32]
func DecodeNLM4Res(r io.Reader) (*nlm.NLM4Res, error) {
	res := &nlm.NLM4Res{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	res.Cookie = cookie

	// Decode stat (uint32)
	stat, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode stat: %w", err)
	}
	res.Status = stat

	return res, nil
}

// DecodeNLM4TestRes decodes an NLM4TestRes response structure from XDR format.
//
// Wire format:
//
//	cookie: [length:uint32][data:bytes][padding]
//	stat:   [uint32]
//	if stat == NLM4_DENIED:
//	    holder: [nlm4_holder structure]
func DecodeNLM4TestRes(r io.Reader) (*nlm.NLM4TestRes, error) {
	res := &nlm.NLM4TestRes{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	res.Cookie = cookie

	// Decode stat (uint32)
	stat, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode stat: %w", err)
	}
	res.Status = stat

	// If denied, decode holder information
	if stat == nlm.NLM4Denied {
		holder, err := DecodeNLM4Holder(r)
		if err != nil {
			return nil, fmt.Errorf("decode holder: %w", err)
		}
		res.Holder = holder
	}

	return res, nil
}
