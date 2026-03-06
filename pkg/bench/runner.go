package bench

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const benchDir = "_dfsctl_bench"

// Runner orchestrates benchmark workloads.
type Runner struct {
	cfg      Config
	progress ProgressFunc
}

// NewRunner creates a benchmark runner with the given config and optional
// progress callback.
func NewRunner(cfg Config, progress ProgressFunc) *Runner {
	return &Runner{cfg: cfg, progress: progress}
}

// Validate checks that the benchmark path exists and is writable.
func (r *Runner) Validate() error {
	info, err := os.Stat(r.cfg.Path)
	if err != nil {
		return fmt.Errorf("path %q: %w", r.cfg.Path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("path %q is not a directory", r.cfg.Path)
	}

	// Test writability by creating and removing a temp file.
	probe := filepath.Join(r.cfg.Path, ".dfsctl_bench_probe")
	f, err := os.Create(probe)
	if err != nil {
		return fmt.Errorf("path %q is not writable: %w", r.cfg.Path, err)
	}
	_ = f.Close()
	_ = os.Remove(probe)

	return nil
}

// Run executes the configured workloads and returns results.
func (r *Runner) Run(ctx context.Context) (*Result, error) {
	workloads := r.cfg.Workloads
	if len(workloads) == 0 {
		workloads = AllWorkloads()
	}

	dir := filepath.Join(r.cfg.Path, benchDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create bench dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()

	result := &Result{
		Timestamp: time.Now().UTC(),
		System:    r.cfg.System,
		Path:      r.cfg.Path,
		Config: ConfigSummary{
			Threads:   r.cfg.Threads,
			FileSize:  r.cfg.FileSize,
			BlockSize: r.cfg.BlockSize,
			Duration:  r.cfg.Duration,
			MetaFiles: r.cfg.MetaFiles,
		},
		Workloads: make(map[WorkloadType]*WorkloadResult),
	}

	start := time.Now()

	for _, w := range workloads {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		var (
			wr  *WorkloadResult
			err error
		)

		switch w {
		case SeqWrite:
			wr, err = runSeqWrite(ctx, r.cfg, dir, r.progress)
		case SeqRead:
			wr, err = runSeqRead(ctx, r.cfg, dir, r.progress)
		case RandWrite:
			wr, err = runRandWrite(ctx, r.cfg, dir, r.progress)
		case RandRead:
			wr, err = runRandRead(ctx, r.cfg, dir, r.progress)
		case Metadata:
			wr, err = runMetadata(ctx, r.cfg, dir, r.progress)
		default:
			return nil, fmt.Errorf("unknown workload %q", w)
		}

		if err != nil {
			return nil, fmt.Errorf("workload %s: %w", w, err)
		}

		result.Workloads[w] = wr
	}

	result.TotalDuration = time.Since(start)

	return result, nil
}
