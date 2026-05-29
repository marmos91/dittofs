package snapshot

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
)

// resetListFlags resets the leaf-level vars between tests.
func resetListFlags() {
	listState = ""
	listNamePrefix = ""
	listNoRelative = false
}

func TestList_EmptyTable(t *testing.T) {
	resetListFlags()
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{}}
	withFakeClient(t, fc)
	cmdutil.Flags.Output = "table"

	// Redirect stdout by running command and capturing — list writes via
	// fmt.Printf to os.Stdout. The empty-list branch is exercised here.
	if err := runList(listCmd, []string{"/archive"}); err != nil {
		t.Fatalf("runList: %v", err)
	}
}

func TestList_InvalidStateErrors(t *testing.T) {
	resetListFlags()
	listState = "redy" // typo
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{}}
	withFakeClient(t, fc)
	cmdutil.Flags.Output = "table"

	err := runList(listCmd, []string{"/archive"})
	if err == nil || !strings.Contains(err.Error(), "invalid --state") {
		t.Fatalf("err = %v, want invalid --state error", err)
	}
}

func TestList_EmptyMessageReflectsFilters(t *testing.T) {
	resetListFlags()
	listState = "ready"
	listNamePrefix = "wk"
	// A creating snapshot exists but the ready+wk filter excludes it.
	fc := &fakeClient{listOverride: []apiclient.Snapshot{
		{ID: "x", State: "creating", CreatedAt: time.Now()},
	}}
	withFakeClient(t, fc)

	got := emptyListMessage("/archive", listState, listNamePrefix)
	if !strings.Contains(got, "state=ready") || !strings.Contains(got, "name-prefix=wk") {
		t.Fatalf("empty message %q does not reflect active filters", got)
	}
	// No-filter case keeps the plain message.
	plain := emptyListMessage("/archive", "", "")
	if strings.Contains(plain, "matching") {
		t.Fatalf("plain message unexpectedly mentions filters: %q", plain)
	}
}

func TestList_FilterByState(t *testing.T) {
	resetListFlags()
	listState = "ready"
	fc := &fakeClient{listOverride: []apiclient.Snapshot{
		{ID: "a1234567xxx", Name: "n1", State: "ready", CreatedAt: time.Now()},
		{ID: "b1234567xxx", Name: "n2", State: "creating", CreatedAt: time.Now()},
	}}

	filtered := applyFilters(fc.listOverride, listState, "")
	if len(filtered) != 1 || filtered[0].State != "ready" {
		t.Fatalf("state filter: got %+v", filtered)
	}
}

func TestList_FilterByNamePrefix(t *testing.T) {
	resetListFlags()
	listNamePrefix = "weekly"
	snaps := []apiclient.Snapshot{
		{ID: "1", Name: "weekly-a", State: "ready", CreatedAt: time.Now()},
		{ID: "2", Name: "daily-b", State: "ready", CreatedAt: time.Now()},
		{ID: "3", Name: "weekly-c", State: "ready", CreatedAt: time.Now()},
	}
	filtered := applyFilters(snaps, "", listNamePrefix)
	if len(filtered) != 2 {
		t.Fatalf("name-prefix filter: got %d, want 2", len(filtered))
	}
}

func TestList_NewestFirstSort(t *testing.T) {
	older := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-1 * time.Hour)
	snaps := []apiclient.Snapshot{
		{ID: "old", State: "ready", CreatedAt: older},
		{ID: "new", State: "ready", CreatedAt: newer},
	}
	out := applyFilters(snaps, "", "")
	if out[0].ID != "new" {
		t.Fatalf("expected newest-first; got %s first", out[0].ID)
	}
}

func TestList_TruncID(t *testing.T) {
	if got := truncID("abc12345xyz"); got != "abc12345" {
		t.Errorf("truncID('abc12345xyz') = %q, want 'abc12345'", got)
	}
	if got := truncID("short"); got != "short" {
		t.Errorf("truncID('short') = %q, want 'short'", got)
	}
}

func TestList_HeadersAreSixColumns(t *testing.T) {
	hdrs := SnapshotList{}.Headers()
	want := []string{"ID", "NAME", "STATE", "DURABLE", "CREATED", "SIZE"}
	if len(hdrs) != 6 {
		t.Fatalf("want 6 columns, got %d", len(hdrs))
	}
	for i, h := range hdrs {
		if h != want[i] {
			t.Errorf("header[%d]=%q want %q", i, h, want[i])
		}
	}
}

func TestList_JSONOutput(t *testing.T) {
	resetListFlags()
	cmdutil.Flags.Output = "json"
	defer func() { cmdutil.Flags.Output = "table" }()

	snaps := []apiclient.Snapshot{
		{ID: "snap-1", Name: "x", Share: "/a", State: "ready"},
	}
	fc := &fakeClient{listOverride: snaps}
	withFakeClient(t, fc)

	// Redirect stdout via os.Pipe.
	old := osStdout()
	r, w := setStdout()
	defer restoreStdout(old)

	if err := runList(listCmd, []string{"/a"}); err != nil {
		t.Fatalf("runList: %v", err)
	}
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(r)

	var got []apiclient.Snapshot
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("json decode: %v\noutput: %s", err, buf.String())
	}
	if len(got) != 1 || got[0].ID != "snap-1" {
		t.Errorf("json output mismatch: %s", buf.String())
	}
}

func TestList_FormatCreated_Relative(t *testing.T) {
	now := time.Now()
	if got := formatCreated(now.Add(-30*time.Second), false); !strings.Contains(got, "s ago") {
		t.Errorf("formatCreated(30s) = %q", got)
	}
	if got := formatCreated(now.Add(-5*time.Hour), false); !strings.Contains(got, "h ago") {
		t.Errorf("formatCreated(5h) = %q", got)
	}
}
