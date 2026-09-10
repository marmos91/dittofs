package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every case runs against the vendored maps, so a case also pins the map entry
// it names. The "want no finding" half matters most: those are the shapes that
// would make this check noisy enough to be turned off.
func TestCheckLine(t *testing.T) {
	specs, err := loadSpecs()
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}

	for _, tc := range []struct {
		name string
		line string
		want string // substring of the expected message; "" means no finding
	}{{
		name: "section absent from the spec",
		line: `// Per [MS-FSA] 2.1.5.14.3, a directory marked for deletion completes notifies`,
		want: "2.1.5.14.3 does not exist",
	}, {
		name: "quoted title disagrees with the cited section",
		line: `// Per MS-FSA §2.1.5.15.2 ("FileRenameInformation"): timestamps`,
		want: `2.1.5.15.2 is "FileBasicInformation", not "FileRenameInformation"`,
	}, {
		name: "quoted title agrees",
		line: `// Per MS-FSA §2.1.5.15.2 ("FileBasicInformation"): timestamps`,
	}, {
		name: "named structure is defined by a different section",
		line: `FileStandardInformation FileInfoClass = 5 // [MS-FSCC] 2.4.41`,
		want: `2.4.41 is "FileQuotaInformation", but the line names FileStandardInformation, which is MS-FSCC 2.4.47`,
	}, {
		name: "named structure matches the cited section",
		line: `FileStandardInformation FileInfoClass = 5 // [MS-FSCC] 2.4.47`,
	}, {
		name: "underscore spelling of the same structure",
		line: `// FILE_FULL_EA_INFORMATION chain (MS-FSCC §2.4.16)`,
	}, {
		name: "Ex-suffixed class is a structure in its own right",
		line: `// FileDispositionInformationEx per [MS-FSCC] 2.4.11`,
		want: `2.4.11 is "FileDispositionInformation", but the line names FileDispositionInformationEx, which is MS-FSCC 2.4.12`,
	}, {
		// The FSCTL sections come in Request/Reply pairs, and citing either
		// one from a line naming the FSCTL is correct.
		name: "FSCTL reply section satisfies a line naming the FSCTL",
		line: `// FSCTL_QUERY_ALLOCATED_RANGES reply buffer, MS-FSCC §2.3.52`,
	}, {
		name: "FSCTL cited at an unrelated structure",
		line: `// FSCTL_SET_ZERO_DATA request buffer, MS-FSCC §2.3.25`,
		want: "but the line names FSCTL_SET_ZERO_DATA",
	}, {
		// Citing an algorithm section from a line that names a structure is
		// how behaviour gets attributed; the structure name is not a claim
		// about which section number belongs there.
		name: "prose-titled section is not second-guessed",
		line: `// FileRenameInformation share-delete check per MS-FSA §2.1.5.1.2.1`,
	}, {
		name: "two structures named on one line stay ambiguous",
		line: `// converts FileStandardInformation into FileNetworkOpenInformation, MS-FSCC §2.4.41`,
	}, {
		name: "two structural citations on one line stay ambiguous",
		line: `// FileStandardInformation, MS-FSCC §2.4.41 and MS-FSA §2.1.5.15.2`,
	}, {
		name: "family reference carries no structure claim",
		line: `// Validate FileAttributes per MS-FSCC §2.6 and MS-SMB2 §2.2.13`,
	}, {
		name: "byte-size parenthetical is not a title",
		line: `// FileEaInformation (MS-FSCC §2.4.13) (4 bytes): total EA size`,
	}, {
		name: "MS-SMB2 prose title is checked when quoted",
		line: `// Per MS-SMB2 §3.3.5.9 ("Receiving an SMB2 WRITE Request")`,
		want: `3.3.5.9 is "Receiving an SMB2 CREATE Request"`,
	}, {
		// Comments quote spec prose in the same position a title goes; a
		// title never carries arithmetic.
		name: "quoted spec prose is not a title claim",
		line: `// per MS-SMB2 §3.3.4.7 ("NewEpoch = Epoch + 1 ... Epoch = Epoch + 1")`,
	}, {
		name: "no citation at all",
		line: `// FileStandardInformation is the class the client asked for`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := checkLine(specs, tc.line)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("want no finding, got %q", got[0].msg)
				}
				return
			}
			if len(got) != 1 {
				t.Fatalf("want 1 finding containing %q, got %d: %v", tc.want, len(got), got)
			}
			if !strings.Contains(got[0].msg, tc.want) {
				t.Fatalf("got %q, want it to contain %q", got[0].msg, tc.want)
			}
		})
	}
}

