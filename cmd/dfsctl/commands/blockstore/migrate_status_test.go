package blockstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

// TestMigrateStatusRenderer_Headers asserts the table renderer reports
// the canonical FIELD/VALUE header layout used by every per-resource
// status command (mirrors graceStatusRenderer).
func TestMigrateStatusRenderer_Headers(t *testing.T) {
	r := migrateStatusRenderer{resp: &apiclient.MigrateStatusResponse{}}
	assert.Equal(t, []string{"FIELD", "VALUE"}, r.Headers())
}

// TestMigrateStatusRenderer_RowsAllFields asserts every documented status
// field appears as its own row in the table output. This is the contract
// the runbook (Plan 14-07) references for operator-facing transcripts.
func TestMigrateStatusRenderer_RowsAllFields(t *testing.T) {
	resp := &apiclient.MigrateStatusResponse{
		Share:           "myshare",
		BlockLayout:     "legacy",
		FilesTotal:      42,
		FilesDone:       17,
		FilesSkipped:    1,
		BytesUploaded:   1024,
		BytesDeduped:    256,
		JournalPresent:  true,
		SnapshotPresent: false,
		LastCommitAt:    "2026-05-05T17:30:08Z",
	}
	r := migrateStatusRenderer{resp: resp}
	rows := r.Rows()

	require.Len(t, rows, 10, "expected 10 field rows, got %d", len(rows))

	// Build a field-name -> value map for order-independent assertions.
	got := make(map[string]string, len(rows))
	for _, row := range rows {
		require.Len(t, row, 2, "every row must be [field,value], got %v", row)
		got[row[0]] = row[1]
	}

	assert.Equal(t, "myshare", got["Share"])
	assert.Equal(t, "legacy", got["BlockLayout"])
	assert.Equal(t, "42", got["FilesTotal"])
	assert.Equal(t, "17", got["FilesDone"])
	assert.Equal(t, "1", got["FilesSkipped"])
	assert.Equal(t, "1024", got["BytesUploaded"])
	assert.Equal(t, "256", got["BytesDeduped"])
	assert.Equal(t, "true", got["JournalPresent"])
	assert.Equal(t, "false", got["SnapshotPresent"])
	assert.Equal(t, "2026-05-05T17:30:08Z", got["LastCommitAt"])
}

// TestMigrateStatusCmd_RequiresShareFlag asserts cobra rejects invocation
// without --share and surfaces the required-flag error.
func TestMigrateStatusCmd_RequiresShareFlag(t *testing.T) {
	// MarkFlagRequired adds "share" to the required-flag set; cobra surfaces
	// the missing flag during command validation. We test the static
	// configuration here rather than driving cobra end-to-end (which would
	// require constructing a fully-parented root command).
	flag := migrateStatusCmd.Flags().Lookup("share")
	require.NotNil(t, flag, "--share flag must be declared")

	requiredAnnotations := flag.Annotations["cobra_annotation_bash_completion_one_required_flag"]
	assert.Contains(t, requiredAnnotations, "true",
		"--share must be marked required so cobra exits non-zero when omitted")
}

// TestMigrateStatusCmd_RegisteredUnderMigrate asserts the subcommand is
// reachable as `dfsctl blockstore migrate status`. Plan 14-06 acceptance
// criterion: `migrateCmd.AddCommand(migrateStatusCmd)` lives in
// blockstore.go's init().
func TestMigrateStatusCmd_RegisteredUnderMigrate(t *testing.T) {
	var found bool
	for _, c := range migrateCmd.Commands() {
		if c.Use == "status" || c == migrateStatusCmd {
			found = true
			break
		}
	}
	assert.True(t, found, "migrate status subcommand must be registered under migrateCmd")
}
