package metadata

import "testing"

func TestExcludedByPatterns(t *testing.T) {
	pol := TrashConfig{ExcludePatterns: []string{"*.tmp", "~$*"}}
	cases := map[string]bool{
		"report.tmp": true,
		"~$doc.docx": true,
		"keep.txt":   false,
		"#recycle":   false,
	}
	for name, want := range cases {
		if got := pol.Excluded(name); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRecycleDirNameIsReserved(t *testing.T) {
	if RecycleDirName != "#recycle" {
		t.Fatalf("RecycleDirName = %q, want #recycle", RecycleDirName)
	}
}
