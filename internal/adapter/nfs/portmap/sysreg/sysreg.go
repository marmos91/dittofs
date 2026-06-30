// Package sysreg registers DittoFS's RPC services (NFS, MOUNT, NLM, NSM) with
// an external system portmapper/rpcbind listening on the well-known port 111.
//
// A kernel NFSv3 client discovers the NLM (lockd) port by querying rpcbind on
// port 111 — a location fixed by RFC 1833 with no client-side override. When a
// host already runs a system rpcbind (the common case), DittoFS cannot bind 111
// itself, so a client mounted without `nolock` finds no NLM registration and
// lockd hangs. This package closes that gap the same way rpc.nfsd / rpc.statd
// do: it SETs DittoFS's service mappings into the running rpcbind at startup
// and UNSETs them at shutdown. Pure Go (portmap v2 SET/UNSET over TCP), no CGO.
package sysreg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// SystemPortmapPort is the well-known portmapper/rpcbind port (RFC 1833).
const SystemPortmapPort = 111

// dialTimeout bounds each SET/UNSET/NULL round-trip to the local portmapper.
const dialTimeout = 5 * time.Second

// Ping issues a portmap NULL (proc 0) against the portmapper at addr to confirm
// a portmapper is actually listening before attempting to register. Returns nil
// when the NULL is accepted, an error otherwise (no portmapper / unreachable).
func Ping(ctx context.Context, addr string) error {
	return call(ctx, addr, types.ProcNull, nil, false)
}

// Register claims every mapping in the portmapper at addr (typically
// 127.0.0.1:111). For each tuple it UNSETs then SETs: a plain SET is refused by
// rpcbind when the (prog, vers, prot) is already registered (e.g. a stale entry
// from a previous run), so we first drop any existing mapping — safe because
// these are DittoFS's own services (the caller must NOT pass shared programs
// such as NSM, whose host owner would be hijacked). See
// (*NFSAdapter).startSystemPortmapRegistration, which filters NSM out.
//
// Best effort: it attempts every tuple and returns the joined set of SETs that
// still failed, rather than aborting on the first — a single conflicting tuple
// must not stop the critical NLM mapping from being registered.
func Register(ctx context.Context, addr string, mappings []*xdr.Mapping) error {
	var errs []error
	for _, m := range mappings {
		// Try a plain SET first. Only on a conflict (the tuple is already
		// registered) do we UNSET then retry to claim it. This ordering matters:
		// rpcbind's UNSET ignores the protocol and drops BOTH the TCP and UDP
		// mapping of a (prog, vers), so unconditionally UNSETting every tuple
		// would have the UDP entry wipe the TCP mapping we just set. Claiming
		// only on conflict avoids touching a sibling protocol we already
		// registered this pass.
		if err := call(ctx, addr, types.ProcSet, m, true); err == nil {
			continue
		}
		_ = call(ctx, addr, types.ProcUnset, m, true)
		if err := call(ctx, addr, types.ProcSet, m, true); err != nil {
			errs = append(errs, fmt.Errorf("portmap SET (prog=%d vers=%d prot=%d port=%d): %w",
				m.Prog, m.Vers, m.Prot, m.Port, err))
		}
	}
	return errors.Join(errs...)
}

// Unregister UNSETs every mapping from the portmapper at addr. It is best
// effort: it attempts all mappings and returns the first error (if any) so a
// single stale tuple does not abort cleanup of the rest.
func Unregister(ctx context.Context, addr string, mappings []*xdr.Mapping) error {
	var firstErr error
	for _, m := range mappings {
		if err := call(ctx, addr, types.ProcUnset, m, true); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("portmap UNSET (prog=%d vers=%d): %w", m.Prog, m.Vers, err)
		}
	}
	return firstErr
}

