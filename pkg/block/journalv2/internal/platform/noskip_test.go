package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
)

// TestNoSkip fails on a skip in any test of this package or of the storage
// contract: a call of Skip, Skipf or SkipNow on any receiver, and any use of
// testing.Short. The Windows job runs the tests with -short, and a skipped
// test passes there while it protects nothing. It reads every test file
// whatever its build tags, so the Windows tests are checked on any platform,
// and finds the testing package by import path, so a renamed import is
// caught; a dot import, whose names it could not tell apart, is refused.
func TestNoSkip(t *testing.T) {
	prof.Watch(t, prof.Options{})

	skips := map[string]bool{
		"Skip":    true,
		"Skipf":   true,
		"SkipNow": true,
	}

	fset := token.NewFileSet()
	checked := 0

	for _, dir := range []string{".", filepath.Join("..", "storage")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}

		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}

			path := filepath.Join(dir, e.Name())

			f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}

			checked++

			names := map[string]bool{}

			for _, imp := range f.Imports {
				p, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatal(err)
				}

				if p != "testing" {
					continue
				}

				local := "testing"

				if imp.Name != nil {
					local = imp.Name.Name
				}

				if local == "." {
					t.Errorf(
						"%s: \"testing\" imported as ., whose Short this check cannot see",
						fset.Position(imp.Pos()),
					)
				}

				names[local] = true
			}

			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}

				if x, ok := sel.X.(*ast.Ident); ok && names[x.Name] && sel.Sel.Name == "Short" {
					t.Errorf(
						"%s: testing.Short: no test here may depend on -short",
						fset.Position(sel.Pos()),
					)
				}

				if skips[sel.Sel.Name] {
					t.Errorf(
						"%s: .%s: no test here may skip",
						fset.Position(sel.Pos()),
						sel.Sel.Name,
					)
				}

				return true
			})
		}
	}

	if checked == 0 {
		t.Fatal("no test file was read")
	}
}
