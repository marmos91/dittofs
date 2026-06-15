package snapshotpolicy

import (
	"os"
	"strconv"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// policyClient is the narrow interface the leaf commands use. Tests replace
// getClient with a fake; production uses the authenticated apiclient.
type policyClient interface {
	UpsertSnapshotPolicy(share string, req apiclient.UpsertSnapshotPolicyRequest) (*apiclient.SnapshotPolicy, error)
	GetSnapshotPolicy(share string) (*apiclient.SnapshotPolicy, error)
	DeleteSnapshotPolicy(share string) error
	ListSnapshotPolicies() ([]apiclient.SnapshotPolicy, error)
	RunSnapshotPolicy(share string) (*apiclient.CreateSnapshotResponse, error)
}

// getClient is overridable by tests.
var getClient = func() (policyClient, error) {
	return cmdutil.GetAuthenticatedClient()
}

// policyRow renders one row of the policy table.
type policyRow struct {
	Share    string
	Enabled  string
	Interval string
	KeepLast string
	TTL      string
	LastRun  string
}

// PolicyList renders policies as a 6-column table.
type PolicyList []policyRow

func (pl PolicyList) Headers() []string {
	return []string{"SHARE", "ENABLED", "INTERVAL", "KEEP_LAST", "TTL", "LAST_RUN"}
}

func (pl PolicyList) Rows() [][]string {
	rows := make([][]string, 0, len(pl))
	for _, r := range pl {
		rows = append(rows, []string{r.Share, r.Enabled, r.Interval, r.KeepLast, r.TTL, r.LastRun})
	}
	return rows
}

func toRow(p apiclient.SnapshotPolicy) policyRow {
	keep := "unlimited"
	if p.KeepLast > 0 {
		keep = strconv.Itoa(p.KeepLast)
	}
	last := "never"
	if p.LastRunAt != nil && !p.LastRunAt.IsZero() {
		last = p.LastRunAt.UTC().Format("2006-01-02 15:04:05Z")
	}
	return policyRow{
		Share:    p.Share,
		Enabled:  cmdutil.BoolToYesNo(p.Enabled),
		Interval: p.Interval,
		KeepLast: keep,
		TTL:      cmdutil.EmptyOr(p.TTL, "-"),
		LastRun:  last,
	}
}

// printPolicy emits a single policy in the requested output format.
func printPolicy(p *apiclient.SnapshotPolicy) error {
	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}
	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, p)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, p)
	default:
		return output.PrintTable(os.Stdout, PolicyList{toRow(*p)})
	}
}
