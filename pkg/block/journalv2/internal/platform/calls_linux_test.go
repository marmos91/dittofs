package platform

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/block/journalv2/journalv2test/prof"
	"golang.org/x/sys/unix"
)

// TestSyscallTableIsReal pins every entry of the table NewOS uses to the
// golang.org/x/sys function the RFC's call goes through, by function pointer:
// a wrapper, or an entry pointing at another function, fails it. So does an
// entry added to the table without a pin here.
func TestSyscallTableIsReal(t *testing.T) {
	prof.Watch(t, prof.Options{})

	got := NewOS(t.TempDir()).calls
	pins := []struct {
		name, want string
		got, x     any
	}{
		{"open", "unix.Open", got.open, unix.Open},
		{"fsync", "unix.Fsync", got.fsync, unix.Fsync},
		{"fdatasync", "unix.Fdatasync", got.fdatasync, unix.Fdatasync},
		{"close", "unix.Close", got.close, unix.Close},
		{"fallocate", "unix.Fallocate", got.fallocate, unix.Fallocate},
		{"pwrite", "unix.Pwrite", got.pwrite, unix.Pwrite},
	}

	if n := reflect.TypeOf(calls{}).NumField(); n != len(pins) {
		t.Fatalf("the table has %d entries and %d are pinned: the table has an entry nobody pinned", n, len(pins))
	}

	for _, entry := range pins {
		if reflect.ValueOf(entry.got).Pointer() != reflect.ValueOf(entry.x).Pointer() {
			t.Errorf("the %s entry is not %s", entry.name, entry.want)
		}
	}
}

// TestNoCallBesideTheTable fails on a golang.org/x/sys/unix or syscall name
// used outside calls.go other than a constant the package passes as data, and
// on Sync or Fd taken from a field such as f.f, an *os.File's own fsync or
// its raw descriptor. A test that swaps the table sees only calls made
// through it, so a call made beside it would go unseen. It reads every
// non-test file whatever its build tags, finds the packages by import path so
// a renamed import is caught, and refuses a dot or blank import of them,
// whose names it could not tell apart.
func TestNoCallBesideTheTable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	constants := map[string]bool{
		"O_RDONLY":                 true,
		"O_DIRECTORY":              true,
		"O_CLOEXEC":                true,
		"EINTR":                    true,
		"EINVAL":                   true,
		"FALLOC_FL_ALLOCATE_RANGE": true,
	}
	raw := map[string]bool{
		"golang.org/x/sys/unix": true,
		"syscall":               true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || name == "calls.go" {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}

		pkgs := map[string]bool{}
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}

			if !raw[p] {
				continue
			}

			local := path.Base(p)
			if imp.Name != nil {
				local = imp.Name.Name
			}

			if local == "." || local == "_" {
				t.Errorf("%s: %q imported as %s", fset.Position(imp.Pos()), p, local)
				continue
			}

			pkgs[local] = true
		}

		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}

			switch x := sel.X.(type) {
			case *ast.Ident:
				if pkgs[x.Name] && !constants[sel.Sel.Name] {
					t.Errorf("%s: %s.%s outside the table", fset.Position(sel.Pos()), x.Name, sel.Sel.Name)
				}
			case *ast.SelectorExpr:
				if sel.Sel.Name == "Sync" || sel.Sel.Name == "Fd" {
					t.Errorf("%s: .%s on a field, beside the table", fset.Position(sel.Pos()), sel.Sel.Name)
				}
			}

			return true
		})
	}
}
