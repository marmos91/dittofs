package vfs

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/marmos91/dittofs/"

// network is the network code the filesystem never imports: routing and
// transport are implemented outside it. net/netip, addresses as values that
// client rules compare, is not network code.
var network = []string{"net", "crypto/tls", "golang.org/x/net", "google.golang.org/grpc"}

// below is what admit, pseudofs and lockorder never import: each reaches the
// store only through the narrow reads it declares beside its own code. The
// store's value types, entity, are no read, and they may name them.
var below = []string{module + "pkg/metadata", module + "pkg/block", module + "pkg/openstate"}

// values is the one package under below they may import.
const values = module + "pkg/metadata/entity"

// adapters is every adapter's code, which the filesystem never imports.
var adapters = []string{module + "internal/adapter", module + "pkg/adapter"}

// under reports whether path is one of roots or a package below one.
func under(path string, roots []string) bool {
	return slices.ContainsFunc(roots, func(r string) bool { return path == r || strings.HasPrefix(path, r+"/") })
}

// violations parses every non-test Go file under root, its testdata left
// out, and names each import or call the filesystem's architecture forbids.
func violations(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		pkg := strings.Split(rel, "/")[0]
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			switch {
			case p != "net/netip" && under(p, network):
				found = append(found, rel+": imports network code "+p)
			case under(p, adapters):
				found = append(found, rel+": imports an adapter, "+p)
			case (pkg == "admit" || pkg == "pseudofs" || pkg == "lockorder") && under(p, below) && p != values:
				found = append(found, rel+": imports "+p+" below its declared reads")
			}
		}
		// Every selector, a call's or not: a method value reaches the same
		// method, and the ControlPlane view's type its methods. A selector on
		// an imported package, as a constant named Granted, is no method.
		pkgs := map[string]bool{}
		for _, imp := range file.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			pkgs[p[strings.LastIndex(p, "/")+1:]] = true
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, _ := sel.X.(*ast.Ident)
			switch name := sel.Sel.Name; {
			case name == "ControlPlane":
				found = append(found, rel+": calls ControlPlane")
			case (name == "Authorize" || name == "Granted") && pkg != "authz" && (x == nil || !pkgs[x.Name]):
				found = append(found, rel+": calls "+name+" outside authz")
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// The filesystem imports no adapter and no network code, admit, pseudofs and
// lockorder import nothing below their declared reads, only authz calls
// Authorize or Granted, and nothing calls the store's ControlPlane.
func TestArchitecture(t *testing.T) {
	for _, v := range violations(t, ".") {
		t.Error(v)
	}
}

// The check reports each of its seeded violations, and nothing else.
func TestArchitectureCheckReportsEachSeededViolation(t *testing.T) {
	got := violations(t, filepath.Join("testdata", "arch"))
	want := []string{
		"adapter.go: imports an adapter, " + module + "internal/adapter/nfs",
		"admit/openstate.go: imports " + module + "pkg/openstate below its declared reads",
		"authorize.go: calls Authorize outside authz",
		"authorize.go: calls Granted outside authz",
		"controlplane.go: calls ControlPlane",
		"cpview.go: calls ControlPlane",
		"grpc.go: imports network code google.golang.org/grpc",
		"lockorder/store.go: imports " + module + "pkg/metadata/kvstore below its declared reads",
		"methodvalue.go: calls Authorize outside authz",
		"network.go: imports network code net",
		"pkgadapter.go: imports an adapter, " + module + "pkg/adapter/nfs",
		"pseudofs/engine.go: imports " + module + "pkg/block/engine below its declared reads",
		"tls.go: imports network code crypto/tls",
		"xnet.go: imports network code golang.org/x/net/http2",
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("the check reported\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
