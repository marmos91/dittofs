package portmap

import (
	"sync"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/nfs/portmap/xdr"
)

// ============================================================================
// Set Tests
// ============================================================================

func TestSet(t *testing.T) {
	r := NewRegistry()

	ok := r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})
	if !ok {
		t.Fatal("Set should return true for valid mapping")
	}

	port := r.Getport(100003, 3, 6)
	if port != 12049 {
		t.Errorf("Getport = %d, want 12049", port)
	}
}

func TestSetOverwrite(t *testing.T) {
	r := NewRegistry()

	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 2049})
	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})

	port := r.Getport(100003, 3, 6)
	if port != 12049 {
		t.Errorf("Getport after overwrite = %d, want 12049", port)
	}

	// Should still be 1 mapping, not 2
	if r.Count() != 1 {
		t.Errorf("Count = %d after overwrite, want 1", r.Count())
	}
}

func TestSetZeroPort(t *testing.T) {
	r := NewRegistry()

	ok := r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 0})
	if ok {
		t.Fatal("Set should return false for port 0")
	}

	if r.Count() != 0 {
		t.Errorf("Count = %d after rejected set, want 0", r.Count())
	}
}

// ============================================================================
// Unset Tests
// ============================================================================

func TestUnset(t *testing.T) {
	r := NewRegistry()

	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})

	ok := r.Unset(100003, 3, 6)
	if !ok {
		t.Fatal("Unset should return true for existing mapping")
	}

	port := r.Getport(100003, 3, 6)
	if port != 0 {
		t.Errorf("Getport after unset = %d, want 0", port)
	}
}

func TestUnsetNonExistent(t *testing.T) {
	r := NewRegistry()

	ok := r.Unset(99999, 1, 6)
	if ok {
		t.Fatal("Unset should return false for non-existent mapping")
	}
}

// ============================================================================
// Getport Tests
// ============================================================================

func TestGetportNotFound(t *testing.T) {
	r := NewRegistry()

	port := r.Getport(100003, 3, 6)
	if port != 0 {
		t.Errorf("Getport for unregistered service = %d, want 0", port)
	}
}

func TestGetportDifferentProtocols(t *testing.T) {
	r := NewRegistry()

	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})  // TCP
	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 17, Port: 12050}) // UDP

	tcpPort := r.Getport(100003, 3, 6)
	udpPort := r.Getport(100003, 3, 17)

	if tcpPort != 12049 {
		t.Errorf("TCP port = %d, want 12049", tcpPort)
	}
	if udpPort != 12050 {
		t.Errorf("UDP port = %d, want 12050", udpPort)
	}
}

// ============================================================================
// Dump Tests
// ============================================================================

func TestDump(t *testing.T) {
	r := NewRegistry()

	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})
	r.Set(&xdr.Mapping{Prog: 100005, Vers: 3, Prot: 6, Port: 12049})
	r.Set(&xdr.Mapping{Prog: 100021, Vers: 4, Prot: 17, Port: 12049})

	mappings := r.Dump()
	if len(mappings) != 3 {
		t.Fatalf("Dump returned %d mappings, want 3", len(mappings))
	}

	// Should be sorted by prog, then vers, then prot
	expected := []struct {
		prog, vers, prot, port uint32
	}{
		{100003, 3, 6, 12049},
		{100005, 3, 6, 12049},
		{100021, 4, 17, 12049},
	}

	for i, exp := range expected {
		m := mappings[i]
		if m.Prog != exp.prog || m.Vers != exp.vers || m.Prot != exp.prot || m.Port != exp.port {
			t.Errorf("Mapping[%d] = {%d, %d, %d, %d}, want {%d, %d, %d, %d}",
				i, m.Prog, m.Vers, m.Prot, m.Port,
				exp.prog, exp.vers, exp.prot, exp.port)
		}
	}
}

func TestDumpEmpty(t *testing.T) {
	r := NewRegistry()

	mappings := r.Dump()
	if mappings == nil {
		t.Fatal("Dump should return empty slice, not nil")
	}
	if len(mappings) != 0 {
		t.Errorf("Dump returned %d mappings, want 0", len(mappings))
	}
}

func TestDumpReturnsSnapshotCopies(t *testing.T) {
	r := NewRegistry()
	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})

	mappings := r.Dump()
	if len(mappings) != 1 {
		t.Fatalf("Dump returned %d mappings, want 1", len(mappings))
	}

	// Mutating the returned mapping should not affect the registry
	mappings[0].Port = 99999
	port := r.Getport(100003, 3, 6)
	if port != 12049 {
		t.Errorf("Registry was mutated via Dump result: port = %d, want 12049", port)
	}
}

