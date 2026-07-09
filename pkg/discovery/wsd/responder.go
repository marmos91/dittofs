package wsd

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
)

const (
	// discoveryGroupV4 / discoveryPort are the IPv4 WS-Discovery multicast group
	// and port (OASIS WS-Discovery).
	discoveryGroupV4 = "239.255.255.250"
	discoveryPort    = 3702

	maxDatagram = 65535

	// httpShutdownTimeout bounds the metadata server's graceful shutdown.
	httpShutdownTimeout = 3 * time.Second
)

// SidecarName is the auxsvc.Group key used for the WS-Discovery responder.
const SidecarName = "wsd"

var discoveryUDPAddrV4 = &net.UDPAddr{IP: net.ParseIP(discoveryGroupV4), Port: discoveryPort}

// Responder is a WS-Discovery host: a UDP multicast responder (Hello/Bye/
// Probe/Resolve) plus the HTTP metadata endpoint Windows fetches to render the
// host as a Computer. One Responder advertises one host; the SMB adapter owns a
// single instance via its auxsvc sidecar.
type Responder struct {
	name       string // computer / friendly name
	workgroup  string // NetBIOS domain or workgroup label
	isDomain   bool   // true => label as Domain: (AD member), false => Workgroup:
	instanceID uint64 // AppSequence InstanceId

	mu       sync.Mutex
	udpConn  *net.UDPConn
	httpSrv  *http.Server
	endpoint Endpoint
	loopCtx  context.Context
	loopStop context.CancelFunc
	wg       sync.WaitGroup
	msgNum   atomic.Uint64
}

// NewResponder builds a WS-Discovery responder advertising the given computer
// name. workgroup is the NetBIOS domain (AD member) or workgroup (standalone)
// name; isDomain selects how Windows labels it in the pub:Computer relationship
// (Domain: vs Workgroup:). instanceID is the AppSequence InstanceId — it should
// be stable within a process run and increase across restarts (e.g. the process
// start time in unix seconds).
func NewResponder(name, workgroup string, isDomain bool, instanceID uint64) *Responder {
	if workgroup == "" {
		workgroup = "WORKGROUP"
		isDomain = false
	}
	return &Responder{name: name, workgroup: workgroup, isDomain: isDomain, instanceID: instanceID}
}

// Name implements the adapter auxsvc.Service interface.
func (r *Responder) Name() string { return SidecarName }

// Start binds the UDP multicast socket and the HTTP metadata server, then emits
// a Hello. A bind failure on either returns an error; the caller (the SMB
// adapter) treats it as non-fatal, matching the portmapper precedent.
func (r *Responder) Start(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.udpConn != nil {
		return nil // already started
	}

	host := hostinfo.PrimaryIPv4()
	if host == nil {
		return errors.New("wsd: no routable IPv4 address to advertise")
	}
	uuidURN := EndpointUUID()
	r.endpoint = Endpoint{
		UUID:            uuidURN,
		Types:           TypesComputer,
		XAddrs:          fmt.Sprintf("http://%s:%d/%s/", host, MetadataPort, strings.TrimPrefix(uuidURN, "urn:uuid:")),
		MetadataVersion: 1,
		InstanceID:      r.instanceID,
	}

	// HTTP metadata endpoint (bind first so a failure doesn't leave the UDP
	// socket dangling).
	mb := metadataBuilder{uuid: uuidURN, name: r.name, workgroup: r.workgroup, isDomain: r.isDomain}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", MetadataPort))
	if err != nil {
		return fmt.Errorf("wsd: listen tcp :%d: %w", MetadataPort, err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", mb.metadataHandler())
	r.httpSrv = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	// UDP multicast responder.
	conn, err := net.ListenMulticastUDP("udp4", nil, discoveryUDPAddrV4)
	if err != nil {
		_ = ln.Close()
		r.httpSrv = nil
		return fmt.Errorf("wsd: listen udp %s:%d: %w", discoveryGroupV4, discoveryPort, err)
	}
	r.udpConn = conn
	r.loopCtx, r.loopStop = context.WithCancel(context.Background())
	loopCtx := r.loopCtx

	r.wg.Add(2)
	go func() {
		defer r.wg.Done()
		if serr := r.httpSrv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			logger.Debug("wsd: metadata server stopped", "error", serr)
		}
	}()
	go func() {
		defer r.wg.Done()
		r.readLoop(conn, loopCtx)
	}()

	// Announce presence. Write directly to the local conn: we still hold r.mu
	// here and r.send would re-acquire it (a self-deadlock that would also wedge
	// the caller's auxsvc.Group, since Group.Start holds its own lock across this).
	if _, err := conn.WriteToUDP(Hello(r.endpoint, r.msgNum.Add(1)), discoveryUDPAddrV4); err != nil {
		logger.Debug("wsd: failed to send Hello", "error", err)
	}

	logger.Info("WS-Discovery responder listening",
		"udp", fmt.Sprintf("%s:%d", discoveryGroupV4, discoveryPort),
		"metadata_tcp", MetadataPort, "name", r.name)
	return nil
}

// Stop emits a Bye, then shuts down the HTTP server and UDP socket and waits for
// the goroutines to exit. Idempotent.
func (r *Responder) Stop(ctx context.Context) error {
	r.mu.Lock()
	conn := r.udpConn
	httpSrv := r.httpSrv
	stop := r.loopStop
	endpoint := r.endpoint
	r.udpConn = nil
	r.httpSrv = nil
	r.loopStop = nil
	r.loopCtx = nil
	r.mu.Unlock()

	if conn == nil {
		return nil
	}

	// Goodbye while the socket is still open.
	if _, err := conn.WriteToUDP(Bye(endpoint, r.msgNum.Add(1)), discoveryUDPAddrV4); err != nil {
		logger.Debug("wsd: failed to send Bye", "error", err)
	}

	if stop != nil {
		stop()
	}
	if httpSrv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), httpShutdownTimeout)
		_ = httpSrv.Shutdown(shutCtx)
		cancel()
	}
	_ = conn.Close()
	r.wg.Wait()
	logger.Info("WS-Discovery responder stopped")
	return nil
}

func (r *Responder) readLoop(conn *net.UDPConn, loopCtx context.Context) {
	buf := make([]byte, maxDatagram)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-loopCtx.Done():
				return
			default:
			}
			logger.Debug("wsd: read error", "error", err)
			continue
		}
		datagram := make([]byte, n)
		copy(datagram, buf[:n])
		r.handleDatagram(datagram, src)
	}
}

func (r *Responder) handleDatagram(data []byte, src *net.UDPAddr) {
	in, err := parseInbound(data)
	if err != nil {
		return // not a well-formed SOAP message we care about
	}
	switch in.kind {
	case kindProbe:
		if probeMatchesTypes(in.types) {
			r.send(ProbeMatch(r.endpoint, in.messageID, r.msgNum.Add(1)), src)
		}
	case kindResolve:
		// Only answer a Resolve targeting our endpoint.
		if in.address == "" || in.address == r.endpoint.UUID {
			r.send(ResolveMatch(r.endpoint, in.messageID, r.msgNum.Add(1)), src)
		}
	}
}

// send writes msg to dst, or to the multicast group when dst is nil.
func (r *Responder) send(msg []byte, dst *net.UDPAddr) {
	r.mu.Lock()
	conn := r.udpConn
	r.mu.Unlock()
	if conn == nil {
		return
	}
	target := dst
	if target == nil {
		target = discoveryUDPAddrV4
	}
	if _, err := conn.WriteToUDP(msg, target); err != nil {
		logger.Debug("wsd: send failed", "error", err)
	}
}
