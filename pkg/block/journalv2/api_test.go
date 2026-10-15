package journalv2

import (
	"bytes"
	"flag"
	"go/ast"
	"go/build"
	"go/doc"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/api.golden from the current API")

const apiGolden = "testdata/api.golden"

// TestAPIGolden renders the exported declarations of the public package and of
// every internal package, then compares them with testdata/api.golden. A
// change to any signature fails here until the golden file is regenerated with
// -update, so every change to the surface shows up in the diff that makes it.
//
// Only declarations are rendered, not their doc comments: the golden file
// pins the shape of the surface, and rewording a contract does not touch it.
func TestAPIGolden(t *testing.T) {
	got := renderAPI(t)

	if *update {
		if err := os.WriteFile(apiGolden, got, 0o644); err != nil {
			t.Fatalf("write %s: %v", apiGolden, err)
		}
		return
	}

	want, err := os.ReadFile(apiGolden)
	if err != nil {
		t.Fatalf("read %s: %v (run with -update to create it)", apiGolden, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("exported API differs from %s; if the change is intended, "+
			"run go test -run TestAPIGolden -update and commit the result\n\ngot:\n%s",
			apiGolden, got)
	}
}

// apiDirs returns the public package and every package under internal, in
// lexical order.
func apiDirs(t *testing.T) []string {
	t.Helper()
	dirs := []string{"."}

	err := filepath.WalkDir("internal", func(dir string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if d.IsDir() && d.Name() != "internal" {
			dirs = append(dirs, filepath.ToSlash(dir))
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walk internal: %v", err)
	}

	return dirs
}

func renderAPI(t *testing.T) []byte {
	t.Helper()

	var buf bytes.Buffer
	for _, dir := range apiDirs(t) {
		renderPackage(t, &buf, dir)
	}

	return buf.Bytes()
}

// renderPackage writes the exported declarations of the package in dir:
// constants, variables, functions, then each type followed by its constants,
// variables, constructors and methods, each group in the order go/doc gives.
//
// decision: files are selected for the host platform. A symbol declared only
// for another platform is missed here; platform-specific files are expected to
// declare the same exported symbols on every platform.
func renderPackage(t *testing.T, buf *bytes.Buffer, dir string) {
	t.Helper()
	importPath := journalv2Path
	if dir != "." {
		importPath += "/" + dir
	}

	bp, err := build.ImportDir(dir, 0)
	if err != nil {
		t.Fatalf("build.ImportDir %s: %v", dir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	for _, name := range bp.GoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		files = append(files, f)
	}

	pkg, err := doc.NewFromFiles(fset, files, importPath)
	if err != nil {
		t.Fatalf("go/doc %s: %v", importPath, err)
	}

	buf.WriteString("package ")
	buf.WriteString(importPath)
	buf.WriteString("\n")
	decl := func(node any) {
		buf.WriteString("\n")
		if err := printer.Fprint(buf, fset, node); err != nil {
			t.Fatalf("print %s: %v", importPath, err)
		}

		buf.WriteString("\n")
	}

	values := func(vs []*doc.Value) {
		for _, v := range vs {
			decl(v.Decl)
		}
	}

	funcs := func(fs []*doc.Func) {
		for _, f := range fs {
			decl(f.Decl)
		}
	}

	values(pkg.Consts)
	values(pkg.Vars)
	funcs(pkg.Funcs)

	for _, typ := range pkg.Types {
		decl(typ.Decl)
		values(typ.Consts)
		values(typ.Vars)
		funcs(typ.Funcs)
		funcs(typ.Methods)
	}

	buf.WriteString("\n")
}
