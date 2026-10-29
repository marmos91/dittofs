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

// bareDelays lists each return of the bare ErrDelay in src: every
// retry-later names its cause in a DelayError.
func bareDelays(fset *token.FileSet, src *ast.File) []string {
	var found []string
	ast.Inspect(src, func(n ast.Node) bool {
		if r, ok := n.(*ast.ReturnStmt); ok {
			for _, x := range r.Results {
				if id, ok := x.(*ast.Ident); ok && id.Name == "ErrDelay" {
					found = append(found, fset.Position(x.Pos()).String())
				}
			}
		}
		return true
	})
	return found
}

// No source of the package returns the bare ErrDelay, and the check finds
// one planted.
func TestNoBareDelay(t *testing.T) {
	fset := token.NewFileSet()
	planted, err := parser.ParseFile(fset, "planted.go", "package p\nfunc f() error { return ErrDelay }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(bareDelays(fset, planted)) != 1 {
		t.Fatal("the check missed a planted bare ErrDelay")
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
			t.Errorf("%s returns the bare ErrDelay: give it a DelayError with its cause", at)
		}
	}
}
