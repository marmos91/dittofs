package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv4"

	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/discovery/hostinfo"
)

const (
	// multicastGroupV4 / multicastPort are the IPv4 mDNS group and port
	// (RFC 6762).
	multicastGroupV4 = "224.0.0.251"
	multicastPort    = 5353

	// announceInterval is the gap between the two unsolicited announcements
	// sent on registration (RFC 6762 §8.3 recommends 1s).
	announceInterval = 1 * time.Second

	// announceRefresh re-sends the full record set periodically to keep the OS
	// mDNS cache warm. On macOS the system mDNSResponder owns :5353, so our
	// receive socket only sees a hashed subset of inbound queries and cannot be
	// relied on to answer live resolves; re-announcing below the SRV/A record TTL
	// (120s) lets Finder resolve and connect at any time, not just within TTL of
	// startup.
	announceRefresh = 60 * time.Second

	// maxDatagram bounds a single inbound mDNS datagram read.
	maxDatagram = 65535

	// multicastWriteTimeout caps a single per-interface multicast send. Some
	// interfaces (notably macOS awdl0) can block a multicast WriteTo forever;
	// without a deadline that stalls sends to the remaining interfaces.
	multicastWriteTimeout = 250 * time.Millisecond
)

// multicastUDPAddrV4 is the destination for outgoing announcements/responses.
var multicastUDPAddrV4 = &net.UDPAddr{IP: net.ParseIP(multicastGroupV4), Port: multicastPort}

// Responder is the shared, process-global mDNS engine. It owns the single
// 224.0.0.251:5353 socket and a table of registered service records. Each
// protocol adapter registers its own records through Register and drops them via
// the returned Handle; the socket and read loop exist only while at least one
// registration is active (refcounted through the services table).
//
// Keeping one responder for the whole process — rather than one per adapter —
// means a single socket, a single host record set, and cheap live toggles: an
// adapter enabling/disabling its mDNS advertising just adds/removes its rows
// without churning the socket (unless it was the last registration).
type Responder struct {
	mu        sync.Mutex
	conn      *net.UDPConn
	pconn     *ipv4.PacketConn // wraps conn for per-interface group join (receive only)
	sendConn  *net.UDPConn     // dedicated ephemeral-port socket for outbound packets
	sendPconn *ipv4.PacketConn // wraps sendConn for per-interface multicast send
	ifaces    []net.Interface  // multicast interfaces the group is joined on
	sendMu    sync.Mutex       // serializes SetMulticastInterface + WriteTo across interfaces
	loopCtx   context.Context
	loopStop  context.CancelFunc
	wg        *sync.WaitGroup // per-socket-generation; isolates Wait from a later generation's Add
	services  map[uint64][]ServiceRecord
	cancels   map[uint64]context.CancelFunc // stops each registration's re-announce loop
	nextID    uint64
}

var (
	sharedOnce sync.Once
	sharedResp *Responder
)

// Shared returns the process-global Responder, creating it on first use.
func Shared() *Responder {
	sharedOnce.Do(func() {
		sharedResp = &Responder{services: make(map[uint64][]ServiceRecord)}
	})
	return sharedResp
}

// Handle identifies one registration so it can be withdrawn.
type Handle struct {
	r  *Responder
	id uint64
}

// Register advertises the given services and returns a Handle to withdraw them.
// The socket and read loop start lazily on the first active registration. Two
// unsolicited announcements are sent ~1s apart in the background.
//
// Services with no explicit IPv4/IPv6 are filled from the host's interface
// addresses at registration time.
func (r *Responder) Register(services []ServiceRecord) (*Handle, error) {
	services = fillAddresses(services)

	r.mu.Lock()
	if r.conn == nil {
		if err := r.startLocked(); err != nil {
			r.mu.Unlock()
			return nil, err
		}
	}
	id := r.nextID
	r.nextID++
	r.services[id] = services
	// Per-registration context so unregister() can stop this registration's
	// re-announce loop without waiting for the whole socket to be torn down.
	regCtx, cancel := context.WithCancel(r.loopCtx)
	if r.cancels == nil {
		r.cancels = make(map[uint64]context.CancelFunc)
	}
	r.cancels[id] = cancel
	wg := r.wg
	wg.Add(1) // under r.mu so it cannot race a concurrent stop's Wait
	r.mu.Unlock()

	go func() {
		defer wg.Done()
		r.announce(regCtx, services)
	}()

	logger.Info("mDNS advertising registered", "services", len(services))
	return &Handle{r: r, id: id}, nil
}

// Unregister withdraws a registration, multicasting a goodbye (TTL 0) for its
// records and stopping the socket when it was the last one. Safe on a nil or
// already-withdrawn Handle.
func (h *Handle) Unregister() {
	if h == nil || h.r == nil {
		return
	}
	h.r.unregister(h.id)
}

