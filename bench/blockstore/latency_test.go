package blockstore

import (
	"sync"
	"testing"
	"time"
)

func TestLatencyRecorder_RecordsSamplesAndCounts(t *testing.T) {
	r := NewLatencyRecorder(4)
	r.Record(10*time.Nanosecond, true)
	r.Record(20*time.Nanosecond, true)
	r.Record(30*time.Nanosecond, false)

	samples := r.Samples()
	if len(samples) != 3 {
		t.Fatalf("samples = %d, want 3", len(samples))
	}
	if samples[0] != 10 || samples[2] != 30 {
		t.Errorf("samples not recorded in order: %v", samples)
	}
	ok, failed := r.Counts()
	if ok != 2 || failed != 1 {
		t.Errorf("counts = %d/%d, want 2/1", ok, failed)
	}
}

func TestLatencyRecorder_NilSafe(t *testing.T) {
	var r *LatencyRecorder
	r.Record(time.Second, true) // must not panic
	if s := r.Samples(); s != nil {
		t.Errorf("nil recorder samples = %v, want nil", s)
	}
	if ok, failed := r.Counts(); ok != 0 || failed != 0 {
		t.Errorf("nil recorder counts = %d/%d, want 0/0", ok, failed)
	}
}

func TestLatencyRecorder_Concurrent(t *testing.T) {
	r := NewLatencyRecorder(0)
	const goroutines, perG = 8, 100
	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				r.Record(time.Duration(i)*time.Nanosecond, i%2 == 0)
			}
		}()
	}
	wg.Wait()
	if got := len(r.Samples()); got != goroutines*perG {
		t.Errorf("concurrent samples = %d, want %d", got, goroutines*perG)
	}
	ok, failed := r.Counts()
	if ok+failed != goroutines*perG {
		t.Errorf("counts sum = %d, want %d", ok+failed, goroutines*perG)
	}
}

func TestLatencyRecorder_SamplesCopy(t *testing.T) {
	r := NewLatencyRecorder(2)
	r.Record(5*time.Nanosecond, true)
	s := r.Samples()
	s[0] = 999 // mutating the returned slice must not affect the recorder
	if again := r.Samples(); again[0] != 5 {
		t.Errorf("Samples() returned aliased slice: %v", again)
	}
}
