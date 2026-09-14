package config

import (
	"strings"
	"testing"
)

func TestCheckRenamedKeys_NamesBothHalves(t *testing.T) {
	err := checkRenamedKeys([]string{"blockstore.local.max_log_bytes"})
	if err == nil {
		t.Fatal("checkRenamedKeys: got nil, want an error for a renamed key")
	}
	for _, want := range []string{"blockstore.local.max_log_bytes", "blockstore.journal.max_log_bytes"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestCheckRenamedKeys_MatchesTheSectionItself(t *testing.T) {
	if err := checkRenamedKeys([]string{"blockstore.local"}); err == nil {
		t.Error("checkRenamedKeys: got nil, want an error for the bare renamed section")
	}
}

// Viper lowercases keys, but the decoder reports them as the file spelled
// them; a config written in mixed case must still be caught.
func TestCheckRenamedKeys_IsCaseInsensitive(t *testing.T) {
	if err := checkRenamedKeys([]string{"Blockstore.Local.MaxLogBytes"}); err == nil {
		t.Error("checkRenamedKeys: got nil, want an error regardless of case")
	}
}

// A key from a section that was deleted outright keeps the old warn-and-ignore
// behaviour, so an upgrade does not hard-fail on it.
func TestCheckRenamedKeys_IgnoresUnrelatedUnknownKeys(t *testing.T) {
	err := checkRenamedKeys([]string{"lock.timeout", "syncer.workers", "blockstore.journal.path"})
	if err != nil {
		t.Errorf("checkRenamedKeys: got %v, want nil for keys that were not renamed", err)
	}
}

// The prefix must not match a longer sibling section that merely starts with
// the same letters.
func TestCheckRenamedKeys_DoesNotMatchSiblingPrefix(t *testing.T) {
	if err := checkRenamedKeys([]string{"blockstore.locality"}); err != nil {
		t.Errorf("checkRenamedKeys: got %v, want nil for a sibling section", err)
	}
}

// End-to-end proof that the old key actually reaches the decoder's unused set:
// a unit test on checkRenamedKeys alone would still pass if nested unknown keys
// never got reported, leaving the guard unable to fire.
func TestLoad_RefusesRenamedBlockstoreLocalSection(t *testing.T) {
	content := `
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
blockstore:
  local:
    max_log_bytes: 3221225472
`
	_, err := Load(writeConfigFile(t, content))
	if err == nil {
		t.Fatal("Load: got nil, want a refusal for the renamed blockstore.local section")
	}
	// mapstructure reports the whole unplaced subtree, so the message names the
	// section rather than the leaf — which is the more useful thing to tell an
	// operator anyway.
	for _, want := range []string{"blockstore.local", "blockstore.journal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load error %q must name %q", err, want)
		}
	}
}

// The same file under the new name must load cleanly — otherwise the guard is
// rejecting everything and the test above proves nothing.
func TestLoad_AcceptsRenamedBlockstoreJournalSection(t *testing.T) {
	content := `
controlplane:
  jwt:
    secret: "test-secret-key-for-testing-minimum-32-chars"
blockstore:
  journal:
    max_log_bytes: 3221225472
`
	cfg, err := Load(writeConfigFile(t, content))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Blockstore.Journal.MaxLogBytes != 3<<30 {
		t.Errorf("MaxLogBytes: got %d, want %d", cfg.Blockstore.Journal.MaxLogBytes, 3<<30)
	}
}