// Comments wrap at column 80, and a citation split across the wrap was
// invisible to a line-at-a-time scan.
func TestLogicalLine(t *testing.T) {
	specs, err := loadSpecs()
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}
	lines := []string{
		"// handleQueryAllocatedRanges handles FSCTL_QUERY_ALLOCATED_RANGES [MS-FSCC]",
		"// 2.3.32. The client supplies a (FileOffset, Length) window.",
	}
	if got := checkLine(specs, lines[0]); len(got) != 0 {
		t.Fatalf("unjoined first line should carry no citation, got %q", got[0].msg)
	}
	got := checkLine(specs, logicalLine(lines, 0))
	if len(got) != 1 || !strings.Contains(got[0].msg, "the line names FSCTL_QUERY_ALLOCATED_RANGES") {
		t.Fatalf("want the wrapped citation flagged, got %v", got)
	}
	if got := logicalLine(lines, 1); got != lines[1] {
		t.Errorf("a line that ends nothing must be returned unchanged, got %q", got)
	}
}

// The maps are the only thing standing between a correct citation and a false
// failure, so a map that lost entries has to be loud.
func TestSectionMapsArePopulated(t *testing.T) {
	specs, err := loadSpecs()
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}
	minimum := map[string]int{
		"MS-FSCC": 250, "MS-FSA": 210, "MS-SMB2": 500, "MS-DTYP": 185, "MS-ERREF": 15,
	}
	for spec, want := range minimum {
		m := specs[spec]
		if m == nil {
			t.Fatalf("%s: no section map", spec)
		}
		if len(m.Sections) < want {
			t.Errorf("%s: %d sections, want at least %d", spec, len(m.Sections), want)
		}
		if m.Revision == "" || m.Revision == "unknown" {
			t.Errorf("%s: revision is %q", spec, m.Revision)
		}
	}
}

// TestScanTreeSkipsNestedCheckouts pins that a working copy of this repository
// nested inside the tree is not scanned.
//
// Without the skip the check reported hundreds of findings from other people's
// in-progress edits and none of them could be silenced: known_wrong.txt keys on
// a path, and a nested checkout's path is whoever happened to create it. The
// findings that belong to this tree were buried, so the local run said 685
// problems where CI said none — and a check whose local output disagrees that
// far with CI is one nobody runs before pushing.
func TestScanTreeSkipsNestedCheckouts(t *testing.T) {
	specs, err := loadSpecs()
	if err != nil {
		t.Fatalf("loadSpecs: %v", err)
	}

	// The same bad citation in both trees: one in the tree being scanned, one
	// inside a nested checkout. Only the first may be reported.
	const badCitation = "// Per [MS-FSA] 2.1.5.14.3, a directory marked for deletion\n"

	root := t.TempDir()
	writeGo := func(rel string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("MkdirAll(%s): %v", rel, err)
		}
		if err := os.WriteFile(full, []byte("package p\n\n"+badCitation), 0o644); err != nil {
			t.Fatalf("WriteFile(%s): %v", rel, err)
		}
	}
	writeGo("mine.go")
	writeGo(".claude/worktrees/someone-else/theirs.go")

	findings, scanned, err := scanTree(root, specs, map[string]bool{})
	if err != nil {
		t.Fatalf("scanTree: %v", err)
	}

	if scanned != 1 {
		t.Errorf("scanned %d files, want 1: the nested checkout was read", scanned)
	}
	if len(findings) != 1 {
		t.Fatalf("got %d findings, want 1: %+v", len(findings), findings)
	}
	if findings[0].file != "mine.go" {
		t.Errorf("finding came from %q, want mine.go", findings[0].file)
	}
}
