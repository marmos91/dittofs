package report

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/internal/dfsbench/fio"
)

func TestRenderTable_HeadlineDashing(t *testing.T) {
	rs := []fio.CellResult{
		{System: "local", Workload: "seq-read", Size: "large", Protocol: "local", Pass: "warm", ThroughputMBps: 2400},
		{System: "local", Workload: "rand-read-4k", Size: "large", Protocol: "local", Pass: "warm", IOPS: 8940},
	}
	out := RenderTable(rs)
	if !strings.Contains(out, "2400") || !strings.Contains(out, "8940") {
		t.Errorf("missing values:\n%s", out)
	}
	// seq-read: IOPS column dashed; rand: MB/s column dashed.
	if !strings.Contains(out, "—") {
		t.Errorf("expected dashed non-headline cells:\n%s", out)
	}
}