func (r *Responder) unregister(id uint64) {
	r.mu.Lock()
	services, ok := r.services[id]
	if ok {
		delete(r.services, id)
	}
	cancel := r.cancels[id]
	delete(r.cancels, id)
	last := ok && len(r.services) == 0
	r.mu.Unlock()

	if !ok {
		return
	}

	// Stop this registration's re-announce loop before the goodbye so it cannot
	// re-multicast the withdrawn records afterwards.
	if cancel != nil {
		cancel()
	}

	// Goodbye so caches evict promptly.
	if msg, err := announcement(services, true); err == nil {
		r.send(msg, nil)
	}
	if last {
		r.stop()
	}
}

// startLocked opens the multicast socket and launches the read loop. Caller
// holds r.mu.
func (r *Responder) startLocked() error {
	// Bind :5353 with SO_REUSEADDR *and* SO_REUSEPORT so we coexist with a host
	// mDNS responder. SO_REUSEADDR alone suffices on Linux (Avahi), but macOS's
	// system mDNSResponder holds the port with SO_REUSEPORT — without it here the
	// bind succeeds yet the kernel delivers no inbound multicast to us, so Finder
	// queries never reach the responder.
	lc := net.ListenConfig{Control: reusePort}
	pc, err := lc.ListenPacket(context.Background(), "udp4", fmt.Sprintf("%s:%d", multicastGroupV4, multicastPort))
	if err != nil {
		return fmt.Errorf("mdns: listen :%d: %w", multicastPort, err)
	}
	conn := pc.(*net.UDPConn)
	r.conn = conn

	// Join the group on every multicast interface so the responder receives
	// queries arriving on any of them — critical on multi-homed hosts, where the
	// OS default multicast interface may not be the LAN the clients are on.
	// Unicast replies route back normally via WriteToUDP(src).
	r.pconn = ipv4.NewPacketConn(conn)
	_ = r.pconn.SetMulticastLoopback(true)
	r.ifaces = hostinfo.MulticastInterfaces()
	joined := 0
	for i := range r.ifaces {
		if err := r.pconn.JoinGroup(&r.ifaces[i], multicastUDPAddrV4); err == nil {
			joined++
		}
	}
	// ListenConfig does not auto-join a default interface the way
	// ListenMulticastUDP did, so if no per-interface join landed, fall back to
	// the system default interface.
	if joined == 0 {
		if err := r.pconn.JoinGroup(nil, multicastUDPAddrV4); err == nil {
			joined++
		}
	}

	// Send from a *separate* ephemeral-port socket, never the :5353 receive
	// socket. On macOS a multicast datagram sent from a socket bound to :5353 is
	// looped straight back to that same socket instead of the system
	// mDNSResponder, so our announcements and replies never reach Finder. An
	// ephemeral source port sidesteps the loopback capture and unicast replies to
	// the querier's :5353 land on the OS responder rather than being reclaimed by
	// our own receive socket.
	sc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		_ = conn.Close()
		r.conn, r.pconn, r.ifaces = nil, nil, nil
		return fmt.Errorf("mdns: send socket: %w", err)
	}
	r.sendConn = sc
	r.sendPconn = ipv4.NewPacketConn(sc)
	_ = r.sendPconn.SetMulticastLoopback(true)

	r.loopCtx, r.loopStop = context.WithCancel(context.Background())
	loopCtx := r.loopCtx
	wg := &sync.WaitGroup{}
	r.wg = wg

	wg.Add(1)
	go func() {
		defer wg.Done()
		r.readLoop(conn, loopCtx)
	}()
	logger.Info("mDNS responder listening", "group", multicastGroupV4, "port", multicastPort, "interfaces", joined)
	return nil
}

// stop cancels the read loop, closes the socket, and waits for this socket
// generation's goroutines to exit.
func (r *Responder) stop() {
	r.mu.Lock()
	// A Register may have raced in between the caller deciding this was the last
	// registration and acquiring the lock here; if so, keep the socket up so its
	// records stay advertised rather than tearing down under it.
	if len(r.services) != 0 {
		r.mu.Unlock()
		return
	}
	conn := r.conn
	sendConn := r.sendConn
	stop := r.loopStop
	wg := r.wg
	r.conn = nil
	r.pconn = nil
	r.sendConn = nil
	r.sendPconn = nil
	r.ifaces = nil
	r.loopStop = nil
	r.loopCtx = nil
	r.wg = nil
	r.mu.Unlock()

	if stop != nil {
		stop()
	}
	if conn != nil {
		_ = conn.Close()
	}
	if sendConn != nil {
		_ = sendConn.Close()
	}
	if wg != nil {
		wg.Wait()
	}
	logger.Info("mDNS responder stopped")
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
			logger.Debug("mdns: read error", "error", err)
			continue
		}
		query := make([]byte, n)
		copy(query, buf[:n])
		r.handlePacket(query, src)
	}
}