// ============================================================================
// RegisterDittoFSServices Tests
// ============================================================================

func TestRegisterDittoFSServices(t *testing.T) {
	r := NewRegistry()
	r.RegisterDittoFSServices(12049)

	// Should have 7 mappings: 7 program/version pairs x TCP only
	// (DittoFS NFS adapter is TCP-only, no UDP transport)
	if r.Count() != 7 {
		t.Fatalf("RegisterDittoFSServices created %d mappings, want 7", r.Count())
	}

	// Verify each expected registration
	expectations := []struct {
		name string
		prog uint32
		vers uint32
	}{
		{"NFS v3", 100003, 3},
		{"NFS v4", 100003, 4},
		{"MOUNT v1", 100005, 1},
		{"MOUNT v2", 100005, 2},
		{"MOUNT v3", 100005, 3},
		{"NLM v4", 100021, 4},
		{"NSM v1", 100024, 1},
	}

	for _, exp := range expectations {
		tcpPort := r.Getport(exp.prog, exp.vers, 6)
		if tcpPort != 12049 {
			t.Errorf("%s TCP: port = %d, want 12049", exp.name, tcpPort)
		}

		// UDP should NOT be registered (NFS adapter is TCP-only)
		udpPort := r.Getport(exp.prog, exp.vers, 17)
		if udpPort != 0 {
			t.Errorf("%s UDP: port = %d, want 0 (should not be registered)", exp.name, udpPort)
		}
	}
}

func TestRegisterDittoFSServicesCustomPort(t *testing.T) {
	r := NewRegistry()
	r.RegisterDittoFSServices(2049)

	port := r.Getport(100003, 3, 6)
	if port != 2049 {
		t.Errorf("NFS v3 TCP port = %d, want 2049", port)
	}
}

// ============================================================================
// Clear Tests
// ============================================================================

func TestClear(t *testing.T) {
	r := NewRegistry()
	r.RegisterDittoFSServices(12049)

	if r.Count() == 0 {
		t.Fatal("Registry should not be empty before Clear")
	}

	r.Clear()

	if r.Count() != 0 {
		t.Errorf("Count after Clear = %d, want 0", r.Count())
	}

	mappings := r.Dump()
	if len(mappings) != 0 {
		t.Errorf("Dump after Clear returned %d mappings, want 0", len(mappings))
	}
}

// ============================================================================
// Count Tests
// ============================================================================

func TestCount(t *testing.T) {
	r := NewRegistry()

	if r.Count() != 0 {
		t.Errorf("Initial count = %d, want 0", r.Count())
	}

	r.Set(&xdr.Mapping{Prog: 100003, Vers: 3, Prot: 6, Port: 12049})
	if r.Count() != 1 {
		t.Errorf("Count after one set = %d, want 1", r.Count())
	}

	r.Set(&xdr.Mapping{Prog: 100005, Vers: 3, Prot: 6, Port: 12049})
	if r.Count() != 2 {
		t.Errorf("Count after two sets = %d, want 2", r.Count())
	}

	r.Unset(100003, 3, 6)
	if r.Count() != 1 {
		t.Errorf("Count after unset = %d, want 1", r.Count())
	}
}

// ============================================================================
// Concurrent Access Tests
// ============================================================================

func TestConcurrentAccess(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup

	// Concurrent writers
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			prog := uint32(100000 + n)
			for j := 0; j < 100; j++ {
				r.Set(&xdr.Mapping{Prog: prog, Vers: 1, Prot: 6, Port: uint32(1000 + j)})
			}
		}(i)
	}

	// Concurrent readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = r.Getport(uint32(100000+n), 1, 6)
				_ = r.Dump()
				_ = r.Count()
			}
		}(i)
	}

	// Concurrent unsets
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			prog := uint32(100000 + n)
			for j := 0; j < 50; j++ {
				r.Unset(prog, 1, 6)
				r.Set(&xdr.Mapping{Prog: prog, Vers: 1, Prot: 6, Port: uint32(2000 + j)})
			}
		}(i)
	}

	wg.Wait()

	// Basic sanity: should not have crashed, count should be reasonable
	count := r.Count()
	if count < 0 {
		t.Errorf("Count after concurrent access = %d, should be non-negative", count)
	}
}
