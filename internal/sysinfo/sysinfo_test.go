package sysinfo

import (
	"runtime"
	"testing"
)

func TestNewDetector_NonNil(t *testing.T) {
	d := NewDetector()
	if d == nil {
		t.Fatal("NewDetector() returned nil")
	}
}

func TestAvailableMemory_Positive(t *testing.T) {
	d := NewDetector()
	mem := d.AvailableMemory()
	if mem == 0 {
		t.Fatal("AvailableMemory() returned 0")
	}
	t.Logf("AvailableMemory() = %d bytes (%.2f GiB)", mem, float64(mem)/(1024*1024*1024))
}

func TestAvailableCPUs_Positive(t *testing.T) {
	d := NewDetector()
	cpus := d.AvailableCPUs()
	if cpus <= 0 {
		t.Fatalf("AvailableCPUs() returned %d, want > 0", cpus)
	}
}

func TestAvailableCPUs_MatchesGOMAXPROCS(t *testing.T) {
	d := NewDetector()
	cpus := d.AvailableCPUs()
	expected := runtime.GOMAXPROCS(0)
	if cpus != expected {
		t.Fatalf("AvailableCPUs() = %d, want %d (runtime.GOMAXPROCS(0))", cpus, expected)
	}
}

func TestMemorySource_NonEmpty(t *testing.T) {
	d := NewDetector()
	source := d.MemorySource()
	if source == "" {
		t.Fatal("MemorySource() returned empty string")
	}
	t.Logf("MemorySource() = %q", source)
}
