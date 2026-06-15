package callback

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/types"
	"github.com/marmos91/dittofs/internal/adapter/nfs/rpc"
)

// PortmapPort is the well-known port for the portmapper (rpcbind) service.
// NLM clients register their dynamically-assigned NLM callback port with their
// local portmapper; the server resolves it via a GETPORT query.
const PortmapPort = 111

// resolveNLMCallbackPort queries the client's portmapper for the TCP port its
// NLM (lockd) service is listening on, so NLM_GRANTED callbacks reach the right
// endpoint rather than a hardcoded port.
//
// NLM clients listen for GRANTED callbacks on a dynamically-assigned port that
// they register with their local portmapper under (program=100021, version=4,
// protocol=TCP). The server discovers that port by sending a portmap GETPORT
// RPC to the client IP on port 111.
//
// Parameters:
//   - ctx: parent context for cancellation/timeout
//   - host: the client IP (must already be validated as the request source)
//   - prog: the NLM program number to look up
//   - vers: the NLM program version to look up
//
// Returns the resolved port, or an error if the query failed or the client's
// portmapper reported the program as not registered (port 0).
func ResolveNLMCallbackPort(ctx context.Context, host string, prog, vers uint32) (uint16, error) {
	return resolveViaPortmapAddr(ctx, host, PortmapPort, prog, vers)
}

// resolveViaPortmapAddr is the core GETPORT query against an explicit portmapper
// port. ResolveNLMCallbackPort fixes the port at the well-known 111; tests use a
// custom port to stand up a fake portmapper on an ephemeral port.
func resolveViaPortmapAddr(ctx context.Context, host string, portmapPort int, prog, vers uint32) (uint16, error) {
	addr := net.JoinHostPort(host, fmt.Sprintf("%d", portmapPort))

	// SSRF guard on the portmap dial too: an empty/unsafe host would otherwise
	// dial e.g. ":111" (the server's own portmapper) or a link-local target.
	// The host is the request's transport source, but validate defensively.
	if err := validateCallbackAddr(addr); err != nil {
		return 0, fmt.Errorf("reject portmapper address %s: %w", addr, err)
	}

	callbackCtx, cancel := context.WithTimeout(ctx, CallbackTimeout)
	defer cancel()

	var dialer net.Dialer
	conn, err := dialer.DialContext(callbackCtx, "tcp", addr)
	if err != nil {
		return 0, fmt.Errorf("dial portmapper %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := callbackCtx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return 0, fmt.Errorf("set deadline: %w", err)
		}
	}

	// GETPORT mapping argument: prog, vers, prot, port(=0, ignored).
	var argsBuf bytes.Buffer
	for _, v := range []uint32{prog, vers, types.ProtoTCP, 0} {
		if err := binary.Write(&argsBuf, binary.BigEndian, v); err != nil {
			return 0, fmt.Errorf("encode getport args: %w", err)
		}
	}

	xid := uint32(time.Now().UnixNano() & 0xFFFFFFFF)
	callMsg, err := buildRPCCallMessage(xid, types.ProgramPortmap, types.PortmapVersion2,
		types.ProcGetport, argsBuf.Bytes())
	if err != nil {
		return 0, fmt.Errorf("build getport call: %w", err)
	}

	if _, err := conn.Write(addRecordMark(callMsg, true)); err != nil {
		return 0, fmt.Errorf("write getport call: %w", err)
	}

	port, err := readGetportReply(conn)
	if err != nil {
		return 0, err
	}
	if port == 0 {
		return 0, fmt.Errorf("portmapper reports NLM (prog=%d vers=%d) not registered", prog, vers)
	}
	return uint16(port), nil
}

// readGetportReply reads the portmap GETPORT reply and returns the port.
//
// It validates the RPC reply is MSG_ACCEPTED/SUCCESS and extracts the trailing
// uint32 port value.
func readGetportReply(conn net.Conn) (uint32, error) {
	var headerBuf [4]byte
	if _, err := io.ReadFull(conn, headerBuf[:]); err != nil {
		return 0, fmt.Errorf("read getport reply header: %w", err)
	}
	fragLen := binary.BigEndian.Uint32(headerBuf[:]) & 0x7FFFFFFF
	if fragLen < 24 || fragLen > 1*1024*1024 {
		return 0, fmt.Errorf("getport reply fragment invalid length: %d", fragLen)
	}

	body := make([]byte, fragLen)
	if _, err := io.ReadFull(conn, body); err != nil {
		return 0, fmt.Errorf("read getport reply body: %w", err)
	}

	// RPC reply: xid(4) msg_type(4)=REPLY reply_stat(4)=MSG_ACCEPTED
	// verf(flavor 4 + len 4 + body) accept_stat(4)=SUCCESS then port(4).
	r := bytes.NewReader(body)
	var xid, msgType, replyStat uint32
	if err := readU32(r, &xid); err != nil {
		return 0, err
	}
	if err := readU32(r, &msgType); err != nil {
		return 0, err
	}
	if msgType != rpc.RPCReply {
		return 0, fmt.Errorf("getport reply: not a reply (msg_type=%d)", msgType)
	}
	if err := readU32(r, &replyStat); err != nil {
		return 0, err
	}
	if replyStat != 0 { // MSG_ACCEPTED
		return 0, fmt.Errorf("getport reply: rejected (reply_stat=%d)", replyStat)
	}

	// Verifier: flavor(4) + length(4) + body(length).
	var verfFlavor, verfLen uint32
	if err := readU32(r, &verfFlavor); err != nil {
		return 0, err
	}
	if err := readU32(r, &verfLen); err != nil {
		return 0, err
	}
	if verfLen > 0 {
		if _, err := io.CopyN(io.Discard, r, int64(verfLen)); err != nil {
			return 0, fmt.Errorf("getport reply: read verifier: %w", err)
		}
	}

	var acceptStat, port uint32
	if err := readU32(r, &acceptStat); err != nil {
		return 0, err
	}
	if acceptStat != 0 { // SUCCESS
		return 0, fmt.Errorf("getport reply: accept_stat=%d", acceptStat)
	}
	if err := readU32(r, &port); err != nil {
		return 0, err
	}
	return port, nil
}

func readU32(r io.Reader, v *uint32) error {
	return binary.Read(r, binary.BigEndian, v)
}
