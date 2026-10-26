package prof

import (
	"go/build"
	"strings"
	"testing"
)

// TestStandardLibraryOnly fails on any import outside the standard library,
// test files included. The package is written so the journal's tree, which
// forbids third-party packages, may use it; the journal's import rule checks
// only its own packages' direct imports, so this package keeps to the rule on
// its own.
func TestStandardLibraryOnly(t *testing.T) {
	// UseAllFiles reads the files built for every platform, not only this one.
	ctx := build.Default
	ctx.UseAllFiles = true
	pkg, err := ctx.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("import .: %v", err)
	}

	for _, imports := range [][]string{pkg.Imports, pkg.TestImports, pkg.XTestImports} {
		for _, path := range imports {
			first, _, _ := strings.Cut(path, "/")
			if strings.Contains(first, ".") {
				t.Errorf("imports %q: only the standard library is allowed", path)
			}
		}
	}
}
