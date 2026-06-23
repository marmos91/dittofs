// Package xdr provides XDR encoding and decoding for NLM v4 protocol messages.
//
// This package implements the wire format serialization for NLM (Network Lock
// Manager) protocol as specified by the Open Group. All functions follow
// RFC 4506 XDR encoding rules with 64-bit offsets/lengths per NLM v4.
package xdr

import (
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nfs/nlm/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/xdr/core"
)

// ============================================================================
// NLM Request Decoders
//
// The `wide` parameter selects the byte-range offset/length wire width:
//   - wide == true  -> 64-bit (unsigned hyper), used by NLM v4.
//   - wide == false -> 32-bit (unsigned int),   used by NLM v1/v3.
//
// macOS NFSv3 lock clients speak NLM v1/v3, so the narrow form must be honoured.
// All callers pass a width derived from the negotiated NLM version
// (see types.IsWideVersion).
// ============================================================================

// decodeNLMOffset reads a byte-range offset or length using the version's wire
// width and widens it to uint64 so handler logic stays version-agnostic.
func decodeNLMOffset(r io.Reader, wide bool) (uint64, error) {
	if wide {
		return xdr.DecodeUint64(r)
	}
	v, err := xdr.DecodeUint32(r)
	if err != nil {
		return 0, err
	}
	return uint64(v), nil
}

// DecodeNLM4Lock decodes an nlm_lock / nlm4_lock structure from XDR format.
//
// Wire format (l_offset/l_len are 64-bit when wide, 32-bit otherwise):
//
//	caller_name: [length:uint32][data:bytes][padding]
//	fh:          [length:uint32][data:bytes][padding]
//	oh:          [length:uint32][data:bytes][padding]
//	svid:        [int32]
//	l_offset:    [uint64|uint32]
//	l_len:       [uint64|uint32]
func DecodeNLM4Lock(r io.Reader, wide bool) (*types.NLM4Lock, error) {
	lock := &types.NLM4Lock{}

	// Decode caller_name (string)
	callerName, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode caller_name: %w", err)
	}
	if len(callerName) > types.LMMaxStrLen {
		return nil, fmt.Errorf("caller_name too long: %d > %d", len(callerName), types.LMMaxStrLen)
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

	// Decode l_offset (64-bit for v4, 32-bit for v1/v3)
	offset, err := decodeNLMOffset(r, wide)
	if err != nil {
		return nil, fmt.Errorf("decode l_offset: %w", err)
	}
	lock.Offset = offset

	// Decode l_len (64-bit for v4, 32-bit for v1/v3)
	length, err := decodeNLMOffset(r, wide)
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
func DecodeNLM4LockArgs(r io.Reader, wide bool) (*types.NLM4LockArgs, error) {
	args := &types.NLM4LockArgs{}

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
	lock, err := DecodeNLM4Lock(r, wide)
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
func DecodeNLM4UnlockArgs(r io.Reader, wide bool) (*types.NLM4UnlockArgs, error) {
	args := &types.NLM4UnlockArgs{}

	// Decode cookie (opaque)
	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	args.Cookie = cookie

	// Decode alock (nlm4_lock)
	lock, err := DecodeNLM4Lock(r, wide)
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
func DecodeNLM4TestArgs(r io.Reader, wide bool) (*types.NLM4TestArgs, error) {
	args := &types.NLM4TestArgs{}

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
	lock, err := DecodeNLM4Lock(r, wide)
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
func DecodeNLM4CancelArgs(r io.Reader, wide bool) (*types.NLM4CancelArgs, error) {
	args := &types.NLM4CancelArgs{}

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
	lock, err := DecodeNLM4Lock(r, wide)
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
func DecodeNLM4GrantedArgs(r io.Reader, wide bool) (*types.NLM4GrantedArgs, error) {
	args := &types.NLM4GrantedArgs{}

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
	lock, err := DecodeNLM4Lock(r, wide)
	if err != nil {
		return nil, fmt.Errorf("decode alock: %w", err)
	}
	args.Lock = *lock

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
func DecodeNLM4ShareArgs(r io.Reader) (*types.NLM4ShareArgs, error) {
	args := &types.NLM4ShareArgs{}

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
	if len(callerName) > types.LMMaxStrLen {
		return nil, fmt.Errorf("caller_name too long: %d > %d", len(callerName), types.LMMaxStrLen)
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

// DecodeNLM4ShareRes decodes an NLM4ShareRes response structure from XDR format.
//
// Wire format:
//
//	cookie:   [length:uint32][data:bytes][padding]
//	stat:     [uint32]
//	sequence: [int32]
func DecodeNLM4ShareRes(r io.Reader) (*types.NLM4ShareRes, error) {
	res := &types.NLM4ShareRes{}

	cookie, err := xdr.DecodeOpaque(r)
	if err != nil {
		return nil, fmt.Errorf("decode cookie: %w", err)
	}
	res.Cookie = cookie

	stat, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode stat: %w", err)
	}
	res.Status = stat

	seq, err := xdr.DecodeInt32(r)
	if err != nil {
		return nil, fmt.Errorf("decode sequence: %w", err)
	}
	res.Sequence = seq

	return res, nil
}
