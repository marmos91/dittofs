package plan

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/harness/registry"
)

func repo(t *testing.T) *registry.Registry {
	t.Helper()
	r, err := registry.Load("../../..")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func names(cells []Cell) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = c.Name
	}
	return out
}

// The pr tier is today's pull-request checks, with the conformance suites on
// the production profile, badger-s3, only. With no change given, the Kerberos
// e2e run, which has paths, is planned too.
func TestPRTier(t *testing.T) {
	cells, err := Plan("../../..", repo(t), "pr", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"lint-go", "lint-repo", "shellcheck",
		"unit", "unit/windows",
		"integration", "integration/portmap", "integration/kerberos",
		"operator",
		"e2e/kerberos",
		"pjdfstest/badger-s3/3", "pjdfstest/badger-s3/4", "pjdfstest/badger-s3/4.1",
		"pynfs/badger-s3/0", "pynfs/badger-s3/1",
		"wpts/badger-s3", "smbtorture/badger-s3",
		"bench-smoke",
	}
	if got := names(cells); !slices.Equal(got, want) {
		t.Fatalf("pr cells:\n got %v\nwant %v", got, want)
	}
	for _, c := range cells {
		nonBlocking := c.Name == "unit/windows" || c.Name == "bench-smoke"
		if c.Blocking == nonBlocking {
			t.Errorf("%s: blocking %t", c.Name, c.Blocking)
		}
	}
}

