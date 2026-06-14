package orchestrator

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
)

// WorkloadParams is one manifest entry: a named workload plus the knobs the
// bench/blockstore primitives accept. It mirrors the subset of
// blockstore.Opts that a manifest needs to express — kept as a plain struct so
// this package has no dependency on bench/blockstore (the WorkloadRunner the
// caller injects owns that translation).
type WorkloadParams struct {
	// Name is the unique manifest key AND the result key. Required.
	Name string `json:"name"`
	// Workload is the bench/blockstore workload identifier (e.g.
	// "sequential-write", "mixed-ops-storm"). Required.
	Workload string `json:"workload"`

	Ops        int    `json:"ops"`
	BlockSize  int    `json:"block_size,omitempty"`
	WorkingSet int    `json:"working_set,omitempty"`
	Workers    int    `json:"workers,omitempty"`
	Seed       uint64 `json:"seed"`
	Remote     string `json:"remote,omitempty"`
	Mix        string `json:"mix,omitempty"`
}

// Manifest is an ordered set of workloads to run. Names must be unique; the
// document keys results by name.
type Manifest struct {
	Workloads []WorkloadParams `json:"workloads"`
}

// DefaultManifest is a small, fast, dependency-free set of in-process
// blockstore workloads suitable for a quick perf snapshot or a CI gate. It
// uses the memory remote so it needs no S3/network, and keeps block sizes
// modest so the whole set runs in a few seconds. For a heavy capture run pass
// --manifest with larger ops/block sizes (e.g. the 8 MiB sequential default
// that the standalone `blockstore` subcommand uses).
func DefaultManifest() Manifest {
	const smallBlock = 64 * 1024 // 64 KiB keeps the default set quick.
	return Manifest{
		Workloads: []WorkloadParams{
			{Name: "seq-write", Workload: "sequential-write", Ops: 2000, BlockSize: smallBlock, WorkingSet: 4, Seed: 1, Remote: "memory"},
			{Name: "random-write", Workload: "random-write", Ops: 5000, BlockSize: 4096, WorkingSet: 4, Seed: 1, Remote: "memory"},
			{Name: "dedup-heavy", Workload: "dedup-heavy", Ops: 2000, BlockSize: smallBlock, Seed: 1, Remote: "memory"},
			{Name: "mixed-rw", Workload: "mixed-rw", Ops: 5000, BlockSize: 4096, WorkingSet: 4, Seed: 1, Remote: "memory"},
			{Name: "storm-4w", Workload: "mixed-ops-storm", Ops: 4000, BlockSize: 4096, Workers: 4, Seed: 1, Remote: "memory"},
		},
	}
}

// LoadManifest decodes a JSON manifest from r and validates it.
func LoadManifest(r io.Reader) (Manifest, error) {
	var m Manifest
	if err := json.NewDecoder(r).Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Validate checks that the manifest is non-empty, every entry names a workload,
// op counts are positive, and names are unique.
func (m Manifest) Validate() error {
	if len(m.Workloads) == 0 {
		return fmt.Errorf("manifest has no workloads")
	}
	seen := map[string]struct{}{}
	for i, w := range m.Workloads {
		if w.Name == "" {
			return fmt.Errorf("workload[%d]: name is required", i)
		}
		if w.Workload == "" {
			return fmt.Errorf("workload %q: workload identifier is required", w.Name)
		}
		if w.Ops <= 0 {
			return fmt.Errorf("workload %q: ops must be > 0", w.Name)
		}
		if _, dup := seen[w.Name]; dup {
			return fmt.Errorf("duplicate workload name %q", w.Name)
		}
		seen[w.Name] = struct{}{}
	}
	return nil
}

// Names returns the manifest workload names in sorted order — handy for a
// stable human summary independent of run order.
func (m Manifest) Names() []string {
	out := make([]string, 0, len(m.Workloads))
	for _, w := range m.Workloads {
		out = append(out, w.Name)
	}
	slices.Sort(out)
	return out
}
