// Package parity is the rclone-parity benchmark harness (#1467, tracker
// #1466). It runs dittofs (engine path: write → rollup → carve → drain to
// remote) and an rclone baseline against the SAME S3 bucket, apples-to-apples,
// across the four workload quadrants — upload/download × large/small file —
// plus a metadata-ops lane, at several client concurrency levels. It emits a
// machine-readable scorecard (JSON + CSV) and a human markdown table with the
// dittofs-vs-rclone ratio per quadrant per concurrency.
//
// The dittofs lane uses the same production engine wiring as bench/blockstore
// (FSStore + syncer + packed-block carve), with upload concurrency PINNED to
// the cell's level so it is comparable to rclone --transfers. During dittofs
// cells the harness samples the datapath Prometheus gauges (inflight, window,
// queue depth, goodput) into a per-cell timeline that ships inside the JSON
// artifact.
//
// Credentials come from environment variables only (AWS_S3_BUCKET,
// AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_ENDPOINT_URL, ...): they are
// never written to disk. See bench/scripts/parity-smoke.sh (local MinIO, no
// cloud creds) and bench/scripts/parity-scw.sh (disposable Scaleway VM → real
// WAN target).
package parity

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Quadrant identifiers. "meta" is the metadata-ops lane: remote-object
// list + delete over the tool's own uploaded small-file dataset (it also
// doubles as per-concurrency bucket cleanup).
const (
	QuadUploadLarge   = "upload-large"
	QuadUploadSmall   = "upload-small"
	QuadDownloadLarge = "download-large"
	QuadDownloadSmall = "download-small"
	QuadMeta          = "meta"
)

// AllQuadrants is the default quadrant set, in execution order (downloads
// need their upload's data; meta needs the small dataset and cleans up).
var AllQuadrants = []string{QuadUploadLarge, QuadDownloadLarge, QuadUploadSmall, QuadDownloadSmall, QuadMeta}

// Tool identifiers.
const (
	ToolDittofs = "dittofs"
	ToolRclone  = "rclone"
)

// Opts configures one harness run.
type Opts struct {
	Label     string        // run label, embedded in artifact names and the remote key prefix
	Conc      []int         // client concurrency levels (rclone --transfers / pinned ParallelUploads)
	Quadrants []string      // subset of AllQuadrants
	Tools     []string      // subset of {dittofs, rclone}
	Seed      uint64        // deterministic dataset seed (same bytes for both tools)
	WorkDir   string        // scratch root: staged datasets + per-cell engine dirs
	OutDir    string        // where scorecard artifacts land (bench/results)
	RcloneBin string        // rclone binary (default "rclone")
	Sample    time.Duration // gauge-timeline sample interval (default 500ms)

	LargeFileBytes int64 // size of each large file
	LargeFileCount int   // number of large files
	SmallFileBytes int64 // size of each small file
	SmallFileCount int   // number of small files

	KeepRemote bool // skip remote cleanup (debugging); meta-delete phase is skipped
}

// Validate applies defaults and rejects nonsense.
func (o *Opts) Validate() error {
	if o.Label == "" {
		o.Label = "parity"
	}
	if len(o.Conc) == 0 {
		o.Conc = []int{1, 8, 24, 64}
	}
	for _, c := range o.Conc {
		if c < 1 || c > 256 {
			return fmt.Errorf("concurrency %d out of range [1,256]", c)
		}
	}
	if len(o.Quadrants) == 0 {
		o.Quadrants = slices.Clone(AllQuadrants)
	}
	for _, q := range o.Quadrants {
		if !slices.Contains(AllQuadrants, q) {
			return fmt.Errorf("unknown quadrant %q (want one of %s)", q, strings.Join(AllQuadrants, ", "))
		}
	}
	if len(o.Tools) == 0 {
		o.Tools = []string{ToolDittofs, ToolRclone}
	}
	for _, t := range o.Tools {
		if t != ToolDittofs && t != ToolRclone {
			return fmt.Errorf("unknown tool %q (want %s or %s)", t, ToolDittofs, ToolRclone)
		}
	}
	if o.RcloneBin == "" {
		o.RcloneBin = "rclone"
	}
	if o.Sample <= 0 {
		o.Sample = 500 * time.Millisecond
	}
	if o.LargeFileBytes <= 0 {
		o.LargeFileBytes = 1 << 30 // 1 GiB
	}
	if o.LargeFileCount <= 0 {
		o.LargeFileCount = 4
	}
	if o.SmallFileBytes <= 0 {
		o.SmallFileBytes = 64 << 10 // 64 KiB
	}
	if o.SmallFileCount <= 0 {
		o.SmallFileCount = 2048
	}
	if o.OutDir == "" {
		o.OutDir = filepath.Join("bench", "results")
	}
	return nil
}

func (o *Opts) wantTool(t string) bool     { return slices.Contains(o.Tools, t) }
func (o *Opts) wantQuadrant(q string) bool { return slices.Contains(o.Quadrants, q) }

