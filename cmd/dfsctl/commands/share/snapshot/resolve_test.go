package snapshot

import (
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/apiclient"
)

func TestResolveSnapshotID_UniquePrefix(t *testing.T) {
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"abc12345-aaaa": {ID: "abc12345-aaaa"},
		"def67890-bbbb": {ID: "def67890-bbbb"},
	}}
	got, err := resolveSnapshotID(fc, "/a", "abc12345")
	if err != nil {
		t.Fatalf("resolveSnapshotID: %v", err)
	}
	if got != "abc12345-aaaa" {
		t.Fatalf("got %q, want abc12345-aaaa", got)
	}
}

func TestResolveSnapshotID_ExactMatchWins(t *testing.T) {
	// One id is a prefix of another; the exact match must short-circuit
	// instead of reporting ambiguity.
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"abc":     {ID: "abc"},
		"abc1234": {ID: "abc1234"},
	}}
	got, err := resolveSnapshotID(fc, "/a", "abc")
	if err != nil {
		t.Fatalf("resolveSnapshotID: %v", err)
	}
	if got != "abc" {
		t.Fatalf("got %q, want abc", got)
	}
}

func TestResolveSnapshotID_Ambiguous(t *testing.T) {
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"abc111": {ID: "abc111"},
		"abc222": {ID: "abc222"},
	}}
	_, err := resolveSnapshotID(fc, "/a", "abc")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("err = %v, want ambiguous error", err)
	}
}

func TestResolveSnapshotID_NotFound(t *testing.T) {
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"abc111": {ID: "abc111"},
	}}
	_, err := resolveSnapshotID(fc, "/a", "zzz")
	if err == nil || !strings.Contains(err.Error(), "no snapshot") {
		t.Fatalf("err = %v, want not-found error", err)
	}
}

// TestDelete_ResolvesPartialID asserts delete resolves an 8-char prefix to
// the full UUID before calling DeleteSnapshot.
func TestDelete_ResolvesPartialID(t *testing.T) {
	resetDeleteFlags()
	deleteYes = true
	fc := &fakeClient{snapshots: map[string]*apiclient.Snapshot{
		"abc12345-full-uuid": {ID: "abc12345-full-uuid"},
	}}
	withFakeClient(t, fc)

	prev := osStdout()
	_, w := setStdout()
	defer restoreStdout(prev)

	if err := runDelete(deleteCmd, []string{"/a", "abc12345"}); err != nil {
		t.Fatalf("runDelete: %v", err)
	}
	_ = w.Close()

	if len(fc.deleteCalls) != 1 || fc.deleteCalls[0] != "abc12345-full-uuid" {
		t.Fatalf("DeleteSnapshot called with %v, want [abc12345-full-uuid]", fc.deleteCalls)
	}
}
