//go:build engine_conformance

// Package conformance holds the engine's conformance checks: one test per
// check the engine must pass, each run against the engine as production
// composes it, through the fixture.
//
// Every test here that fails is a known failure until the behaviour it checks
// lands, and is listed in test/engine/known.yaml until then.
package conformance

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"slices"
	"testing"
)

// The engine's package, type-checked from its source, test files left out.
func enginePackage(t *testing.T) (*token.FileSet, []*ast.File, *types.Package, *types.Info) {
	t.Helper()
	bp, err := build.ImportDir("..", 0)
	if err != nil {
		t.Fatalf("the engine's package: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(bp.Dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	pkg, info := typeCheck(t, fset, files)
	return fset, files, pkg, info
}

// sourcePackage type-checks src, one file of a package importing only the
// standard library.
func sourcePackage(t *testing.T, src string) (*token.FileSet, []*ast.File, *types.Package, *types.Info) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "seeded.go", src, 0)
	if err != nil {
		t.Fatalf("parse the seeded source: %v", err)
	}
	pkg, info := typeCheck(t, fset, []*ast.File{f})
	return fset, []*ast.File{f}, pkg, info
}

func typeCheck(t *testing.T, fset *token.FileSet, files []*ast.File) (*types.Package, *types.Info) {
	t.Helper()
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}, Selections: map[*ast.SelectorExpr]*types.Selection{}, Uses: map[*ast.Ident]types.Object{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check(files[0].Name.Name, fset, files, info)
	if err != nil {
		t.Fatalf("type-check %s: %v", files[0].Name.Name, err)
	}
	return pkg, info
}

// declaredInterfaces is every interface the package declares: the interfaces
// Deps holds and those their methods answer, since the engine declares no
// interface of its own besides.
func declaredInterfaces(pkg *types.Package) map[string]*types.Interface {
	out := map[string]*types.Interface{}
	for _, name := range pkg.Scope().Names() {
		if tn, ok := pkg.Scope().Lookup(name).(*types.TypeName); ok && !tn.IsAlias() {
			if it, ok := tn.Type().Underlying().(*types.Interface); ok {
				out[name] = it
			}
		}
	}
	return out
}

// uncalled names, as Interface.Method, every method of a declared interface
// the package never calls. Only a selection that is called counts: a method
// value or a selection discarded with _ = calls nothing.
func uncalled(files []*ast.File, pkg *types.Package, info *types.Info) []string {
	called := map[types.Object]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if fun, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok {
					if sel := info.Selections[fun]; sel != nil {
						called[sel.Obj()] = true
					}
				}
			}
			return true
		})
	}
	var out []string
	for name, it := range declaredInterfaces(pkg) {
		for m := range it.ExplicitMethods() {
			if !called[m] {
				out = append(out, name+"."+m.Name())
			}
		}
	}
	slices.Sort(out)
	return out
}

// Every method of every interface the engine declares for its dependencies
// is one the engine calls: an interface holds exactly what its consumer
// needs.
func TestRFC8C_DeclaredInterfaces(t *testing.T) {
	_, seededFiles, seeded, seededInfo := sourcePackage(t, `package seeded
type Store interface{ Used(); Valued(); Discarded(); Unused() }
type Deps struct{ S Store }
func use(d Deps) {
	d.S.Used()
	f := d.S.Valued
	_ = f
	_ = d.S.Discarded
}
`)
	want := []string{"Store.Discarded", "Store.Unused", "Store.Valued"}
	if got := uncalled(seededFiles, seeded, seededInfo); !slices.Equal(got, want) {
		t.Fatalf("seeded source: uncalled %v, want %v", got, want)
	}

	_, files, pkg, info := enginePackage(t)
	if got := uncalled(files, pkg, info); len(got) > 0 {
		t.Errorf("the engine never calls %d declared methods: %v", len(got), got)
	}
}

// assertions reports every type assertion or type switch on a value of a
// declared interface, every assertion to an interface literal, and every
// setter on *Engine.
func assertions(fset *token.FileSet, files []*ast.File, pkg *types.Package, info *types.Info) []string {
	declared := declaredInterfaces(pkg)
	var out []string
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.TypeAssertExpr:
				onDep := false
				if named, ok := types.Unalias(info.TypeOf(n.X)).(*types.Named); ok && named.Obj().Pkg() == pkg {
					_, onDep = declared[named.Obj().Name()]
				}
				_, literal := n.Type.(*ast.InterfaceType)
				if onDep || literal {
					out = append(out, fmt.Sprintf("%s: type assertion on %s", fset.Position(n.Pos()), info.TypeOf(n.X)))
				}
			case *ast.FuncDecl:
				if n.Recv != nil && len(n.Name.Name) > 3 && n.Name.Name[:3] == "Set" {
					if star, ok := n.Recv.List[0].Type.(*ast.StarExpr); ok {
						if id, ok := star.X.(*ast.Ident); ok && id.Name == "Engine" {
							out = append(out, fmt.Sprintf("%s: setter %s on *Engine", fset.Position(n.Pos()), n.Name.Name))
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// The engine never asks what a dependency is beyond its declared interface,
// and has no setter: what it depends on is fixed by Deps at New.
func TestRFC8C_NoAssertions(t *testing.T) {
	seeded := assertions(sourcePackage(t, `package seeded
type Store interface{ Get() }
type Engine struct{ s Store }
func (e *Engine) SetStore(s Store) { e.s = s }
func (e *Engine) peek() {
	_, _ = e.s.(interface{ Put() })
	switch e.s.(type) {
	}
	var v any = 1
	_ = v.(int)
}
`))
	if len(seeded) != 3 {
		t.Fatalf("seeded source: reported %d, want the setter, the assertion and the switch:\n%v", len(seeded), seeded)
	}

	if got := assertions(enginePackage(t)); len(got) > 0 {
		t.Errorf("the engine asserts on its dependencies or has a setter:\n%v", got)
	}
}
