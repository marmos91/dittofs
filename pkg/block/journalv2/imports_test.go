package journalv2

import (
	"errors"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const (
	modulePath    = "github.com/marmos91/dittofs"
	journalv2Path = modulePath + "/pkg/block/journalv2"
	journalv1Path = modulePath + "/pkg/block/journal"
	clockPath     = modulePath + "/pkg/clock"
	simPath       = modulePath + "/pkg/sim"
	xsysPath      = "golang.org/x/sys"
)

// TestImportRule walks every package under this directory, internal, test and
// conformance packages included, and fails on any import the journal may not
// have. The rule:
//
//   - the standard library and the journal's own packages are allowed
//     everywhere;
//   - golang.org/x/sys is allowed in internal/platform only;
//   - the set's clock package is allowed in journalv2test only, and its
//     test-clock and fault package in journalv2test and conformance, whose
//     checks build fault rules; they reach the clock only through the
//     fixture;
//   - the previous journal package is forbidden everywhere;
//   - anything else is forbidden.
//
// Test files are checked like any other file, and so are files built only for
// another platform.
//
// Deliberately go/build and not go/packages: the latter lives in
// golang.org/x/tools, which this rule forbids.
func TestImportRule(t *testing.T) {
	checked := 0
	err := filepath.WalkDir(".", func(dir string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		rel := filepath.ToSlash(dir)
		for _, imp := range packageImports(t, dir) {
			if reason := forbidden(rel, imp.path); reason != "" {
				t.Errorf("%s: forbidden %s %q: %s", rel, imp.kind, imp.path, reason)
			}
		}

		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if checked == 0 {
		t.Fatal("no package checked")
	}
}

type importRef struct {
	kind string
	path string
}

// packageImports returns every import of the package in dir: those of the files
// built for this platform, of its test files, and of the files built only for
// another platform. A directory with no Go files yields nothing.
func packageImports(t *testing.T, dir string) []importRef {
	t.Helper()
	pkg, err := build.ImportDir(dir, 0)

	if err != nil {
		// A directory whose Go files are all for another platform is a
		// NoGoError, but its IgnoredGoFiles are still listed and checked
		// below. A directory with no Go files at all lists nothing.
		var noGo *build.NoGoError
		if !errors.As(err, &noGo) {
			t.Fatalf("build.ImportDir %s: %v", dir, err)
		}
	}

	// build.ImportDir resolves build constraints and _GOOS/_GOARCH suffixes
	// against the host, so a file for another platform lands in
	// IgnoredGoFiles and its imports are not listed. Parse those files
	// directly so the rule holds for every platform, not only the one the test
	// runs on.
	var refs []importRef
	fset := token.NewFileSet()
	for _, name := range pkg.IgnoredGoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}

		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote %s in %s: %v", spec.Path.Value, name, err)
			}
			refs = append(refs, importRef{"other-platform import", path})
		}
	}

	for _, group := range []struct {
		kind    string
		imports []string
	}{
		{"import", pkg.Imports},
		{"test import", pkg.TestImports},
		{"external test import", pkg.XTestImports},
	} {
		for _, path := range group.imports {
			refs = append(refs, importRef{group.kind, path})
		}
	}

	return refs
}

// forbidden returns why the package at rel, a slash-separated path relative to
// this directory, may not import path, or "" when it may.
func forbidden(rel, path string) string {
	switch {
	case within(path, journalv1Path):
		return "the journal must not depend on the previous journal package"
	case within(path, journalv2Path):
		return ""
	case within(path, xsysPath):
		if within(rel, "internal/platform") {
			return ""
		}
		return "golang.org/x/sys is allowed in internal/platform only"
	case within(path, simPath):
		if within(rel, "journalv2test") || within(rel, "conformance") {
			return ""
		}
		return "the set's test-clock and fault package is allowed in journalv2test and conformance only"
	case within(path, clockPath):
		if within(rel, "journalv2test") {
			return ""
		}
		return "the set's clock package is allowed in journalv2test only"
	case isStdlib(path):
		return ""
	default:
		return "the journal may import only the standard library"
	}
}

// within reports whether path is root or a package below it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// isStdlib reports whether path names a standard-library package: one whose
// first element carries no dot, the rule the go command uses to tell a
// standard-library path from a module path.
func isStdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

// TestImportRuleScopesSim pins where the set's fault package may be
// imported: by the test components and the conformance checks, which build
// fault rules, and nowhere else; and the clock package by the test
// components only.
func TestImportRuleScopesSim(t *testing.T) {
	for _, c := range []struct {
		rel, path string
		allowed   bool
	}{
		{"journalv2test", simPath, true},
		{"conformance", simPath, true},
		{"internal/core", simPath, false},
		{".", simPath, false},
		{"conformance", clockPath, false},
		{"journalv2test", clockPath, true},
	} {
		if got := forbidden(c.rel, c.path) == ""; got != c.allowed {
			t.Errorf("%s importing %s: allowed %v, want %v", c.rel, c.path, got, c.allowed)
		}
	}
}