// call performs one portmap RPC against addr. When m is non-nil its (prog, vers,
// prot, port) tuple is sent as the mapping argument (SET/UNSET); for NULL m is
// nil. wantBool indicates the reply carries a trailing xdr bool result that must
// be true (SET/UNSET succeeded); NULL carries no result.
func call(ctx context.Context, addr string, proc uint32, m *xdr.Mapping, wantBool bool) error {
	callCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(callCtx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("dial portmapper %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := callCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return fmt.Errorf("set deadline: %w", err)
		}
	}

	var args []byte
	if m != nil {
		// portmap mapping arg: prog, vers, prot, port — four big-endian uint32s.
		var buf bytes.Buffer
		for _, v := range []uint32{m.Prog, m.Vers, m.Prot, m.Port} {
			_ = binary.Write(&buf, binary.BigEndian, v)
		}
		args = buf.Bytes()
	}

	xid := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	callMsg := buildCall(xid, types.ProgramPortmap, types.PortmapVersion2, proc, args)
	if _, err := conn.Write(addRecordMark(callMsg)); err != nil {
		return fmt.Errorf("write portmap call: %w", err)
	}
	return readReply(conn, wantBool)
}

// buildCall encodes an RPC CALL message with AUTH_NULL credentials and verifier
// (RFC 5531): xid, msg_type=CALL, rpcvers=2, prog, vers, proc, cred{0,0},
// verf{0,0}, then args.
func buildCall(xid, prog, vers, proc uint32, args []byte) []byte {
	var b bytes.Buffer
	write := func(v uint32) { _ = binary.Write(&b, binary.BigEndian, v) }
	write(xid)
	write(0) // msg_type = CALL
	write(2) // rpcvers = 2
	write(prog)
	write(vers)
	write(proc)
	write(0) // cred flavor = AUTH_NULL
	write(0) // cred length = 0
	write(0) // verf flavor = AUTH_NULL
	write(0) // verf length = 0
	b.Write(args)
	return b.Bytes()
}

// addRecordMark prepends the 4-byte record-marking header for a single final
// RPC fragment over a stream transport (high bit set | length).
func addRecordMark(msg []byte) []byte {
	out := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(out[0:4], 0x80000000|uint32(len(msg)))
	copy(out[4:], msg)
	return out
}

// readReply reads a record-marked RPC reply and validates it is
// MSG_ACCEPTED/SUCCESS. When wantBool is set the trailing xdr bool result must
// be non-zero (the portmapper accepted the SET/UNSET).
func readReply(conn net.Conn, wantBool bool) error {
	var markBuf [4]byte
	if _, err := io.ReadFull(conn, markBuf[:]); err != nil {
		return fmt.Errorf("read reply record mark: %w", err)
	}
	fragLen := binary.BigEndian.Uint32(markBuf[:]) & 0x7FFFFFFF
	if fragLen < 24 || fragLen > 1*1024*1024 {
		return fmt.Errorf("reply fragment invalid length: %d", fragLen)
	}

	body := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return fmt.Errorf("read reply body: %w", err)
	}

	r := bytes.NewReader(body)
	var xid, msgType, replyStat uint32
	if err := readU32(r, &xid); err != nil {
		return err
	}
	if err := readU32(r, &msgType); err != nil {
		return err
	}
	if msgType != rpc.RPCReply {
		return fmt.Errorf("not a reply (msg_type=%d)", msgType)
	}
	if err := readU32(r, &replyStat); err != nil {
		return err
	}
	if replyStat != 0 { // MSG_ACCEPTED
		return fmt.Errorf("rpc rejected (reply_stat=%d)", replyStat)
	}

	// Verifier: flavor(4) + length(4) + padded body.
	var verfFlavor, verfLen uint32
	if err := readU32(r, &verfFlavor); err != nil {
		return err
	}
	if err := readU32(r, &verfLen); err != nil {
		return err
	}
	if padded := (verfLen + 3) &^ 3; padded > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(padded)); err != nil {
			return fmt.Errorf("read verifier: %w", err)
		}
	}

	var acceptStat uint32
	if err := readU32(r, &acceptStat); err != nil {
		return err
	}
	if acceptStat != 0 { // SUCCESS
		return fmt.Errorf("accept_stat=%d", acceptStat)
	}

	if !wantBool {
		return nil
	}
	var result uint32
	if err := readU32(r, &result); err != nil {
		return err
	}
	if result == 0 {
		return fmt.Errorf("portmapper refused the mapping (result=false)")
	}
	return nil
}

func readU32(r io.Reader, v *uint32) error {
	return binary.Read(r, binary.BigEndian, v)
}
