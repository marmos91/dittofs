package report

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/marmos91/dittofs/internal/dfsbench/exec"
	"github.com/marmos91/dittofs/internal/dfsbench/fio"
)

// NewReportCmd builds the `report` subcommand, which re-renders the comparison
// table from a results directory.
func NewReportCmd() *cobra.Command {
	var results string
	cmd := &cobra.Command{
		Use:     "report",
		Short:   "Re-render the comparison table from a results directory",
		Example: "  dfsbench report --results ./bench-results",
		RunE: func(cmd *cobra.Command, _ []string) error {
			rs, err := fio.LoadResults(results)
			if err != nil {
				return err
			}
			if len(rs) == 0 {
				return fmt.Errorf("no results in %q", results)
			}
			_, _ = fmt.Fprint(exec.CmdOut, RenderTable(rs))
			return nil
		},
	}
	cmd.Flags().StringVar(&results, "results", "./bench-results", "results directory")
	return cmd
}

// RenderTable produces the markdown comparison table. Columns follow the plan's
// dictionary; rate columns render "—" when not the workload's headline metric,
// and CTXSW/s + CPU% dash when unmetered (off Linux / pre-meter runs). S3MB
// stays 0 until the S3 network meter lands.
func RenderTable(rs []fio.CellResult) string {
	if len(rs) == 0 {
		return "no results\n"
	}
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i], rs[j]
		if a.System != b.System {
			return a.System < b.System
		}
		if a.Workload != b.Workload {
			return a.Workload < b.Workload
		}
		return a.Size < b.Size
	})

	head := []string{"SYSTEM", "WORKLOAD", "SIZE", "PROTO", "PASS", "IOPS", "MB/s", "p50µs", "p99µs", "S3MB", "CTXSW/s", "CPU%", "err"}
	rows := make([][]string, 0, len(rs))
	for _, r := range rs {
		rows = append(rows, []string{
			r.System, r.Workload, r.Size, r.Protocol, r.Pass,
			rate(r.IOPS, isRandom(r.Workload)),
			rate(r.ThroughputMBps, !isRandom(r.Workload)),
			fmt.Sprintf("%.0f", r.LatencyP50Us),
			fmt.Sprintf("%.0f", r.LatencyP99Us),
			fmt.Sprintf("%d", r.S3Bytes/fio.Mib),
			metered(r.CtxSwPerSec),
			metered(r.CPUPct),
			fmt.Sprintf("%d", r.Errors),
		})
	}
	return markdownTable(head, rows)
}

// isRandom reports whether IOPS is the workload's headline metric.
func isRandom(w string) bool {
	return strings.HasPrefix(w, "rand-") || w == "mixed-rw" || w == "metadata"
}

// rate formats a rate column, dashing it when it isn't this workload's headline.
func rate(v float64, headline bool) string {
	if !headline {
		return "—"
	}
	return fmt.Sprintf("%.0f", v)
}

// metered formats a server-resource column, dashing an unmeasured 0 (off Linux,
// or a run predating the meter) rather than printing a misleading zero.
func metered(v float64) string {
	if v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f", v)
}

// markdownTable renders a GitHub-flavored markdown table with aligned columns.
func markdownTable(head []string, rows [][]string) string {
	w := make([]int, len(head))
	for i, h := range head {
		w[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if len(c) > w[i] {
				w[i] = len(c)
			}
		}
	}
	var b strings.Builder
	writeRow := func(cells []string) {
		b.WriteString("|")
		for i, c := range cells {
			fmt.Fprintf(&b, " %-*s |", w[i], c)
		}
		b.WriteString("\n")
	}
	writeRow(head)
	b.WriteString("|")
	for i := range head {
		b.WriteString(" " + strings.Repeat("-", w[i]) + " |")
	}
	b.WriteString("\n")
	for _, r := range rows {
		writeRow(r)
	}
	return b.String()
}
