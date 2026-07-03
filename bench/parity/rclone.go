package parity

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// rcloneRunner drives the rclone baseline. The remote is configured entirely
// through process environment variables (RCLONE_CONFIG_PARITY_*) — no config
// file, no credentials on disk or argv.
type rcloneRunner struct {
	opts    Opts
	s3cfg   *s3Config
	env     []string
	version string

	largeDir string
	smallDir string
}

func newRcloneRunner(ctx context.Context, opts Opts, s3cfg *s3Config) (*rcloneRunner, error) {
	r := &rcloneRunner{
		opts:     opts,
		s3cfg:    s3cfg,
		env:      s3cfg.rcloneEnv(),
		largeDir: filepath.Join(opts.WorkDir, "rclone-dataset", "large"),
		smallDir: filepath.Join(opts.WorkDir, "rclone-dataset", "small"),
	}
	out, err := exec.CommandContext(ctx, opts.RcloneBin, "version").Output()
	if err != nil {
		return nil, fmt.Errorf("rclone not runnable (%s): %w — install rclone or pass --rclone-bin", opts.RcloneBin, err)
	}
	if sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan() {
		r.version = strings.TrimSpace(sc.Text())
	}
	return r, nil
}

// stage materializes the deterministic dataset for one size class (idempotent
// across concurrency levels — the bytes are identical to what the dittofs
// lane writes into the engine).
func (r *rcloneRunner) stage(cl sizeClass) (string, error) {
	dir := r.largeDir
	if cl.name == "small" {
		dir = r.smallDir
	}
	return dir, stageDataset(dir, cl.name, cl.fileCount, cl.fileBytes, r.opts.Seed)
}

// runConc runs every selected rclone cell at one concurrency level.
func (r *rcloneRunner) runConc(ctx context.Context, basePrefix string, conc int) ([]Cell, error) {
	var cells []Cell
	for _, cl := range r.opts.classes() {
		wantUp, wantDown, wantMeta := r.opts.wantLanes(cl)
		if !wantUp && !wantDown && !wantMeta {
			continue
		}
		srcDir, err := r.stage(cl)
		if err != nil {
			return nil, fmt.Errorf("stage %s dataset: %w", cl.name, err)
		}
		prefix := fmt.Sprintf("%s/rclone/c%d/%s", basePrefix, conc, cl.name)
		target := r.s3cfg.rcloneTarget(prefix)
		totalBytes := cl.fileBytes * int64(cl.fileCount)

		// Upload (always runs — downloads and meta need the data there).
		elapsed, err := r.timedRclone(ctx, conc, "copy", srcDir, target)
		if err != nil {
			return nil, fmt.Errorf("rclone %s upload (c%d): %w", cl.name, conc, err)
		}
		if wantUp {
			cell := newCell(ToolRclone, cl.uploadQ, conc, cl.fileCount, totalBytes, elapsed)
			cell.Objects = int64(cl.fileCount)
			printRcloneCell(cell)
			cells = append(cells, cell)
		}

		if wantDown {
			destDir := filepath.Join(r.opts.WorkDir, fmt.Sprintf("rclone-down-c%d-%s", conc, cl.name))
			elapsed, err := r.timedRclone(ctx, conc, "copy", target, destDir)
			if err != nil {
				return nil, fmt.Errorf("rclone %s download (c%d): %w", cl.name, conc, err)
			}
			cell := newCell(ToolRclone, cl.downloadQ, conc, cl.fileCount, totalBytes, elapsed)
			printRcloneCell(cell)
			cells = append(cells, cell)
			_ = os.RemoveAll(destDir)
		}

		switch {
		case wantMeta:
			cell, err := r.metaCell(ctx, target, conc, cl)
			if err != nil {
				return nil, err
			}
			cells = append(cells, cell)
		case !r.opts.KeepRemote:
			if _, err := r.rclone(ctx, conc, "purge", target); err != nil {
				fmt.Fprintf(os.Stderr, "parity: rclone cleanup %s: %v\n", prefix, err)
			}
		}
	}
	return cells, nil
}

// metaCell lists then deletes the uploaded small tree: rclone's remote-object
// metadata throughput, counterpart of dittofsMetaCell.
func (r *rcloneRunner) metaCell(ctx context.Context, target string, conc int, cl sizeClass) (Cell, error) {
	start := time.Now()
	out, err := r.rclone(ctx, conc, "lsf", "-R", "--files-only", target)
	if err != nil {
		return Cell{}, fmt.Errorf("rclone meta lsf (c%d): %w", conc, err)
	}
	objects := int64(0)
	for sc := bufio.NewScanner(bytes.NewReader(out)); sc.Scan(); {
		if strings.TrimSpace(sc.Text()) != "" {
			objects++
		}
	}
	if !r.opts.KeepRemote {
		if _, err := r.rclone(ctx, conc, "purge", target); err != nil {
			return Cell{}, fmt.Errorf("rclone meta purge (c%d): %w", conc, err)
		}
	}
	elapsed := time.Since(start)

	cell := newCell(ToolRclone, QuadMeta, conc, 0, 0, elapsed)
	cell.Objects = objects
	if elapsed > 0 {
		cell.OpsPerSec = float64(objects) / elapsed.Seconds()
	}
	fmt.Printf("parity: rclone  %-14s c%-3d %8.1f obj/s   %6.1fs  (objects=%d)\n",
		QuadMeta, conc, cell.OpsPerSec, cell.Seconds, objects)
	return cell, nil
}

// timedRclone runs one rclone transfer and returns its wall time.
func (r *rcloneRunner) timedRclone(ctx context.Context, conc int, args ...string) (time.Duration, error) {
	start := time.Now()
	_, err := r.rclone(ctx, conc, args...)
	return time.Since(start), err
}

// rclone invokes the binary with the parity remote in env and the
// concurrency knobs that make it comparable to the dittofs lane:
// --transfers/--checkers = client concurrency; 16 MiB multipart chunks
// matching dittofs's packed-block size (and the 2026-06-29 baseline).
func (r *rcloneRunner) rclone(ctx context.Context, conc int, args ...string) ([]byte, error) {
	full := append([]string{
		"--transfers", fmt.Sprint(conc),
		"--checkers", fmt.Sprint(conc),
		"--s3-chunk-size", "16Mi",
		"--s3-upload-concurrency", fmt.Sprint(conc),
		"--stats", "0",
		"-q",
	}, args...)
	cmd := exec.CommandContext(ctx, r.opts.RcloneBin, full...)
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rclone %s: %w\n%s", strings.Join(args, " "), err, stderr.String())
	}
	return stdout.Bytes(), nil
}

func printRcloneCell(c Cell) {
	fmt.Printf("parity: rclone  %-14s c%-3d %8.1f Mbit/s  %6.1fs\n",
		c.Quadrant, c.Conc, c.ThroughputMbps, c.Seconds)
}
