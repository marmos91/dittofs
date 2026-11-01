package vfserr_test

import (
	"go/build"
	"strings"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs/vfserr"
)

// Every declared kind reads by its own name, never as an undeclared one, so
// a value added without a name fails here.
func TestEveryDeclaredKindHasAName(t *testing.T) {
	var errs []error
	for c := vfserr.AccessCause(1); c.Valid(); c++ {
		errs = append(errs, &vfserr.AccessError{Cause: c})
	}
	for s := vfserr.QuotaScope(1); s.Valid(); s++ {
		errs = append(errs, &vfserr.QuotaError{Scope: s})
	}
	for c := vfserr.DelayCause(1); c.Valid(); c++ {
		errs = append(errs, &vfserr.Delay{Cause: c})
	}
	for k := vfserr.StateKind(1); k.Valid(); k++ {
		errs = append(errs, &vfserr.StateError{Kind: k})
	}
	for k := vfserr.CrossKind(1); k.Valid(); k++ {
		errs = append(errs, &vfserr.CrossError{Kind: k})
	}
	if len(errs) == 0 {
		t.Fatal("no kind is declared valid")
	}
	for _, err := range errs {
		if msg := err.Error(); strings.HasSuffix(msg, ": unknown") {
			t.Errorf("%#v reads %q: its kind has no name", err, msg)
		}
	}
}

// The engine imports this package, and every adapter compiles it through the
// service's contract, so it imports the standard library and nothing else.
func TestImportsOnlyTheStandardLibrary(t *testing.T) {
	p, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range p.Imports {
		dep, err := build.Import(path, ".", build.FindOnly)
		if err != nil {
			t.Fatal(err)
		}
		if !dep.Goroot {
			t.Errorf("imports %s, outside the standard library", path)
		}
	}
}
