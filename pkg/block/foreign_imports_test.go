package block

import (
	"go/build"
	"strings"
	"testing"
)

// TestNoForeignImports fails if block's non-test OR test files import anything
// outside the standard library, the BLAKE3 implementation, and block itself.
//
// The package is the vocabulary every other block package speaks, so it must
// stay a leaf: anything it imports is something the vocabulary drags along.
// README.md says so, and a prose claim about a dependency graph rots, so the
// claim is asserted rather than written down.
//
// Deliberately go/build and not go/packages: the latter lives in
// golang.org/x/tools, which is exactly the kind of dependency this test
// forbids. It would fail itself.
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
			t.Errorf("forbidden %s %q: block may import only the standard "+
				"library and lukechampine.com/blake3", group.kind, imp)
		}
	}
}

// importAllowed reports whether an import path is the standard library, the
// hash implementation, or block itself (the external _test package imports it
// back). Standard-library paths are the ones whose first segment carries no
// dot — the same rule the go command uses to tell a stdlib path from a module
// path.
func importAllowed(path string) bool {
	switch path {
	case "lukechampine.com/blake3", "github.com/marmos91/dittofs/pkg/block":
		return true
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
