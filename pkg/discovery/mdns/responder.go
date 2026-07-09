package mdns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

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

	// maxDatagram bounds a single inbound mDNS datagram read.
	maxDatagram = 65535
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
	mu       sync.Mutex
	conn     *net.UDPConn
	loopCtx  context.Context
	loopStop context.CancelFunc
	wg       *sync.WaitGroup // per-socket-generation; isolates Wait from a later generation's Add
	services map[uint64][]ServiceRecord
	nextID   uint64
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
	ctx := r.loopCtx
	wg := r.wg
	wg.Add(1) // under r.mu so it cannot race a concurrent stop's Wait
	r.mu.Unlock()

	go func() {
		defer wg.Done()
		r.announce(ctx, services)
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
	last := ok && len(r.services) == 0
	r.mu.Unlock()

	if !ok {
		return
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
	// ListenMulticastUDP sets SO_REUSEADDR so we coexist with a host mDNS
	// responder (e.g. Avahi), and joins the group on the system's default
	// multicast interface. Multi-interface join is a documented follow-up.
	conn, err := net.ListenMulticastUDP("udp4", nil, multicastUDPAddrV4)
	if err != nil {
		return fmt.Errorf("mdns: listen %s:%d: %w", multicastGroupV4, multicastPort, err)
	}
	r.conn = conn
	r.loopCtx, r.loopStop = context.WithCancel(context.Background())
	wg := &sync.WaitGroup{}
	r.wg = wg

	wg.Add(1)
	go func() {
		defer wg.Done()
		r.readLoop(conn)
	}()
	logger.Info("mDNS responder listening", "group", multicastGroupV4, "port", multicastPort)
	return nil
}

// stop cancels the read loop, closes the socket, and waits for this socket
// generation's goroutines to exit.
func (r *Responder) stop() {
	r.mu.Lock()
	conn := r.conn
	stop := r.loopStop
	wg := r.wg
	r.conn = nil
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
	if wg != nil {
		wg.Wait()
	}
	logger.Info("mDNS responder stopped")
}

func (r *Responder) readLoop(conn *net.UDPConn) {
	buf := make([]byte, maxDatagram)
	for {
		n, src, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-r.loopCtx.Done():
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

// announce sends the two startup announcements, bailing if the socket is torn
// down between them.
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
}

// send writes msg to dst, or to the multicast group when dst is nil.
func (r *Responder) send(msg []byte, dst *net.UDPAddr) {
	r.mu.Lock()
	conn := r.conn
	r.mu.Unlock()
	if conn == nil {
		return
	}
	target := dst
	if target == nil {
		target = multicastUDPAddrV4
	}
	if _, err := conn.WriteToUDP(msg, target); err != nil {
		logger.Debug("mdns: send failed", "error", err)
	}
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
