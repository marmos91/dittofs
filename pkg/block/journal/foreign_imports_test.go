package journal

import (
	"go/build"
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

	for _, group := range []struct {
		kind    string
		imports []string
	}{
		{"import", pkg.Imports},
		{"test import", pkg.TestImports},
		{"external test import", pkg.XTestImports},
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
