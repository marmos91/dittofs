package journal

import (
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoForeignImports fails if journal's non-test OR test files import
// anything outside the standard library and golang.org/x/sys.
//
// The package is designed to stand alone as its own library, and doc.go says
// so. A prose claim about a dependency graph rots — this one already had,
// naming two packages that no longer exist here and one that did — so the
// claim is asserted instead of written down.
//
// Deliberately go/build and not go/packages: the latter lives in
// golang.org/x/tools, which is exactly what this test forbids. It would fail
// itself.
func TestNoForeignImports(t *testing.T) {
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("build.ImportDir: %v", err)
	}

	// build.ImportDir resolves //go:build tags and _GOOS/_GOARCH filename
	// suffixes against the host, so a file that does not match lands in
	// IgnoredGoFiles and its imports never reach the lists above: a forbidden
	// import behind //go:build darwin would pass unseen on a linux run. Parse
	// those files directly so the rule holds for every platform rather than
	// only the one the test happens to run on.
	var otherPlatform []string
	fset := token.NewFileSet()
	for _, name := range pkg.IgnoredGoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range f.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote %s in %s: %v", spec.Path.Value, name, err)
			}
			otherPlatform = append(otherPlatform, path)
		}
	}

	for _, group := range []struct {
		kind    string
		imports []string
	}{
		{"import", pkg.Imports},
		{"test import", pkg.TestImports},
		{"external test import", pkg.XTestImports},
		{"other-platform import", otherPlatform},
	} {
		for _, imp := range group.imports {
			if importAllowed(imp) {
				t.Logf("%s %q ok", group.kind, imp)
				continue
			}
			t.Errorf("forbidden %s %q: journal may import only the standard "+
				"library and golang.org/x/sys", group.kind, imp)
		}
	}
}

// importAllowed reports whether an import path is the standard library or the
// one sanctioned exception. Standard-library paths are the ones whose first
// segment carries no dot — the same rule the go command uses to tell a stdlib
// path from a module path.
func importAllowed(path string) bool {
	if path == "golang.org/x/sys" || strings.HasPrefix(path, "golang.org/x/sys/") {
		return true
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
