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

func TestValidateExcludePatterns(t *testing.T) {
	if err := ValidateExcludePatterns(nil); err != nil {
		t.Errorf("nil patterns: unexpected error %v", err)
	}
	if err := ValidateExcludePatterns([]string{"*.tmp", "~$*", "report-[0-9].log"}); err != nil {
		t.Errorf("valid patterns: unexpected error %v", err)
	}
	// path.Match returns ErrBadPattern for an unterminated character class.
	if err := ValidateExcludePatterns([]string{"*.tmp", "bad["}); err == nil {
		t.Error("expected error for malformed glob \"bad[\", got nil")
	}
}

func TestRecycleDirNameIsReserved(t *testing.T) {
	if RecycleDirName != "#recycle" {
		t.Fatalf("RecycleDirName = %q, want #recycle", RecycleDirName)
	}
}
