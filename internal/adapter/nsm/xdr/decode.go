// Package xdr provides XDR encoding/decoding for NSM protocol messages.
//
// This package uses the shared XDR utilities from internal/adapter/xdr
// for primitive type encoding/decoding, following the same pattern as
// the NLM XDR package.
package xdr

import (
	"fmt"
	"io"

	"github.com/marmos91/dittofs/internal/adapter/nsm/types"
	"github.com/marmos91/dittofs/internal/adapter/xdr"
)

// ============================================================================
// NSM Request Decoding
// ============================================================================

// DecodeSmName decodes an sm_name structure (SM_STAT, SM_UNMON argument).
//
// XDR format:
//
//	struct sm_name {
//	    string mon_name<SM_MAXSTRLEN>;
//	};
func DecodeSmName(r io.Reader) (*types.SMName, error) {
	name, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode name: %w", err)
	}
	if len(name) > types.SMMaxStrLen {
		return nil, fmt.Errorf("name too long: %d > %d", len(name), types.SMMaxStrLen)
	}
	return &types.SMName{Name: name}, nil
}

// DecodeMyID decodes a my_id structure (callback RPC info).
//
// XDR format:
//
//	struct my_id {
//	    string my_name<SM_MAXSTRLEN>;
//	    int    my_prog;
//	    int    my_vers;
//	    int    my_proc;
//	};
func DecodeMyID(r io.Reader) (*types.MyID, error) {
	myName, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode my_name: %w", err)
	}
	if len(myName) > types.SMMaxStrLen {
		return nil, fmt.Errorf("my_name too long: %d > %d", len(myName), types.SMMaxStrLen)
	}

	myProg, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode my_prog: %w", err)
	}

	myVers, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode my_vers: %w", err)
	}

	myProc, err := xdr.DecodeUint32(r)
	if err != nil {
		return nil, fmt.Errorf("decode my_proc: %w", err)
	}

	return &types.MyID{
		MyName: myName,
		MyProg: myProg,
		MyVers: myVers,
		MyProc: myProc,
	}, nil
}

// DecodeMonID decodes a mon_id structure.
//
// XDR format:
//
//	struct mon_id {
//	    string mon_name<SM_MAXSTRLEN>;
//	    my_id  my_id;
//	};
func DecodeMonID(r io.Reader) (*types.MonID, error) {
	monName, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode mon_name: %w", err)
	}
	if len(monName) > types.SMMaxStrLen {
		return nil, fmt.Errorf("mon_name too long: %d > %d", len(monName), types.SMMaxStrLen)
	}

	myID, err := DecodeMyID(r)
	if err != nil {
		return nil, fmt.Errorf("decode my_id: %w", err)
	}

	return &types.MonID{
		MonName: monName,
		MyID:    *myID,
	}, nil
}

// DecodeMon decodes SM_MON arguments (mon structure).
//
// XDR format:
//
//	struct mon {
//	    mon_id   mon_id;
//	    opaque   priv[16];
//	};
//
// Note: priv is a fixed-size opaque[16], not variable-length.
// Per XDR, fixed-size opaque has no length prefix.
func DecodeMon(r io.Reader) (*types.Mon, error) {
	monID, err := DecodeMonID(r)
	if err != nil {
		return nil, fmt.Errorf("decode mon_id: %w", err)
	}

	// Decode priv as opaque[16] (fixed size, no length prefix)
	var priv [16]byte
	if _, err := io.ReadFull(r, priv[:]); err != nil {
		return nil, fmt.Errorf("decode priv: %w", err)
	}

	return &types.Mon{
		MonID: *monID,
		Priv:  priv,
	}, nil
}

// DecodeStatChge decodes SM_NOTIFY arguments (stat_chge structure).
//
// XDR format:
//
//	struct stat_chge {
//	    string   mon_name<SM_MAXSTRLEN>;
//	    int      state;
//	};
func DecodeStatChge(r io.Reader) (*types.StatChge, error) {
	monName, err := xdr.DecodeString(r)
	if err != nil {
		return nil, fmt.Errorf("decode mon_name: %w", err)
	}
	if len(monName) > types.SMMaxStrLen {
		return nil, fmt.Errorf("mon_name too long: %d > %d", len(monName), types.SMMaxStrLen)
	}

	state, err := xdr.DecodeInt32(r)
	if err != nil {
		return nil, fmt.Errorf("decode state: %w", err)
	}

	return &types.StatChge{
		MonName: monName,
		State:   state,
	}, nil
}
