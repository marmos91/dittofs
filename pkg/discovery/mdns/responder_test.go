package mdns

import (
	"net"
	"testing"
)

func TestFillAddresses_KeepsExplicitIPs(t *testing.T) {
	in := []ServiceRecord{{
		Instance: "DITTOFS",
		Service:  "_smb._tcp",
		Port:     445,
		IPv4:     []net.IP{net.IPv4(10, 0, 0, 5)},
	}}
	out := fillAddresses(in)
	if len(out) != 1 || len(out[0].IPv4) != 1 || !out[0].IPv4[0].Equal(net.IPv4(10, 0, 0, 5)) {
		t.Fatalf("explicit IPs should be preserved, got %+v", out)
	}
}

func TestFillAddresses_FillsWhenEmpty(t *testing.T) {
	// Records with no address get the host's IPs (may be empty in a sandbox;
	// the important property is it does not panic and leaves explicit records
	// untouched).
	in := []ServiceRecord{{Instance: "DITTOFS", Service: "_nfs._tcp", Port: 12049}}
	out := fillAddresses(in)
	if len(out) != 1 {
		t.Fatalf("expected 1 record, got %d", len(out))
	}
	// Input slice must not be mutated (fillAddresses returns copies).
	if len(in[0].IPv4) != 0 {
		t.Fatal("fillAddresses mutated the input record")
	}
}

// TestRegisterUnregister_Lifecycle exercises the real socket path where the
// environment allows binding the mDNS group; it skips otherwise so it never
// flakes on a headless runner with no multicast interface.
func TestRegisterUnregister_Lifecycle(t *testing.T) {
	r := &Responder{services: make(map[uint64][]ServiceRecord)}

	h, err := r.Register([]ServiceRecord{{
		Instance: "DITTOFS",
		Service:  "_smb._tcp",
		Port:     445,
		IPv4:     []net.IP{net.IPv4(127, 0, 0, 1)},
	}})
	if err != nil {
		t.Skipf("cannot bind mDNS socket in this environment: %v", err)
	}

	r.mu.Lock()
	connUp := r.conn != nil
	r.mu.Unlock()
	if !connUp {
		t.Fatal("socket should be open after Register")
	}

	// Second registration reuses the same socket.
	h2, err := r.Register([]ServiceRecord{{
		Instance: "DITTOFS",
		Service:  "_nfs._tcp",
		Port:     12049,
		IPv4:     []net.IP{net.IPv4(127, 0, 0, 1)},
	}})
	if err != nil {
		t.Fatalf("second Register: %v", err)
	}

	// Dropping one registration keeps the socket (refcount > 0).
	h.Unregister()
	r.mu.Lock()
	stillUp := r.conn != nil
	r.mu.Unlock()
	if !stillUp {
		t.Fatal("socket should stay open while a registration remains")
	}

	// Dropping the last registration closes the socket.
	h2.Unregister()
	r.mu.Lock()
	down := r.conn == nil
	r.mu.Unlock()
	if !down {
		t.Fatal("socket should be closed after the last Unregister")
	}
}
