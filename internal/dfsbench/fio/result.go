package fio

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// CellResult is one benchmark cell: a single (system, workload, size, protocol,
// pass) measurement. Metric field names mirror the retired pkg/bench
// WorkloadResult so downstream tooling that already parsed those keys keeps
// working. S3Bytes is carried here but only populated once the S3 network meter
// lands (plan PR5) — it stays 0 in local/smoke runs.
type CellResult struct {
	System    string    `json:"system"`   // backend[-protocol], e.g. "local" or "dittofs-s3-nfs3"
	Workload  string    `json:"workload"` // fio job name, e.g. "seq-read"
	Size      string    `json:"size"`     // size class: small|medium|large (or explicit like "64k")
	Protocol  string    `json:"protocol"` // nfs3|nfs4|smb3|local
	Pass      string    `json:"pass"`     // warm|cold
	Timestamp time.Time `json:"timestamp"`

	ThroughputMBps float64 `json:"throughput_mbps,omitempty"`
	IOPS           float64 `json:"iops,omitempty"`
	OpsPerSec      float64 `json:"ops_per_sec,omitempty"`

	LatencyP50Us float64 `json:"latency_p50_us"`
	LatencyP95Us float64 `json:"latency_p95_us"`
	LatencyP99Us float64 `json:"latency_p99_us"`
	LatencyAvgUs float64 `json:"latency_avg_us"`

	TotalOps   int64   `json:"total_ops"`
	TotalBytes int64   `json:"total_bytes"`
	Errors     int64   `json:"errors"`
	DurationS  float64 `json:"duration_s"`

	S3Bytes int64 `json:"s3_bytes"` // server↔S3 bytes; populated in a later PR

	// Server-side resource use during the fio pass (system-wide, from /proc/stat).
	// CtxSwPerSec is the FUSE-tax indicator; 0 off Linux (local/smoke on macOS).
	CtxSwPerSec float64 `json:"ctxsw_per_sec,omitempty"`
	CPUPct      float64 `json:"cpu_pct,omitempty"`
}

// Slug is the stable, filesystem-safe identity of a cell. Two runs of the same
// cell produce the same slug, which is what makes --resume idempotent (skip if
// the result file already exists).
func (c CellResult) Slug() string {
	return fmt.Sprintf("%s__%s__%s__%s__%s", c.System, c.Workload, c.Size, c.Protocol, c.Pass)
}

func (c CellResult) filename() string { return c.Slug() + ".json" }

// Save writes the cell result JSON atomically into dir.
func (c CellResult) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	final := filepath.Join(dir, c.filename())
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// LoadResults reads every *.json cell result from dir (ignoring .tmp partials),
// used by `report` and by `--resume` skip checks.
func LoadResults(dir string) ([]CellResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []CellResult
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		var r CellResult
		if err := json.Unmarshal(data, &r); err != nil {
			return nil, fmt.Errorf("%s: %w", e.Name(), err)
		}
		out = append(out, r)
	}
	return out, nil
}

// ResultExists reports whether a completed result file for slug is on disk.
func ResultExists(dir, slug string) bool {
	_, err := os.Stat(filepath.Join(dir, slug+".json"))
	return err == nil
}
