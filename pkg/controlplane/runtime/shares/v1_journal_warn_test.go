package shares

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cubbit/dittofs/internal/logger"
	"github.com/cubbit/dittofs/pkg/metadata"
	"github.com/cubbit/dittofs/pkg/metadata/store/badger/badgertest"
)

// TestAddShare_WarnsAboutAPreviousEngineJournal pins the boot warning: a
// share whose state directory still holds the previous engine's journal is
// served, but its start says, once, that the journal's content is not
// readable by this build. A share without one starts quietly.
func TestAddShare_WarnsAboutAPreviousEngineJournal(t *testing.T) {
	buf := &bytes.Buffer{}
	logger.InitWithWriter(buf, "WARN", "text", false)
	t.Cleanup(func() { logger.InitWithWriter(&bytes.Buffer{}, "ERROR", "text", false) })

	svc := New()
	store := badgertest.NewInMemory(t)
	t.Cleanup(func() { _ = store.Close() })
	defaults := journalDefaults(t, svc)

	old := filepath.Join(ShareJournalDir(defaults.JournalRoot, "/old"), v1JournalDir)
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "00000001.seg"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"/old", "/new"} {
		cfg := &ShareConfig{Name: name, MetadataStore: "m", Enabled: true, BlockStoreID: testBlockStoreID}
		if err := svc.AddShare(context.Background(), cfg, fixedStoreProvider{store: store},
			metadata.New(), memBlockStoreProvider{}, defaults); err != nil {
			t.Fatalf("AddShare %s: %v", name, err)
		}
	}

	out := buf.String()
	if n := strings.Count(out, "journal from the previous engine"); n != 1 {
		t.Fatalf("previous-engine journal warnings = %d, want 1 (for /old only):\n%s", n, out)
	}
	if !strings.Contains(out, old) {
		t.Errorf("the warning does not name the directory %s:\n%s", old, out)
	}
}
