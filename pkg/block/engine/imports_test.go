package engine

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

const module = "github.com/cubbit/dittofs/"

// allowed is every import a package of the engine may have besides the
// standard library and the engine's own packages. Each entry names the
// packages under this directory it applies to; "*" is every one. Nothing of
// the previous engine, journal, syncer, carver, remote tier or metadata
// service is on it, and neither is the filesystem's package: the engine takes
// only its errors, from pkg/vfs/vfserr. Open state is the fixture's alone,
// which adapts the table to the engine's CloneHold as the root does.
var allowed = map[string][]string{
	module + "pkg/block/journal":    {"*"},
	module + "pkg/metadata/entity":  {"*"},
	module + "pkg/metadata/kvstore": {"*"},
	module + "pkg/openstate":        {"enginetest"},
	module + "pkg/vfs/vfserr":       {"*"},
	// The test fixture composes the engine over the real journal, its seam and
	// profiler, the real store on an in-memory kv.KV, and a test clock. The
	// unit tests' start failure tests put the fault-injecting kv.KV under that
	// store.
	module + "pkg/block/journal/journaltest":      {"enginetest"},
	module + "pkg/block/journal/journaltest/prof": {"enginetest", unitTests},
	module + "pkg/metadata/kv":                    {"enginetest", unitTests},
	module + "pkg/metadata/kv/badgerkv":           {"enginetest", unitTests},
	module + "pkg/metadata/kv/kvtest":             {unitTests},
	module + "pkg/metadata/kvstore/kvstoretest":   {"enginetest", unitTests},
	module + "pkg/sim":                            {"enginetest", unitTests},
}

// unitTests names the engine's external test package, whose unit
// composition runs the engine over the fake journal, the real store on an
// in-memory kv.KV, a test clock and the profiler, and whose start failure
// tests put the fault-injecting kv.KV under that store. The engine's own
// files, tests in its package included, import none of them.
const unitTests = "._test"

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
		imports, xtest := packageImports(t, dir)
		for _, imp := range imports {
			if why := forbidden(rel, imp); why != "" {
				t.Errorf("%s: forbidden import %q: %s", rel, imp, why)
			}
		}
		for _, imp := range xtest {
			if why := forbidden(rel+"_test", imp); why != "" {
				t.Errorf("%s_test: forbidden import %q: %s", rel, imp, why)
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
// previous components, the filesystem's package and open state are refused,
// while the metadata packages the engine needs, below the previous metadata
// service, and the filesystem's errors are allowed.
func TestImportRuleRefusesPreviousAndFilesystem(t *testing.T) {
	for _, c := range []struct {
		path    string
		allowed bool
	}{
		{module + "pkg/block/syncer", false},
		{module + "pkg/block/carver", false},
		{module + "pkg/block/remote/s3", false},
		{module + "pkg/metadata", false},
		{module + "pkg/vfs", false},
		{module + "pkg/vfs/vfstest", false},
		{module + "internal/vfs", false},
		{module + "pkg/block/journal/journaltest", false},
		{module + "pkg/block/journal", true},
		{module + "pkg/block/engine/enginetest", true},
		{module + "pkg/metadata/entity", true},
		{module + "pkg/metadata/kvstore", true},
		{module + "pkg/openstate", false},
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
	if !strings.Contains(first, ".") || within(path, module+"pkg/block/engine") {
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
// for this platform, its test files in the package, and its files built only
// for another platform, which go/build would otherwise leave unlisted; and,
// apart, those of its external test package.
func packageImports(t *testing.T, dir string) (imports, xtest []string) {
	t.Helper()
	pkg, err := build.ImportDir(dir, 0)
	if err != nil {
		var noGo *build.NoGoError
		if !errors.As(err, &noGo) {
			t.Fatalf("build.ImportDir %s: %v", dir, err)
		}
	}
	imports = append(append([]string{}, pkg.Imports...), pkg.TestImports...)
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
	return imports, pkg.XTestImports
}

// TestImportRuleKeepsTheCompositionInTests pins that the unit composition's
// store, clock and profiler are allowed in the engine's external test
// package and refused in the engine itself.
func TestImportRuleKeepsTheCompositionInTests(t *testing.T) {
	for _, path := range []string{module + "pkg/sim", module + "pkg/metadata/kv/badgerkv", module + "pkg/metadata/kvstore/kvstoretest"} {
		if forbidden(".", path) == "" {
			t.Errorf("the engine may import %s", path)
		}
		if why := forbidden(unitTests, path); why != "" {
			t.Errorf("the unit composition may not import %s: %s", path, why)
		}
	}
}
