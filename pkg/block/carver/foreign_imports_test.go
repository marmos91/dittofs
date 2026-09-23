package carver

import (
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoForeignImports fails if carver's non-test OR test files import
// anything outside the standard library, the BLAKE3 implementation, and
// chunker.
//
// The package doc says the carver wraps chunker and nothing else, and that is
// load-bearing rather than tidy: engine imports carver, so any edge from
// carver back toward engine is a cycle, and an edge to journal or local would
// make the boundary-search-plus-batching logic answerable only in terms of a
// run and a local store. Pulling engine's read loop in here needs journal.Run,
// journal.LocalStore, and engine's own BlockSink/Deduper/CarveChunk — the last
// three are the cycle. A prose claim about a dependency graph rots, so the
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
			t.Errorf("forbidden %s %q: carver may import only the standard "+
				"library, lukechampine.com/blake3 and pkg/block/chunker",
				group.kind, imp)
		}
	}
}

// importAllowed reports whether an import path is the standard library, the
// hash implementation, or chunker. Standard-library paths are the ones whose
// first segment carries no dot — the same rule the go command uses to tell a
// stdlib path from a module path.
func importAllowed(path string) bool {
	switch path {
	case "lukechampine.com/blake3", "github.com/marmos91/dittofs/pkg/block/chunker":
		return true
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
