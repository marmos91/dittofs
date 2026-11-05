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

	"github.com/cubbit/dittofs/pkg/block/journal/journaltest/prof"
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
		{"openat", "unix.Openat", got.openat, unix.Openat},
		{"fstatfs", "unix.Fstatfs", got.fstatfs, unix.Fstatfs},
		{"fstat", "unix.Fstat", got.fstat, unix.Fstat},
		{"statx", "unix.Statx", got.statx, unix.Statx},
		{"ioctlGetUint32", "unix.IoctlGetUint32", got.ioctlGetUint32, unix.IoctlGetUint32},
		{"getdents", "unix.Getdents", got.getdents, unix.Getdents},
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

// TestNoCallBesideTheTable fails on a golang.org/x/sys/unix,
// golang.org/x/sys/windows or syscall name used outside the table's files
// other than a constant or type the package passes as data, and on Sync or Fd
// taken from anything, an *os.File's own sync or its raw descriptor. On macOS
// the os package's File.Sync answers a full sync the filesystem refuses with a
// plain fsync, which RFC 1 §5.2 forbids. A test that swaps the table sees only
// calls made through it, so a call made beside it would go unseen. It reads
// every non-test file whatever its build tags, finds the packages by import
// path so a renamed import is caught, and refuses a dot or blank import of
// them, whose names it could not tell apart.
func TestNoCallBesideTheTable(t *testing.T) {
	prof.Watch(t, prof.Options{})

	constants := map[string]bool{
		"O_RDONLY":                 true,
		"O_DIRECTORY":              true,
		"O_CLOEXEC":                true,
		"EINTR":                    true,
		"EINVAL":                   true,
		"EOPNOTSUPP":               true,
		"FALLOC_FL_ALLOCATE_RANGE": true,
		"F_FULLFSYNC":              true,
		"F_PREALLOCATE":            true,
		"F_ALLOCATEALL":            true,
		"F_PEOFPOSMODE":            true,
		"Fstore_t":                 true,
		"Stat_t":                   true,
		"O_NOFOLLOW":               true,
		"O_NONBLOCK":               true,
		"AT_EMPTY_PATH":            true,
		"STATX_MNT_ID":             true,
		"FS_IOC_GETFLAGS":          true,
		"Statfs_t":                 true,
		"Statx_t":                  true,
		"ENOSYS":                   true,
		"DT_REG":                   true,
		"DT_DIR":                   true,
		"DT_LNK":                   true,
		"DT_FIFO":                  true,
		"DT_SOCK":                  true,
		"DT_CHR":                   true,
		"DT_BLK":                   true,
		"Dirent":                   true,

		// Windows: constants, the handle, overlapped and file information
		// types, the conversion of a path to UTF-16, which makes no system
		// call, and the errno type and ENOSPC a full volume is matched with.
		"GENERIC_READ":               true,
		"GENERIC_WRITE":              true,
		"FILE_SHARE_READ":            true,
		"FILE_SHARE_WRITE":           true,
		"FILE_SHARE_DELETE":          true,
		"CREATE_NEW":                 true,
		"OPEN_EXISTING":              true,
		"FILE_ATTRIBUTE_NORMAL":      true,
		"FILE_FLAG_BACKUP_SEMANTICS": true,
		"FileAllocationInfo":         true,
		"FileEndOfFileInfo":          true,
		"ERROR_INVALID_PARAMETER":    true,
		"ERROR_DISK_FULL":            true,
		"ERROR_HANDLE_DISK_FULL":     true,
		"Handle":                     true,
		"Overlapped":                 true,
		"ByHandleFileInformation":    true,
		"UTF16PtrFromString":         true,
		"Errno":                      true,
		"ENOSPC":                     true,
	}
	raw := map[string]bool{
		"golang.org/x/sys/unix":    true,
		"golang.org/x/sys/windows": true,
		"syscall":                  true,
	}
	tables := map[string]bool{
		"calls.go":         true,
		"calls_darwin.go":  true,
		"calls_windows.go": true,
	}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || tables[name] {
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

			if x, ok := sel.X.(*ast.Ident); ok && pkgs[x.Name] {
				if !constants[sel.Sel.Name] {
					t.Errorf("%s: %s.%s outside the table", fset.Position(sel.Pos()), x.Name, sel.Sel.Name)
				}

				return true
			}

			if sel.Sel.Name == "Sync" || sel.Sel.Name == "Fd" {
				t.Errorf("%s: .%s beside the table", fset.Position(sel.Pos()), sel.Sel.Name)
			}

			return true
		})
	}
}
