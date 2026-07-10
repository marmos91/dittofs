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

	"golang.org/x/net/ipv4"

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

	// helloInterval is how often the responder re-announces its presence so
	// clients that came up after the initial Hello still learn about it.
	helloInterval = 30 * time.Second
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
	pconn    *ipv4.PacketConn // wraps udpConn for per-interface group join + multicast send
	ifaces   []net.Interface  // multicast interfaces the group is joined on
	sendMu   sync.Mutex       // serializes SetMulticastInterface + WriteTo
	httpSrv  *http.Server
	endpoint Endpoint
	hostIPs  []net.IP // advertised host IPv4s (for per-client XAddrs ordering)
	uuidBare string   // endpoint UUID without the urn:uuid: prefix (for XAddrs)
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
//
// ctx bounds the responder's lifetime: if it is cancelled (the owning adapter's
// Serve context ends) without an explicit Stop, the responder tears itself down,
// matching the ctx-driven NFS auxiliary services.
func (r *Responder) Start(ctx context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.udpConn != nil {
		return nil // already started
	}

	uuidURN := EndpointUUID()
	r.uuidBare = strings.TrimPrefix(uuidURN, "urn:uuid:")

	// Collect the host IPv4s. XAddrs is a space-separated list of metadata URLs
	// (one per IP) so a client on any of a multi-homed host's subnets can reach
	// the endpoint; per Probe/Resolve the list is reordered so the address that
	// routes to that client comes first (see endpointFor).
	r.hostIPs = nil
	for _, ip := range hostinfo.AllHostIPs() {
		if v4 := ip.To4(); v4 != nil {
			r.hostIPs = append(r.hostIPs, v4)
		}
	}
	if len(r.hostIPs) == 0 {
		return errors.New("wsd: no routable IPv4 address to advertise")
	}
	r.endpoint = Endpoint{
		UUID:            uuidURN,
		Types:           TypesComputer,
		XAddrs:          buildXAddrs(r.uuidBare, r.hostIPs),
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
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	r.httpSrv = srv

	// UDP multicast responder.
	conn, err := net.ListenMulticastUDP("udp4", nil, discoveryUDPAddrV4)
	if err != nil {
		_ = ln.Close()
		r.httpSrv = nil
		return fmt.Errorf("wsd: listen udp %s:%d: %w", discoveryGroupV4, discoveryPort, err)
	}
	r.udpConn = conn

	// Join the group on every multicast interface so probes arriving on any of
	// them are received — critical on multi-homed hosts where the OS default
	// multicast interface may not be the LAN the Windows clients are on. Unicast
	// ProbeMatch/ResolveMatch replies route back normally via WriteToUDP(src).
	r.pconn = ipv4.NewPacketConn(conn)
	_ = r.pconn.SetMulticastLoopback(true)
	r.ifaces = hostinfo.MulticastInterfaces()
	for i := range r.ifaces {
		_ = r.pconn.JoinGroup(&r.ifaces[i], discoveryUDPAddrV4)
	}

	r.loopCtx, r.loopStop = context.WithCancel(context.Background())
	loopCtx := r.loopCtx

	// Capture srv/conn/loopCtx as locals for the goroutines — Stop nils the
	// struct fields under r.mu, so the goroutines must not read them unlocked.
	r.wg.Add(3)
	go func() {
		defer r.wg.Done()
		if serr := srv.Serve(ln); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			logger.Debug("wsd: metadata server stopped", "error", serr)
		}
	}()
	go func() {
		defer r.wg.Done()
		r.readLoop(conn, loopCtx)
	}()
	// Periodic Hello: the spec only requires Hello on join, but a client whose
	// Function Discovery starts after us (or that misses the one-shot Hello) then
	// never learns about us unless it actively probes. Re-announcing keeps such
	// clients' caches populated. Bounded by loopCtx.
	go func() {
		defer r.wg.Done()
		r.periodicHello(loopCtx)
	}()

	// Tear down if the base context is cancelled without an explicit Stop. This
	// watcher is intentionally NOT in r.wg (it blocks until ctx is cancelled, so
	// Stop's wg.Wait must not wait on it); Stop is idempotent.
	go func() {
		<-ctx.Done()
		_ = r.Stop(context.Background())
	}()

	// Announce presence out every interface. Uses the local pconn/ifaces (not
	// r.send, which would re-acquire r.mu that we still hold — a self-deadlock).
	multicastAll(r.pconn, conn, r.ifaces, &r.sendMu, Hello(r.endpoint, r.msgNum.Add(1)))

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
	pconn := r.pconn
	ifaces := r.ifaces
	httpSrv := r.httpSrv
	stop := r.loopStop
	endpoint := r.endpoint
	r.udpConn = nil
	r.pconn = nil
	r.ifaces = nil
	r.httpSrv = nil
	r.loopStop = nil
	r.loopCtx = nil
	r.mu.Unlock()

	if conn == nil {
		return nil
	}

	// Goodbye out every interface while the socket is still open.
	multicastAll(pconn, conn, ifaces, &r.sendMu, Bye(endpoint, r.msgNum.Add(1)))

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
			r.send(ProbeMatch(r.endpointFor(src), in.messageID, r.msgNum.Add(1)), src)
		}
	case kindResolve:
		// Only answer a Resolve targeting our endpoint.
		if in.address == "" || in.address == r.endpoint.UUID {
			r.send(ResolveMatch(r.endpointFor(src), in.messageID, r.msgNum.Add(1)), src)
		}
	}
}

