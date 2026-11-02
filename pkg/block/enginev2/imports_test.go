package enginev2

import (
	"errors"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/marmos91/dittofs/"

// allowed is every import a package of the engine may have besides the
// standard library and the engine's own packages. Each entry names the
// packages under this directory it applies to; "*" is every one. Nothing of
// the previous engine, journal, syncer, carver, remote tier or metadata
// service is on it, and neither is the filesystem's package: the engine takes
// only its errors, from pkg/vfs/vfserr.
var allowed = map[string][]string{
	module + "pkg/block/journalv2":  {"*"},
	module + "pkg/metadata/entity":  {"*"},
	module + "pkg/metadata/kvstore": {"*"},
	module + "pkg/openstate":        {"*"},
	module + "pkg/vfs/vfserr":       {"*"},
}

// TestImportRule walks every package under this directory, test packages
// included, and fails on any import the engine may not have: anything but
// the standard library, the engine's own packages and the allowlist.
// Test files are checked like any other, and so are files built only for
// another platform.
func TestImportRule(t *testing.T) {
	checked := 0
	err := filepath.WalkDir(".", func(dir string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		rel := filepath.ToSlash(dir)
		for _, imp := range packageImports(t, dir) {
			if why := forbidden(rel, imp); why != "" {
				t.Errorf("%s: forbidden import %q: %s", rel, imp, why)
			}
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if checked == 0 {
		t.Fatal("no package checked")
	}
}

// TestImportRuleRefusesPreviousAndFilesystem pins the rule's edges: the
// previous components and the filesystem's package are refused, while the
// metadata packages the engine needs, below the previous metadata service,
// and the filesystem's errors are allowed.
func TestImportRuleRefusesPreviousAndFilesystem(t *testing.T) {
	for _, c := range []struct {
		path    string
		allowed bool
	}{
		{module + "pkg/block/engine", false},
		{module + "pkg/block/journal", false},
		{module + "pkg/block/journal/wal", false},
		{module + "pkg/block/syncer", false},
		{module + "pkg/block/carver", false},
		{module + "pkg/block/remote/s3", false},
		{module + "pkg/metadata", false},
		{module + "pkg/vfs", false},
		{module + "pkg/vfs/vfstest", false},
		{module + "internal/vfs", false},
		{module + "pkg/block/journalv2/journalv2test", false},
		{module + "pkg/block/journalv2", true},
		{module + "pkg/block/enginev2/enginev2test", true},
		{module + "pkg/metadata/entity", true},
		{module + "pkg/metadata/kvstore", true},
		{module + "pkg/openstate", true},
		{module + "pkg/vfs/vfserr", true},
		{"context", true},
	} {
		if got := forbidden(".", c.path) == ""; got != c.allowed {
			t.Errorf("importing %s: allowed %v, want %v", c.path, got, c.allowed)
		}
	}
}

// forbidden returns why the package at rel, a slash-separated path relative
// to this directory, may not import path, or "" when it may.
func forbidden(rel, path string) string {
	first, _, _ := strings.Cut(path, "/")
	if !strings.Contains(first, ".") || within(path, module+"pkg/block/enginev2") {
		return ""
	}
	for _, pkg := range allowed[path] {
		if pkg == "*" || pkg == rel {
			return ""
		}
	}
	return "not on the engine's allowlist: no previous component, no pkg/vfs"
}

// within reports whether path is root or a package below it.
func within(path, root string) bool {
	return path == root || strings.HasPrefix(path, root+"/")
}

// packageImports returns every import of the package in dir: its files built
// for this platform, its test files, and its files built only for another
// platform, which go/build would otherwise leave unlisted.
func packageImports(t *testing.T, dir string) []string {
	t.Helper()
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		var noGo *build.NoGoError
		if !errors.As(err, &noGo) {
			t.Fatalf("build.ImportDir %s: %v", dir, err)
		}
	}
	imports := append(append(append([]string{}, pkg.Imports...), pkg.TestImports...), pkg.XTestImports...)
	fset := token.NewFileSet()
	for _, name := range pkg.IgnoredGoFiles {
		f, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Join(dir, name), err)
		}
		for _, spec := range f.Imports {
			p, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote %s in %s: %v", spec.Path.Value, name, err)
			}
			imports = append(imports, p)
		}
	}
	return imports
}