// Each tier contains the ones before it, and manual runs only when asked.
func TestTiersContainTheOnesBefore(t *testing.T) {
	r := repo(t)
	var prev []string
	for _, tier := range registry.Tiers {
		cells, err := Plan("../../..", r, tier, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := names(cells)
		for _, n := range prev {
			if !slices.Contains(got, n) {
				t.Errorf("%s lacks %s from the tier before it", tier, n)
			}
		}
		if hasKnfsd := slices.Contains(got, "pynfs/knfsd/0"); hasKnfsd != (tier == "manual") {
			t.Errorf("%s: pynfs/knfsd planned %t", tier, hasKnfsd)
		}
		prev = got
	}
}

// Shards deal a group's tests in turn; needs_by_test sends its tests to
// a cell of their own on the place that meets them, unsharded; a cell's limit
// covers the sum of its tests' limits plus the fixture.
func TestSplitsShardsAndTimeouts(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"00-a-xs.sh", "01-b-s.sh", "02-c-m.sh", "90-d-xl.sh", "91-e-xxl.sh"} {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	y := `version: 1
defaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }
suites:
  system:
    cmd: run {test}
    tests: "[0-9][0-9]-*.sh"
    select: { merge: "[0-8]?-*.sh" }
    needs: [linux, podman]
    needs_by_test: { "9?-*.sh": ["host:dedicated"] }
    timeout: { test: size, cell: 90m }
    shards: 2
`
	r, err := registry.Parse(root, []byte(y))
	if err != nil {
		t.Fatal(err)
	}

	merge, err := Plan(root, r, "merge", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(merge); !slices.Equal(got, []string{"system/shard-1", "system/shard-2"}) {
		t.Fatalf("merge cells %v", got)
	}
	if got := merge[0].Tests; !slices.Equal(got, []string{"00-a-xs.sh", "02-c-m.sh"}) {
		t.Errorf("shard 1 tests %v: dealt in turn", got)
	}

	nightly, err := Plan(root, r, "nightly", nil)
	if err != nil {
		t.Fatal(err)
	}
	last := nightly[len(nightly)-1]
	if last.Name != "system/dedicated" || last.Place != Dedicated || len(last.Tests) != 2 {
		t.Fatalf("needs_by_test cell %+v", last)
	}
	// 90-d-xl's 122 minutes, 91-e-xxl counted as the cell's 90, and 5 for the fixture.
	if last.TimeoutMinutes != 217 || len(last.Notes) != 1 {
		t.Errorf("dedicated cell: %d minutes, notes %v", last.TimeoutMinutes, last.Notes)
	}
	if merge[0].TimeoutMinutes != 90 {
		t.Errorf("shard limit %d: the cell's 90m covers its tests", merge[0].TimeoutMinutes)
	}
	// The needs a glob adds count only on a tier that plans its tests.
	for tier, want := range map[string]int{"merge": 0, "nightly": 1, "": 1} {
		got, err := NeedsByTest(root, r.Units(r.Suites[0])[0], tier)
		if err != nil || len(got) != want || want == 1 && !slices.Equal(got["9?-*.sh"], []string{"host:dedicated"}) {
			t.Errorf("tier %q: needs by test %v, %v", tier, got, err)
		}
	}
}

// A fixture adds to each cell's needs what its fields imply there: a service
// for an S3 profile, and root and the client for a mount, but nothing for an
// export without one.
func TestFixtureNeeds(t *testing.T) {
	root := t.TempDir()
	y := `version: 1
defaults: { format: exit, tier: merge, timeout: { test: 30m, cell: 60m } }
suites:
  a:
    cmd: "true"
    matrix: { profile: [memory, badger-s3], nfs: ["export", "4.1"] }
    fixture: { profile: "{profile}", nfs: "{nfs}" }
    needs: [linux, root]
  b: { cmd: "true", fixture: { profile: memory, smb: { mount: true } } }
`
	r, err := registry.Parse(root, []byte(y))
	if err != nil {
		t.Fatal(err)
	}
	cells, err := Plan(root, r, "merge", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"a/memory/export":    {"linux", "root"},
		"a/memory/4.1":       {"linux", "root", "nfs-client"},
		"a/badger-s3/export": {"linux", "root", "service:localstack"},
		"a/badger-s3/4.1":    {"linux", "root", "service:localstack", "nfs-client"},
		"b":                  {"root", "smb-client"},
	}
	for _, c := range cells {
		if !slices.Equal(c.Needs, want[c.Name]) {
			t.Errorf("%s: needs %v, want %v", c.Name, c.Needs, want[c.Name])
		}
	}
	if len(cells) != len(want) {
		t.Errorf("cells %v", names(cells))
	}
}

func TestPlaceOf(t *testing.T) {
	cases := map[string][]string{
		Ubuntu:    {"linux", "root", "disk:10"},
		Windows:   {"windows"},
		MacOS:     {"macos"},
		Dedicated: {"linux", "disk:110"},
	}
	for want, needs := range cases {
		if got := placeOf(needs); got != want {
			t.Errorf("placeOf(%v) = %s, want %s", needs, got, want)
		}
	}
	if got := placeOf([]string{"host:dedicated"}); got != Dedicated {
		t.Errorf("host:dedicated goes to %s", got)
	}
}

func TestSizeLimit(t *testing.T) {
	cases := map[string]time.Duration{
		"test/scenarios/00-smb-x-xs.sh": 2*10*time.Second + 2*time.Minute,
		"57-smb-x-l.sh":                 32 * time.Minute,
		"92-smb-x-xl.sh":                2*time.Hour + 2*time.Minute,
		"90-smb-x-xxl.sh":               0,
	}
	for name, want := range cases {
		if got := sizeLimit(name); got != want {
			t.Errorf("sizeLimit(%s) = %s, want %s", name, got, want)
		}
	}
}

func TestUnknownTier(t *testing.T) {
	if _, err := Plan("../../..", repo(t), "weekly", nil); err == nil || !strings.Contains(err.Error(), "weekly") {
		t.Fatalf("got %v", err)
	}
}

// On the pr tier a docs-only change plans nothing, and a suite with paths runs
// only when the change touches one of them.
func TestChangedPaths(t *testing.T) {
	r := repo(t)
	plan := func(changed ...string) []string {
		t.Helper()
		cells, err := Plan("../../..", r, "pr", changed)
		if err != nil {
			t.Fatal(err)
		}
		return names(cells)
	}
	if got := plan("docs/guide/x.md", "README.md", "test/e2e/README.md"); len(got) != 0 {
		t.Errorf("docs-only change planned %v", got)
	}
	if got := plan("pkg/block/engine/flush.go"); slices.Contains(got, "e2e/kerberos") || !slices.Contains(got, "unit") {
		t.Errorf("a block change: %v", got)
	}
	for _, p := range []string{
		"internal/adapter/smb/handlers/session_setup.go", "pkg/metadata/file.go", "test/e2e/smb_kerberos_test.go",
		"pkg/controlplane/runtime/export_auth_policy.go", "internal/controlplane/api/handlers/identity_mappings.go",
		"pkg/config/config.go", "cmd/dfs/commands/start.go",
	} {
		if got := plan(p); !slices.Contains(got, "e2e/kerberos") {
			t.Errorf("a change to %s doesn't plan e2e/kerberos", p)
		}
	}
	// Other tiers ignore the change: everything runs after a merge.
	cells, err := Plan("../../..", r, "merge", []string{"docs/x.md"})
	if err != nil || len(cells) == 0 {
		t.Errorf("merge with a docs-only change: %d cells, %v", len(cells), err)
	}
}

// A .txt test list may give a test its own limit, which the cell covers.
func TestTestListLimits(t *testing.T) {
	root := t.TempDir()
	list := "# a comment\nslow 40m\nquick\n"
	if err := os.WriteFile(filepath.Join(root, "tests.txt"), []byte(list), 0o644); err != nil {
		t.Fatal(err)
	}
	y := `version: 1
defaults: { format: exit, tier: merge, timeout: { test: 10m, cell: 30m } }
suites:
  s: { cmd: "run {test}", tests: tests.txt, shards: 2 }
`
	r, err := registry.Parse(root, []byte(y))
	if err != nil {
		t.Fatal(err)
	}
	cells, err := Plan(root, r, "merge", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cells) != 2 || !slices.Equal(cells[0].Tests, []string{"slow"}) {
		t.Fatalf("cells %+v", cells)
	}
	// slow: its own 40m plus 5m for the fixture; quick: the suite's 10m fits in 30m.
	if cells[0].TimeoutMinutes != 45 || cells[1].TimeoutMinutes != 30 {
		t.Errorf("limits %d and %d minutes, want 45 and 30", cells[0].TimeoutMinutes, cells[1].TimeoutMinutes)
	}

	if err := os.WriteFile(filepath.Join(root, "tests.txt"), []byte("slow forty\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Plan(root, r, "merge", nil); err == nil || !strings.Contains(err.Error(), "tests.txt:1") {
		t.Errorf("a bad limit: %v", err)
	}
}

func TestShort(t *testing.T) {
	cases := map[time.Duration]string{
		10 * time.Minute:            "10m",
		2*time.Hour + 7*time.Minute: "2h7m",
		2 * time.Hour:               "2h",
		90 * time.Minute:            "1h30m",
		5 * time.Minute:             "5m",
	}
	for d, want := range cases {
		if got := short(d); got != want {
			t.Errorf("short(%s) = %q, want %q", d, got, want)
		}
	}
}

// A shard left with no tests is dropped, since a cell listing none runs the
// whole suite.
func TestNoEmptyShards(t *testing.T) {
	root := t.TempDir()
	for _, f := range []string{"a.sh", "b.sh"} {
		if err := os.WriteFile(filepath.Join(root, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	y := "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 1m, cell: 10m } }\nsuites:\n  s: { cmd: \"run {test}\", tests: \"*.sh\", shards: 4 }\n"
	r, err := registry.Parse(root, []byte(y))
	if err != nil {
		t.Fatal(err)
	}
	cells, err := Plan(root, r, "pr", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := names(cells); !slices.Equal(got, []string{"s/shard-1", "s/shard-2"}) {
		t.Fatalf("cells %v", got)
	}
}

// A test list that comes out empty is an error, not a suite with no cells.
func TestEmptyTestListIsAnError(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.sh"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, suite := range map[string]string{
		"glob":   `{ cmd: "run {test}", tests: "nope/*.sh", shards: 2 }`,
		"select": `{ cmd: "run {test}", tests: "*.sh", select: { pr: "typo-*.sh" } }`,
	} {
		y := "version: 1\ndefaults: { format: exit, tier: pr, timeout: { test: 1m, cell: 10m } }\nsuites:\n  s: " + suite + "\n"
		r, err := registry.Parse(root, []byte(y))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Plan(root, r, "pr", nil); err == nil || !strings.Contains(err.Error(), "no test to plan") {
			t.Errorf("%s: got %v", name, err)
		}
	}
}
