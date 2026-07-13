package fio

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// LoadOpts are the per-cell fio knobs the harness overrides on top of the .fio
// template. Zero values mean "use the job file's own value".
type LoadOpts struct {
	Size         string // fio --size (e.g. "64k", "1g")
	Threads      int    // BENCH_THREADS
	Runtime      int    // BENCH_RUNTIME seconds
	Engine       string // FIO_ENGINE (defaults per-OS)
	FioBin       string // fio binary (default "fio")
	LiveProgress bool   // let fio's live ETA line reach stdout (set on a TTY)
}

// defaultEngine picks a portable fio ioengine. libaio is Linux-only; local dev
// on macOS needs psync. Overridable via LoadOpts.Engine / --fio-engine.
func defaultEngine() string {
	if runtime.GOOS == "linux" {
		return "libaio"
	}
	return "psync"
}

// RunFio executes workload against targetDir and fills the metric fields of a
// CellResult. The caller stamps identity/pass fields.
func RunFio(ctx context.Context, workload, targetDir string, opts LoadOpts) (CellResult, error) {
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

	// --output routes the JSON report to a file so fio's stdout is free for its
	// live ETA line. On a TTY (LiveProgress) we let that reach the terminal;
	// otherwise fio's stdout is discarded and it auto-suppresses the ETA, keeping
	// piped/captured output clean.
	outFile, err := os.CreateTemp("", "dfsbench-out-*.json")
	if err != nil {
		return CellResult{}, err
	}
	outPath := outFile.Name()
	_ = outFile.Close()
	defer func() { _ = os.Remove(outPath) }()

	// directory/size/threads/runtime are baked into the rendered job file (see
	// jobDefaults) because job-file options beat fio's global CLI flags.
	cmd := exec.CommandContext(ctx, bin, "--output="+outPath, "--output-format=json", jobPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if opts.LiveProgress {
		cmd.Stdout = os.Stdout
	}
	if err := cmd.Run(); err != nil {
		return CellResult{}, fmt.Errorf("run %s (%s): %w\n%s", bin, workload, err, stderr.Bytes())
	}
	if opts.LiveProgress {
		_, _ = fmt.Fprintln(os.Stdout) // close fio's in-place ETA line
	}

	out, err := os.ReadFile(outPath)
	if err != nil {
		return CellResult{}, err
	}
	res, err := parseFioJSON(out)
	if err != nil {
		return CellResult{}, fmt.Errorf("%s: %w", workload, err)
	}
	return res, nil
}

// CheckTarget makes the target directory (mkdir -p, 0755) if absent and verifies
// it is writable — fio's own error on a bad dir is opaque, so fail early and
// clearly. An existing non-directory makes MkdirAll fail with a clear error.
func CheckTarget(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("target %q: %w", dir, err)
	}
	probe := filepath.Join(dir, ".dfsbench-write-probe")
	if err := os.WriteFile(probe, []byte("x"), 0o644); err != nil {
		return fmt.Errorf("target %q not writable: %w", dir, err)
	}
	return os.Remove(probe)
}
