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
//     everywhere, except that journalv2test and the packages below it are
//     imported by test files only, and journalv2test/raw imports no package
//     of the journal at all, so it stays an account of the bytes that shares
//     no code with the codecs it checks;
//   - golang.org/x/sys is allowed in internal/platform only;
//   - the set's clock and test-clock packages are allowed in journalv2test
//     only;
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
			if reason := forbidden(rel, imp.kind, imp.path); reason != "" {
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
			kind := "other-platform import"
			if strings.HasSuffix(name, "_test.go") {
				kind = "other-platform test import"
			}
			refs = append(refs, importRef{kind, path})
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
// this directory, may not import path as an import of the given kind, or ""
// when it may.
func forbidden(rel, kind, path string) string {
	switch {
	case within(path, journalv1Path):
		return "the journal must not depend on the previous journal package"
	case within(rel, "journalv2test/raw") && !isStdlib(path):
		return "journalv2test/raw may import the standard library only, so it shares no code with what it checks"
	case within(path, journalv2Path+"/journalv2test") && !within(rel, "journalv2test"):
		if strings.Contains(kind, "test import") {
			return ""
		}
		return "journalv2test and the packages below it are test helpers: only test files import them"
	case within(path, journalv2Path):
		return ""
	case within(path, xsysPath):
		if within(rel, "internal/platform") {
			return ""
		}
		return "golang.org/x/sys is allowed in internal/platform only"
	case within(path, clockPath), within(path, simPath):
		if within(rel, "journalv2test") {
			return ""
		}
		return "the set's clock and test-clock packages are allowed in journalv2test only"
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

// TestForbidden pins the rules that no package of the tree breaks today, so
// the walk above cannot pass because a rule matches nothing.
func TestForbidden(t *testing.T) {
	const (
		raw    = journalv2Path + "/journalv2test/raw"
		format = journalv2Path + "/internal/format"
		core   = journalv2Path + "/internal/core"
	)

	for _, c := range []struct {
		name            string
		rel, kind, path string
		forbidden       bool
	}{
		{"production code imports raw", "internal/format", "import", raw, true},
		{"production code of another platform imports raw", "internal/format", "other-platform import", raw, true},
		{"a test imports raw", "internal/format", "test import", raw, false},
		{"an external test imports raw", "internal/format", "external test import", raw, false},
		{"a test of another platform imports raw", "internal/format", "other-platform test import", raw, false},
		{"raw imports the codec", "journalv2test/raw", "import", format, true},
		{"a test of raw imports the codec", "journalv2test/raw", "test import", format, true},
		{"raw imports core", "journalv2test/raw", "import", core, true},
		{"raw imports the standard library", "journalv2test/raw", "import", "hash/crc32", false},
		{"journalv2test imports raw", "journalv2test", "import", raw, false},
		{"production code imports the codec", "journalv2test", "import", format, false},
		{"the root package imports journalv2test", ".", "import", journalv2Path + "/journalv2test", true},
	} {
		if got := forbidden(c.rel, c.kind, c.path) != ""; got != c.forbidden {
			t.Errorf("%s: forbidden(%q, %q, %q) = %t, want %t", c.name, c.rel, c.kind, c.path, got, c.forbidden)
		}
	}
}