// Cell is one measured (tool, quadrant, concurrency) result.
type Cell struct {
	Tool     string  `json:"tool"`
	Quadrant string  `json:"quadrant"`
	Conc     int     `json:"conc"`
	Files    int     `json:"files"`
	Bytes    int64   `json:"bytes"`
	Objects  int64   `json:"objects,omitempty"` // meta lane: remote objects listed/deleted
	Seconds  float64 `json:"seconds"`
	// ThroughputMbps is payload megabits per second (upload/download lanes).
	ThroughputMbps float64 `json:"throughput_mbps,omitempty"`
	// OpsPerSec is files/s (small-file lanes) or objects/s (meta lane).
	OpsPerSec float64 `json:"ops_per_sec,omitempty"`
	// RemoteReadBytes (dittofs download lanes) is the verified bytes actually
	// fetched from packed blocks — proof the reads hit the remote, not a
	// leftover local copy.
	RemoteReadBytes int64  `json:"remote_read_bytes,omitempty"`
	Error           string `json:"error,omitempty"`
}

// Run is the full scorecard artifact.
type Run struct {
	SchemaVersion int       `json:"schema_version"`
	Label         string    `json:"label"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	GitCommit     string    `json:"git_commit,omitempty"`
	Host          string    `json:"host,omitempty"`
	// EndpointHost is the S3 endpoint host only — never credentials.
	EndpointHost  string              `json:"endpoint_host,omitempty"`
	Bucket        string              `json:"bucket,omitempty"`
	RcloneVersion string              `json:"rclone_version,omitempty"`
	Opts          RunOpts             `json:"opts"`
	Cells         []Cell              `json:"cells"`
	Timelines     map[string]Timeline `json:"timelines,omitempty"` // key: dittofs/<quadrant>/c<N>
}

// RunOpts is the subset of Opts worth persisting in the artifact.
type RunOpts struct {
	Conc           []int  `json:"conc"`
	LargeFileBytes int64  `json:"large_file_bytes"`
	LargeFileCount int    `json:"large_file_count"`
	SmallFileBytes int64  `json:"small_file_bytes"`
	SmallFileCount int    `json:"small_file_count"`
	Seed           uint64 `json:"seed"`
}

// Execute runs the full harness and writes the scorecard artifacts.
// Returns the artifact base path (without extension).
func Execute(ctx context.Context, opts Opts) (string, error) {
	if err := opts.Validate(); err != nil {
		return "", err
	}
	s3cfg, err := s3ConfigFromEnv()
	if err != nil {
		return "", err
	}
	if opts.WorkDir == "" {
		wd, err := os.MkdirTemp("", "dfsbench-parity-*")
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(wd)
		opts.WorkDir = wd
	}

	started := time.Now().UTC()
	run := &Run{
		SchemaVersion: 1,
		Label:         opts.Label,
		StartedAt:     started,
		GitCommit:     gitCommit(),
		Host:          hostname(),
		EndpointHost:  s3cfg.endpointHost(),
		Bucket:        s3cfg.Bucket,
		Opts: RunOpts{
			Conc:           opts.Conc,
			LargeFileBytes: opts.LargeFileBytes,
			LargeFileCount: opts.LargeFileCount,
			SmallFileBytes: opts.SmallFileBytes,
			SmallFileCount: opts.SmallFileCount,
			Seed:           opts.Seed,
		},
		Timelines: map[string]Timeline{},
	}

	basePrefix := fmt.Sprintf("%sparity/%s-%s", s3cfg.prefixRoot(), opts.Label, started.Format("20060102-150405"))
	fmt.Printf("parity: bucket=%s endpoint=%s prefix=%s conc=%v\n",
		s3cfg.Bucket, s3cfg.endpointHost(), basePrefix, opts.Conc)

	var rcl *rcloneRunner
	if opts.wantTool(ToolRclone) {
		rcl, err = newRcloneRunner(ctx, opts, s3cfg)
		if err != nil {
			return "", err
		}
		run.RcloneVersion = rcl.version
	}

	for _, conc := range opts.Conc {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if opts.wantTool(ToolDittofs) {
			cells, tls, err := runDittofsConc(ctx, opts, s3cfg, basePrefix, conc)
			if err != nil {
				return "", fmt.Errorf("dittofs cells at conc=%d: %w", conc, err)
			}
			run.Cells = append(run.Cells, cells...)
			for k, v := range tls {
				run.Timelines[k] = v
			}
		}
		if rcl != nil {
			cells, err := rcl.runConc(ctx, basePrefix, conc)
			if err != nil {
				return "", fmt.Errorf("rclone cells at conc=%d: %w", conc, err)
			}
			run.Cells = append(run.Cells, cells...)
		}
	}

	run.FinishedAt = time.Now().UTC()
	return writeArtifacts(opts, run)
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}
