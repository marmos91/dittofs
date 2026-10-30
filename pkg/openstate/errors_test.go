package openstate

import (
	"go/scanner"
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
	rnd.wait(t) // the first open is minting its reference
	if _, err := f.open(a, in(1), OpenRequest{Access: rd, Owner: oo, Seqs: num(1)}); delayedFor(err) != DelayConflict {
		t.Errorf("an owner's request while its first is in flight: %v, want ErrDelay (DelayConflict)", err)
	}
	close(rnd.released)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
}

// No source of the package but errors.go names ErrDelay: every retry-later
// names its cause in a DelayError, so no other code returns, assigns, wraps or
// tests for the bare one.
func TestNoBareDelay(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if name == "errors.go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		var s scanner.Scanner
		s.Init(fset.AddFile(name, -1, len(b)), b, nil, 0)
		for pos, tok, lit := s.Scan(); tok != token.EOF; pos, tok, lit = s.Scan() {
			if tok == token.IDENT && lit == "ErrDelay" {
				t.Errorf("%s uses the bare ErrDelay: give it a DelayError with its cause", fset.Position(pos))
			}
		}
	}
}
