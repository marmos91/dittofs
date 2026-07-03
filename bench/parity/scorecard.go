package parity

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// writeArtifacts emits the machine-readable scorecard (JSON with embedded
// gauge timelines + flat CSV) and the human markdown table. Returns the
// artifact base path (no extension).
func writeArtifacts(opts Opts, run *Run) (string, error) {
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return "", err
	}
	base := filepath.Join(opts.OutDir,
		fmt.Sprintf("parity-%s-%s", run.Label, run.StartedAt.Format("20060102-150405")))

	js, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".json", append(js, '\n'), 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".csv", []byte(cellsCSV(run.Cells)), 0o644); err != nil {
		return "", err
	}
	md := Scorecard(run)
	if err := os.WriteFile(base+".md", []byte(md), 0o644); err != nil {
		return "", err
	}
	fmt.Printf("\n%s\nparity: artifacts written to %s.{json,csv,md}\n", md, base)
	return base, nil
}

// cellsCSV renders every cell as one flat CSV row.
func cellsCSV(cells []Cell) string {
	var sb strings.Builder
	w := csv.NewWriter(&sb)
	_ = w.Write([]string{"tool", "quadrant", "conc", "files", "bytes", "objects",
		"seconds", "throughput_mbps", "ops_per_sec", "remote_read_bytes", "error"})
	for _, c := range cells {
		_ = w.Write([]string{
			c.Tool, c.Quadrant, strconv.Itoa(c.Conc), strconv.Itoa(c.Files),
			strconv.FormatInt(c.Bytes, 10), strconv.FormatInt(c.Objects, 10),
			fmtF(c.Seconds), fmtF(c.ThroughputMbps), fmtF(c.OpsPerSec),
			strconv.FormatInt(c.RemoteReadBytes, 10), c.Error,
		})
	}
	w.Flush()
	return sb.String()
}

func fmtF(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }

// Scorecard renders the human markdown: one table per quadrant, one row per
// concurrency, dittofs vs rclone with the parity ratio.
func Scorecard(run *Run) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "# rclone-parity scorecard — %s\n\n", run.Label)
	fmt.Fprintf(&sb, "- started: %s  finished: %s\n", run.StartedAt.Format("2006-01-02 15:04:05Z"), run.FinishedAt.Format("15:04:05Z"))
	fmt.Fprintf(&sb, "- host: %s  commit: %s\n", orDash(run.Host), orDash(run.GitCommit))
	fmt.Fprintf(&sb, "- target: %s / bucket %s\n", orDash(run.EndpointHost), orDash(run.Bucket))
	if run.RcloneVersion != "" {
		fmt.Fprintf(&sb, "- baseline: %s\n", run.RcloneVersion)
	}
	fmt.Fprintf(&sb, "- dataset: large %d x %s, small %d x %s (seed %d)\n\n",
		run.Opts.LargeFileCount, humanBytes(run.Opts.LargeFileBytes),
		run.Opts.SmallFileCount, humanBytes(run.Opts.SmallFileBytes), run.Opts.Seed)

	for _, q := range AllQuadrants {
		rows := scoreRows(run, q)
		if len(rows) == 0 {
			continue
		}
		unit := "Mbit/s"
		if q == QuadMeta {
			unit = "obj/s"
		}
		fmt.Fprintf(&sb, "## %s\n\n", q)
		fmt.Fprintf(&sb, "| conc | dittofs (%s) | rclone (%s) | dittofs/rclone |\n", unit, unit)
		sb.WriteString("|---:|---:|---:|---:|\n")
		for _, r := range rows {
			sb.WriteString(r)
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// scoreRows builds the per-concurrency rows for one quadrant.
func scoreRows(run *Run, quadrant string) []string {
	metric := func(c *Cell) float64 {
		if quadrant == QuadMeta {
			return c.OpsPerSec
		}
		return c.ThroughputMbps
	}
	byConc := map[int]map[string]*Cell{}
	var concs []int
	for i := range run.Cells {
		c := &run.Cells[i]
		if c.Quadrant != quadrant {
			continue
		}
		if byConc[c.Conc] == nil {
			byConc[c.Conc] = map[string]*Cell{}
			concs = append(concs, c.Conc)
		}
		byConc[c.Conc][c.Tool] = c
	}
	var rows []string
	for _, conc := range concs {
		d, r := byConc[conc][ToolDittofs], byConc[conc][ToolRclone]
		ratio := "—"
		if d != nil && r != nil && metric(r) > 0 {
			ratio = fmt.Sprintf("%.0f%%", 100*metric(d)/metric(r))
		}
		rows = append(rows, fmt.Sprintf("| %d | %s | %s | %s |\n",
			conc, cellMetric(d, metric), cellMetric(r, metric), ratio))
	}
	return rows
}

func cellMetric(c *Cell, metric func(*Cell) float64) string {
	if c == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", metric(c))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%dGiB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%dMiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%dKiB", n>>10)
	default:
		return fmt.Sprintf("%dB", n)
	}
}
