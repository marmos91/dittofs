package orchestrator

import "testing"

func TestLatencyFromSamples_Empty(t *testing.T) {
	if got := LatencyFromSamples(nil); got != nil {
		t.Fatalf("empty samples must yield nil latency, got %+v", got)
	}
	if got := LatencyFromSamples([]int64{}); got != nil {
		t.Fatalf("empty slice must yield nil latency, got %+v", got)
	}
}

func TestLatencyFromSamples_Known(t *testing.T) {
	// 1..100 ns. Nearest-rank ranks: p50→ceil(0.5*100)=50→sorted[49]=50,
	// p95→95→95, p99→99→99.
	samples := make([]int64, 100)
	for i := range samples {
		samples[i] = int64(i + 1)
	}
	got := LatencyFromSamples(samples)
	if got == nil {
		t.Fatal("nil latency for 100 samples")
	}
	if got.P50Ns != 50 || got.P95Ns != 95 || got.P99Ns != 99 {
		t.Errorf("p50/p95/p99 = %d/%d/%d, want 50/95/99", got.P50Ns, got.P95Ns, got.P99Ns)
	}
}

func TestLatencyFromSamples_Unsorted(t *testing.T) {
	// Same multiset, shuffled — percentiles must be order-independent, and the
	// caller's slice must not be mutated.
	in := []int64{30, 10, 50, 20, 40}
	cp := append([]int64(nil), in...)
	got := LatencyFromSamples(in)
	// n=5: p50→ceil(0.5*5)=3→sorted[2]=30; p95→ceil(4.75)=5→50; p99→5→50.
	if got.P50Ns != 30 || got.P95Ns != 50 || got.P99Ns != 50 {
		t.Errorf("p50/p95/p99 = %d/%d/%d, want 30/50/50", got.P50Ns, got.P95Ns, got.P99Ns)
	}
	for i := range in {
		if in[i] != cp[i] {
			t.Fatalf("input slice was mutated at %d: %d != %d", i, in[i], cp[i])
		}
	}
}

func TestLatencyFromSamples_Single(t *testing.T) {
	got := LatencyFromSamples([]int64{42})
	if got.P50Ns != 42 || got.P95Ns != 42 || got.P99Ns != 42 {
		t.Errorf("single sample percentiles = %d/%d/%d, want all 42", got.P50Ns, got.P95Ns, got.P99Ns)
	}
}

func TestPercentile_RankClamping(t *testing.T) {
	sorted := []int64{1, 2, 3}
	// p1 over 3 samples: ceil(0.03)=1→sorted[0]=1 (clamped to rank 1).
	if got := percentile(sorted, 1); got != 1 {
		t.Errorf("p1 = %d, want 1", got)
	}
	// p100: rank=3→sorted[2]=3.
	if got := percentile(sorted, 100); got != 3 {
		t.Errorf("p100 = %d, want 3", got)
	}
}
