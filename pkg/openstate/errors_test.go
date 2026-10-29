package openstate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// A second numbered request of an owner, arriving while its first is
// between its check and its answer, is told to retry for that cause.
func TestOwnerRequestInFlightIsDelayed(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("a", 1, entity.NewUser()))
	oo := owner(a, "oo")
	rnd := &blockingRand{entered: make(chan struct{}), released: make(chan struct{}), r: newSeeded(t.Name())}
	f.d.Rand = rnd
	rnd.armed.Store(true)
	first := make(chan error)
	go func() {
		_, err := f.open(a, in(1), OpenRequest{Access: rd, Owner: oo, Seqs: num(1)})
		first <- err
	}()
	<-rnd.entered // the first open is minting its reference
	if _, err := f.open(a, in(1), OpenRequest{Access: rd, Owner: oo, Seqs: num(1)}); delayedFor(err) != DelayConflict {
		t.Errorf("an owner's request while its first is in flight: %v, want ErrDelay (DelayConflict)", err)
	}
	close(rnd.released)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

// bareDelays lists each use of ErrDelay in src but its declaration and
// DelayError's own methods: every retry-later names its cause in a
// DelayError, so no other code returns, assigns or wraps the bare one.
func bareDelays(fset *token.FileSet, src *ast.File) []string {
	var found []string
	ast.Inspect(src, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.FuncDecl:
			if x.Recv != nil && len(x.Recv.List) == 1 {
				if st, ok := x.Recv.List[0].Type.(*ast.StarExpr); ok {
					if id, ok := st.X.(*ast.Ident); ok && id.Name == "DelayError" {
						return false
					}
				}
			}
		case *ast.ValueSpec:
			for _, v := range x.Values {
				ast.Inspect(v, func(m ast.Node) bool {
					if id, ok := m.(*ast.Ident); ok && id.Name == "ErrDelay" {
						found = append(found, fset.Position(id.Pos()).String())
					}
					return true
				})
			}
			return false // its names declare, and are no use
		case *ast.Ident:
			if x.Name == "ErrDelay" {
				found = append(found, fset.Position(x.Pos()).String())
			}
		}
		return true
	})
	return found
}

// No source of the package uses the bare ErrDelay, and the check finds one
// returned, assigned or wrapped, and passes its declaration, DelayError's
// methods and a comment naming it.
func TestNoBareDelay(t *testing.T) {
	fset := token.NewFileSet()
	for src, want := range map[string]int{
		"func f() error { return ErrDelay }":                                    1,
		"func f() (err error) { err = ErrDelay; return err }":                   1,
		"func f() error { return fmt.Errorf(\"x: %w\", ErrDelay) }":             1,
		"var e = struct{ error }{ErrDelay}":                                     1,
		"var ErrDelay = errors.New(\"d\")":                                      0,
		"func (e *DelayError) Is(t error) bool { return t == ErrDelay }":        0,
		"// a comment naming ErrDelay\nfunc f() error { return &DelayError{} }": 0,
	} {
		planted, err := parser.ParseFile(fset, "planted.go", "package p\n"+src+"\n", parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		if got := len(bareDelays(fset, planted)); got != want {
			t.Fatalf("the check found %d bare ErrDelay in %q, want %d", got, src, want)
		}
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		src, err := parser.ParseFile(fset, name, b, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, at := range bareDelays(fset, src) {
			t.Errorf("%s uses the bare ErrDelay: give it a DelayError with its cause", at)
		}
	}
}
