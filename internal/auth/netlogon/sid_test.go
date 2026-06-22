package netlogon

import "testing"

func TestSIDFromRID(t *testing.T) {
	got, err := SIDFromRID("S-1-5-21-1004336348-1177238915-682003330", 1103)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "S-1-5-21-1004336348-1177238915-682003330-1103"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestSIDFromRIDRejectsMalformed(t *testing.T) {
	if _, err := SIDFromRID("not-a-sid", 1103); err == nil {
		t.Fatal("expected error for malformed domain SID")
	}
}
