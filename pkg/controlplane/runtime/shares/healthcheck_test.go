package shares

import (
	"context"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/health"
	metadatamemory "github.com/marmos91/dittofs/pkg/metadata/store/memory"
)

// TestCombineShareReports_WorstOfDerivation drives the pure function
// that derives the share's status from its two subsystems. Tested as
// a table so the worst-of severity ordering is unambiguous and a
// future maintainer can see at a glance which combinations should
// produce which result.
func TestCombineShareReports_WorstOfDerivation(t *testing.T) {
	rep := func(s health.Status, msg string) health.Report {
		return health.Report{Status: s, Message: msg}
	}

	cases := []struct {
		name       string
		meta       health.Report
		block      health.Report
		hasMeta    bool
		hasBlock   bool
		wantStatus health.Status
		wantPrefix string // expected prefix on the combined message
	}{
		{
			name:       "both healthy",
			meta:       rep(health.StatusHealthy, ""),
			block:      rep(health.StatusHealthy, ""),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusHealthy,
		},
		{
			name:       "metadata unhealthy beats block healthy",
			meta:       rep(health.StatusUnhealthy, "badger view: db closed"),
			block:      rep(health.StatusHealthy, ""),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusUnhealthy,
			wantPrefix: "metadata: badger view: db closed",
		},
		{
			name:       "block unhealthy beats metadata healthy",
			meta:       rep(health.StatusHealthy, ""),
			block:      rep(health.StatusUnhealthy, "local: fs block store is closed"),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusUnhealthy,
			wantPrefix: "block: local: fs block store is closed",
		},
		{
			name:       "block degraded surfaces share as degraded",
			meta:       rep(health.StatusHealthy, ""),
			block:      rep(health.StatusDegraded, "remote unreachable: connection refused"),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusDegraded,
			wantPrefix: "block: remote unreachable",
		},
		{
			name:       "unhealthy beats degraded",
			meta:       rep(health.StatusDegraded, "slow ping"),
			block:      rep(health.StatusUnhealthy, "local: closed"),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusUnhealthy,
			wantPrefix: "block:",
		},
		{
			name:       "unknown beats healthy",
			meta:       rep(health.StatusUnknown, "context canceled"),
			block:      rep(health.StatusHealthy, ""),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusUnknown,
			wantPrefix: "metadata:",
		},
		{
			name:       "tie on unhealthy: metadata wins (more impactful)",
			meta:       rep(health.StatusUnhealthy, "ping failed"),
			block:      rep(health.StatusUnhealthy, "fs broken"),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusUnhealthy,
			wantPrefix: "metadata:",
		},
		{
			name:       "metadata-only share (no block)",
			meta:       rep(health.StatusHealthy, ""),
			hasMeta:    true,
			hasBlock:   false,
			wantStatus: health.StatusHealthy,
		},
		{
			name:       "block-only share (no metadata)",
			block:      rep(health.StatusDegraded, "remote down"),
			hasMeta:    false,
			hasBlock:   true,
			wantStatus: health.StatusDegraded,
			wantPrefix: "block:",
		},
		{
			name:       "neither side present",
			hasMeta:    false,
			hasBlock:   false,
			wantStatus: health.StatusUnknown,
			wantPrefix: "share has neither",
		},
		{
			name:       "non-healthy with empty message gets a synthesised one",
			meta:       rep(health.StatusUnhealthy, ""),
			block:      rep(health.StatusHealthy, ""),
			hasMeta:    true,
			hasBlock:   true,
			wantStatus: health.StatusUnhealthy,
			wantPrefix: "metadata: unhealthy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := combineShareReports(tc.meta, tc.block, tc.hasMeta, tc.hasBlock)
			if got.Status != tc.wantStatus {
				t.Fatalf("status: got %q, want %q", got.Status, tc.wantStatus)
			}
			if tc.wantPrefix != "" && !strings.HasPrefix(got.Message, tc.wantPrefix) {
				t.Fatalf("message: got %q, want prefix %q", got.Message, tc.wantPrefix)
			}
		})
	}
}

// TestShareHealthcheck_HealthyWithMetaOnly exercises the wrapper for
// the metadata-only edge case: a share with no block store should
// report purely on the metadata store. Uses the real in-memory
// metadata store implementation to keep the test free of fakes.
func TestShareHealthcheck_HealthyWithMetaOnly(t *testing.T) {
	share := &Share{Name: "test", BlockStore: nil}
	meta := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	defer func() { _ = meta.Close() }()

	rep := share.Healthcheck(context.Background(), meta)
	if rep.Status != health.StatusHealthy {
		t.Fatalf("meta-only share with healthy meta: got %q (%q), want healthy", rep.Status, rep.Message)
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("CheckedAt should be populated by the wrapper")
	}
}

// TestShareHealthcheck_RespectsCanceledContext verifies the wrapper
// short-circuits on a canceled caller context before either sub-probe
// runs.
func TestShareHealthcheck_RespectsCanceledContext(t *testing.T) {
	share := &Share{Name: "test"}
	meta := metadatamemory.NewMemoryMetadataStoreWithDefaults()
	defer func() { _ = meta.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rep := share.Healthcheck(ctx, meta)
	if rep.Status != health.StatusUnknown {
		t.Fatalf("canceled ctx: got %q (%q), want unknown", rep.Status, rep.Message)
	}
}

// TestShareHealthcheck_NeitherSidePresent locks the degenerate case
// where the share somehow ends up with neither a metadata store nor a
// block store — combineShareReports falls through to its
// "neither side present" branch.
func TestShareHealthcheck_NeitherSidePresent(t *testing.T) {
	share := &Share{Name: "test", BlockStore: nil}

	rep := share.Healthcheck(context.Background(), nil)
	if rep.Status != health.StatusUnknown {
		t.Fatalf("neither side: got %q (%q), want unknown", rep.Status, rep.Message)
	}
	if !strings.Contains(rep.Message, "neither") {
		t.Fatalf("neither side: message should explain the absence; got %q", rep.Message)
	}
}