// endpointFor returns the advertised endpoint with its XAddrs reordered so the
// metadata URL on the interface that routes to src comes first — a client on a
// multi-homed host's other subnet would otherwise try an unreachable address
// first and give up before rendering the device.
func (r *Responder) endpointFor(src *net.UDPAddr) Endpoint {
	ep := r.endpoint
	if src == nil {
		return ep
	}
	if local := localIPForDest(src.IP); local != nil {
		ep.XAddrs = buildXAddrs(r.uuidBare, preferFirst(r.hostIPs, local))
	}
	return ep
}

// send writes msg unicast to dst (a ProbeMatch/ResolveMatch reply), or — when
// dst is nil — multicasts it out every joined interface.
func (r *Responder) send(msg []byte, dst *net.UDPAddr) {
	r.mu.Lock()
	conn := r.udpConn
	pconn := r.pconn
	ifaces := r.ifaces
	r.mu.Unlock()
	if conn == nil {
		return
	}
	if dst != nil { // unicast reply — routed normally
		if _, err := conn.WriteToUDP(msg, dst); err != nil {
			logger.Debug("wsd: send failed", "error", err)
		}
		return
	}
	multicastAll(pconn, conn, ifaces, &r.sendMu, msg)
}

// periodicHello re-multicasts a Hello every helloInterval until ctx is done.
func (r *Responder) periodicHello(ctx context.Context) {
	t := time.NewTicker(helloInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.send(Hello(r.endpoint, r.msgNum.Add(1)), nil)
		}
	}
}

// buildXAddrs renders the space-separated list of metadata URLs, one per IP.
func buildXAddrs(uuidBare string, ips []net.IP) string {
	urls := make([]string, 0, len(ips))
	for _, ip := range ips {
		urls = append(urls, fmt.Sprintf("http://%s:%d/%s/", ip, MetadataPort, uuidBare))
	}
	return strings.Join(urls, " ")
}

// localIPForDest returns the local source IP the OS would use to reach dst (no
// packet is sent — Dial on a UDP socket just resolves the route). Returns nil on
// error.
func localIPForDest(dst net.IP) net.IP {
	c, err := net.Dial("udp4", net.JoinHostPort(dst.String(), "5357"))
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close() }()
	if la, ok := c.LocalAddr().(*net.UDPAddr); ok {
		return la.IP
	}
	return nil
}

// preferFirst returns ips reordered so first leads, preserving the relative
// order of the rest. If first is not one of ips (not an address we advertise),
// the order is left unchanged.
func preferFirst(ips []net.IP, first net.IP) []net.IP {
	found := false
	for _, ip := range ips {
		if ip.Equal(first) {
			found = true
			break
		}
	}
	if !found {
		return ips
	}
	out := make([]net.IP, 0, len(ips))
	out = append(out, first)
	for _, ip := range ips {
		if !ip.Equal(first) {
			out = append(out, ip)
		}
	}
	return out
}

// multicastAll sends msg to the WS-Discovery group out every interface, falling
// back to the default route when no interface list is available. Serialized via
// mu because SetMulticastInterface mutates shared socket state.
func multicastAll(pconn *ipv4.PacketConn, conn *net.UDPConn, ifaces []net.Interface, mu *sync.Mutex, msg []byte) {
	if conn == nil {
		return
	}
	if pconn == nil || len(ifaces) == 0 {
		_, _ = conn.WriteToUDP(msg, discoveryUDPAddrV4)
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range ifaces {
		if err := pconn.SetMulticastInterface(&ifaces[i]); err != nil {
			continue
		}
		_, _ = pconn.WriteTo(msg, nil, discoveryUDPAddrV4)
	}
}
