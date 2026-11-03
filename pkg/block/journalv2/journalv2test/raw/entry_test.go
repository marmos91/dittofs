package raw

import "testing"

// Offsets below are the catalog entry table's, written out here and never
// taken from the package's constants.

// TestCatalogEntryLayout pins every field of a catalog entry at its offset and
// size.
func TestCatalogEntryLayout(t *testing.T) {
	b := testCatalogEntry.Bytes()
	if len(b) != 72 {
		t.Fatalf("a catalog entry is %d bytes, want 72", len(b))
	}

	if got := [16]byte(b[0:16]); got != testCatalogEntry.FileID {
		t.Errorf("FileID at [0,16) is % x, want % x", got, testCatalogEntry.FileID)
	}

	if got := le64(b[16:24]); got != testCatalogEntry.Offset {
		t.Errorf("file offset at [16,24) is %#x, want %#x", got, testCatalogEntry.Offset)
	}

	if got := le32(b[24:28]); got != testCatalogEntry.RecordOffset {
		t.Errorf("recordOffset at [24,28) is 0x%08x, want 0x%08x", got, testCatalogEntry.RecordOffset)
	}

	if got := le64(b[28:36]); got != testCatalogEntry.Length {
		t.Errorf("length at [28,36) is %#x, want %#x", got, testCatalogEntry.Length)
	}

	if got := le64(b[36:44]); got != testCatalogEntry.Seq {
		t.Errorf("sequence number at [36,44) is %#x, want %#x", got, testCatalogEntry.Seq)
	}

	if got := le64(b[44:52]); got != testCatalogEntry.Version.Epoch {
		t.Errorf("version epoch at [44,52) is %#x, want %#x", got, testCatalogEntry.Version.Epoch)
	}

	if got := le64(b[52:60]); got != testCatalogEntry.Version.Counter {
		t.Errorf("version counter at [52,60) is %#x, want %#x", got, testCatalogEntry.Version.Counter)
	}

	if got := b[60]; got != testCatalogEntry.Kind {
		t.Errorf("kind at [60] is %#x, want %#x", got, testCatalogEntry.Kind)
	}

	if !zero(b[61:64]) {
		t.Errorf("reserved [61,64) is % x, want zeros", b[61:64])
	}

	if got := le32(b[64:68]); got != testCatalogEntry.Tag {
		t.Errorf("share tag at [64,68) is 0x%08x, want 0x%08x", got, testCatalogEntry.Tag)
	}

	if !zero(b[68:72]) {
		t.Errorf("reserved [68,72) is % x, want zeros", b[68:72])
	}
}