func (r *Responder) handlePacket(query []byte, src *net.UDPAddr) {
	// Cheap pre-filter: the multicast group carries mostly responses (other
	// hosts' and our own announcements). Drop those on a header-only parse before
	// snapshotting the record set.
	if !isQuery(query) {
		return
	}
	r.mu.Lock()
	services := r.snapshotServicesLocked()
	r.mu.Unlock()
	if len(services) == 0 {
		return
	}

	resp, unicast, ok, err := buildResponse(services, query)
	if err != nil {
		logger.Debug("mdns: build response failed", "error", err)
		return
	}
	if !ok {
		return
	}
	if unicast {
		r.send(resp, src)
	} else {
		r.send(resp, nil)
	}
}

// announce sends the two startup announcements, then re-announces periodically
// for the lifetime of the registration to keep the OS mDNS cache warm. It bails
// as soon as the responder's context is cancelled.
func (r *Responder) announce(ctx context.Context, services []ServiceRecord) {
	msg, err := announcement(services, false)
	if err != nil {
		logger.Warn("mdns: failed to build announcement", "error", err)
		return
	}
	for i := 0; i < 2; i++ {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(announceInterval):
			}
		}
		r.send(msg, nil)
	}

	ticker := time.NewTicker(announceRefresh)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// ctx may have been cancelled while this tick was pending; don't
			// re-announce a registration that unregister() is withdrawing.
			if ctx.Err() != nil {
				return
			}
			r.send(msg, nil)
		}
	}
}

// send writes msg unicast to dst, or — when dst is nil — multicasts it out every
// joined interface so announcements/responses reach clients on all of them.
func (r *Responder) send(msg []byte, dst *net.UDPAddr) {
	r.mu.Lock()
	conn := r.sendConn
	pconn := r.sendPconn
	ifaces := r.ifaces
	r.mu.Unlock()
	if conn == nil {
		return
	}
	// Serialize every write on the shared send socket. The multicast loop below
	// mutates SetMulticastInterface and the per-write deadline; without this a
	// concurrent unicast reply could inherit that deadline and be dropped.
	r.sendMu.Lock()
	defer r.sendMu.Unlock()
	if dst != nil { // unicast reply — routed normally
		if _, err := conn.WriteToUDP(msg, dst); err != nil {
			logger.Debug("mdns: send failed", "error", err)
		}
		return
	}
	// Multicast: send out each interface.
	if pconn == nil || len(ifaces) == 0 {
		if _, err := conn.WriteToUDP(msg, multicastUDPAddrV4); err != nil {
			logger.Debug("mdns: send failed", "error", err)
		}
		return
	}
	for i := range ifaces {
		if err := pconn.SetMulticastInterface(&ifaces[i]); err != nil {
			continue
		}
		// Bound each send: a multicast WriteTo on some interfaces (notably macOS
		// awdl0) can block indefinitely, which would stall the remaining sends —
		// including the LAN interface where the OS mDNS responder listens, so the
		// announcement never reaches Finder.
		_ = conn.SetWriteDeadline(time.Now().Add(multicastWriteTimeout))
		if _, err := pconn.WriteTo(msg, nil, multicastUDPAddrV4); err != nil {
			logger.Debug("mdns: multicast send failed", "iface", ifaces[i].Name, "error", err)
		}
	}
	_ = conn.SetWriteDeadline(time.Time{})
}

// snapshotServicesLocked flattens every registration's records into one slice.
// Caller holds r.mu.
func (r *Responder) snapshotServicesLocked() []ServiceRecord {
	var out []ServiceRecord
	for _, svcs := range r.services {
		out = append(out, svcs...)
	}
	return out
}

// fillAddresses returns copies of services, filling any with no explicit
// address from the host's interface addresses.
func fillAddresses(services []ServiceRecord) []ServiceRecord {
	var hostIPs []net.IP
	out := make([]ServiceRecord, len(services))
	for i, s := range services {
		if len(s.IPv4) == 0 && len(s.IPv6) == 0 {
			if hostIPs == nil {
				hostIPs = hostinfo.AllHostIPs()
			}
			for _, ip := range hostIPs {
				if ip.To4() != nil {
					s.IPv4 = append(s.IPv4, ip)
				} else {
					s.IPv6 = append(s.IPv6, ip)
				}
			}
		}
		out[i] = s
	}
	return out
}
