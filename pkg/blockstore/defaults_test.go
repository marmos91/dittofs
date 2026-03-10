package blockstore

import (
	"strings"
	"testing"
)

type mockDetector struct {
	memory uint64
	cpus   int
}

func (m *mockDetector) AvailableMemory() uint64 { return m.memory }
func (m *mockDetector) AvailableCPUs() int      { return m.cpus }

func TestDeduceDefaults(t *testing.T) {
	tests := []struct {
		name           string
		memory         uint64
		cpus           int
		wantLocalStore uint64
		wantL1Cache    int64
		wantPending    uint64
		wantSyncs      int
		wantFetches    int
		wantPrefetch   int
	}{
		{
			name:           "normal machine 8GiB/8CPU",
			memory:         8 * gib,
			cpus:           8,
			wantLocalStore: 2 * gib, // 25% of 8GiB
			wantL1Cache:    1 * gib, // 12.5% of 8GiB
			wantPending:    1 * gib, // 50% of 2GiB
			wantSyncs:      8,       // max(4, 8)
			wantFetches:    16,      // max(8, 8*2)
			wantPrefetch:   DefaultPrefetchWorkers,
		},
		{
			name:           "small machine 512MiB/1CPU",
			memory:         512 * mib,
			cpus:           1,
			wantLocalStore: 256 * mib, // 25% of 512MiB = 128MiB, floor 256MiB
			wantL1Cache:    64 * mib,  // 12.5% of 512MiB = 64MiB, exactly at floor
			wantPending:    128 * mib, // 50% of 256MiB
			wantSyncs:      4,         // max(4, 1) = floor
			wantFetches:    8,         // max(8, 2) = floor
			wantPrefetch:   DefaultPrefetchWorkers,
		},
		{
			name:           "very small machine 256MiB/1CPU",
			memory:         256 * mib,
			cpus:           1,
			wantLocalStore: 256 * mib, // 25% of 256MiB = 64MiB, floor 256MiB
			wantL1Cache:    64 * mib,  // 12.5% of 256MiB = 32MiB, floor 64MiB
			wantPending:    128 * mib, // 50% of 256MiB
			wantSyncs:      4,         // floor
			wantFetches:    8,         // floor
			wantPrefetch:   DefaultPrefetchWorkers,
		},
		{
			name:           "large machine 256GiB/64CPU",
			memory:         256 * gib,
			cpus:           64,
			wantLocalStore: 64 * gib, // 25% of 256GiB
			wantL1Cache:    32 * gib, // 12.5% of 256GiB
			wantPending:    32 * gib, // 50% of 64GiB
			wantSyncs:      64,       // max(4, 64)
			wantFetches:    128,      // max(8, 128)
			wantPrefetch:   DefaultPrefetchWorkers,
		},
		{
			name:           "medium machine 4GiB/4CPU",
			memory:         4 * gib,
			cpus:           4,
			wantLocalStore: 1 * gib,   // 25% of 4GiB
			wantL1Cache:    512 * mib, // 12.5% of 4GiB
			wantPending:    512 * mib, // 50% of 1GiB
			wantSyncs:      4,         // max(4, 4)
			wantFetches:    8,         // max(8, 8)
			wantPrefetch:   DefaultPrefetchWorkers,
		},
		{
			name:           "many CPUs low memory",
			memory:         2 * gib,
			cpus:           32,
			wantLocalStore: 512 * mib, // 25% of 2GiB
			wantL1Cache:    256 * mib, // 12.5% of 2GiB
			wantPending:    256 * mib, // 50% of 512MiB
			wantSyncs:      32,        // max(4, 32)
			wantFetches:    64,        // max(8, 64)
			wantPrefetch:   DefaultPrefetchWorkers,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &mockDetector{memory: tt.memory, cpus: tt.cpus}
			got := DeduceDefaults(d)

			if got.LocalStoreSize != tt.wantLocalStore {
				t.Errorf("LocalStoreSize = %d, want %d", got.LocalStoreSize, tt.wantLocalStore)
			}
			if got.L1CacheSize != tt.wantL1Cache {
				t.Errorf("L1CacheSize = %d, want %d", got.L1CacheSize, tt.wantL1Cache)
			}
			if got.MaxPendingSize != tt.wantPending {
				t.Errorf("MaxPendingSize = %d, want %d", got.MaxPendingSize, tt.wantPending)
			}
			if got.ParallelSyncs != tt.wantSyncs {
				t.Errorf("ParallelSyncs = %d, want %d", got.ParallelSyncs, tt.wantSyncs)
			}
			if got.ParallelFetches != tt.wantFetches {
				t.Errorf("ParallelFetches = %d, want %d", got.ParallelFetches, tt.wantFetches)
			}
			if got.PrefetchWorkers != tt.wantPrefetch {
				t.Errorf("PrefetchWorkers = %d, want %d", got.PrefetchWorkers, tt.wantPrefetch)
			}
		})
	}
}

func TestDeduceDefaults_PrefetchWorkersFixed(t *testing.T) {
	// PrefetchWorkers should always be DefaultPrefetchWorkers regardless of CPU count.
	for _, cpus := range []int{1, 2, 4, 8, 16, 32, 64, 128} {
		d := &mockDetector{memory: 8 * gib, cpus: cpus}
		got := DeduceDefaults(d)
		if got.PrefetchWorkers != DefaultPrefetchWorkers {
			t.Errorf("cpus=%d: PrefetchWorkers = %d, want %d",
				cpus, got.PrefetchWorkers, DefaultPrefetchWorkers)
		}
	}
}

func TestDeduceDefaults_String(t *testing.T) {
	d := &mockDetector{memory: 8 * gib, cpus: 8}
	got := DeduceDefaults(d)
	s := got.String()
	if s == "" {
		t.Fatal("String() returned empty")
	}
	t.Logf("String() = %s", s)

	for _, want := range []string{"LocalStoreSize", "L1CacheSize", "ParallelSyncs", "ParallelFetches", "MaxPendingSize"} {
		if !strings.Contains(s, want) {
			t.Errorf("String() missing %q: %s", want, s)
		}
	}
}
