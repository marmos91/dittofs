package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// LoadOpts are the per-cell fio knobs the harness overrides on top of the .fio
// template. Zero values mean "use the job file's own value".
type LoadOpts struct {
	Size    string // fio --size (e.g. "64k", "1g")
	Threads int    // BENCH_THREADS
	Runtime int    // BENCH_RUNTIME seconds
	Engine  string // FIO_ENGINE (defaults per-OS)
	FioBin  string // fio binary (default "fio")
}

// defaultEngine picks a portable fio ioengine. libaio is Linux-only; local dev
// on macOS needs psync. Overridable via LoadOpts.Engine / --fio-engine.
func defaultEngine() string {
	if runtime.GOOS == "linux" {
		return "libaio"
	}
	return "psync"
}

// runFio executes workload against targetDir and fills the metric fields of a
// CellResult. The caller stamps identity/pass fields.
func runFio(ctx context.Context, workload, targetDir string, opts LoadOpts) (CellResult, error) {
	engine := opts.Engine
	if engine == "" {
		engine = defaultEngine()
	}
	threads := opts.Threads
	if threads == 0 {
		threads = 4
	}
	runtimeSec := opts.Runtime
	if runtimeSec == 0 {
		runtimeSec = 60
	}
	bin := opts.FioBin
	if bin == "" {
		bin = "fio"
	}

	body, err := loadJob(workload, jobDefaults(engine, targetDir, opts.Size, threads, runtimeSec))
	if err != nil {
		return CellResult{}, err
	}

	jobFile, err := os.CreateTemp("", "dfsbench-*.fio")
	if err != nil {
		return CellResult{}, err
	}
	jobPath := jobFile.Name()
	defer func() { _ = os.Remove(jobPath) }()
	_, writeErr := jobFile.WriteString(body)
	closeErr := jobFile.Close()
	if writeErr != nil {
		return CellResult{}, writeErr
	}
	if closeErr != nil {
		return CellResult{}, closeErr
	}

	// directory/size/threads/runtime are baked into the rendered job file (see
	// jobDefaults) because job-file options beat fio's global CLI flags.
	cmd := exec.CommandContext(ctx, bin, "--output-format=json", jobPath)
	out, err := cmd.Output()
	if err != nil {
		return CellResult{}, fmt.Errorf("run %s (%s): %w\n%s", bin, workload, err, fioStderr(err))
	}

	res, err := parseFioJSON(out)
	if err != nil {
		return CellResult{}, fmt.Errorf("%s: %w", workload, err)
	}
	return res, nil
}

// fioStderr surfaces a failed fio invocation's stderr, if any.
func fioStderr(err error) []byte {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.Stderr
	}
	return nil
}

// checkTarget verifies the target directory exists and is writable — fio's own
// error on a bad dir is opaque, so fail early and clearly.
func checkTarget(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("target %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("target %q is not a directory", dir)
	}
	probe := filepath.Join(dir, ".dfsbench-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return fmt.Errorf("target %q not writable: %w", dir, err)
	}
	return os.Remove(probe)
}
