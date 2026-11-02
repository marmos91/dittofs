package vfs_test

import (
	"reflect"
	"testing"

	"github.com/marmos91/dittofs/pkg/vfs"
)

// The calls that change nothing, written out by hand; every other Service
// method may change state.
var readOnly = map[vfs.Op]bool{
	vfs.OpPropose: true, vfs.OpTestState: true, vfs.OpLookup: true, vfs.OpReadDir: true,
	vfs.OpGetAttr: true, vfs.OpGetACL: true, vfs.OpXattrs: true, vfs.OpStreams: true,
	vfs.OpAccess: true, vfs.OpRead: true, vfs.OpSeek: true, vfs.OpTestLock: true,
	vfs.OpRoot: true, vfs.OpSecInfo: true, vfs.OpListShares: true, vfs.OpStatFS: true,
	vfs.OpQuota: true, vfs.OpSnapshots: true, vfs.OpRecord: true, vfs.OpSpell: true,
}

func TestMutates(t *testing.T) {
	svc := reflect.TypeFor[vfs.Service]()
	n := 0
	for op := vfs.Op(1); op.String() != "Op(?)"; op++ {
		n++
		if _, ok := svc.MethodByName(op.String()); !ok {
			t.Errorf("Op %d is %q, no Service method", op, op)
		}
		if got := vfs.Mutates(op); got == readOnly[op] {
			t.Errorf("Mutates(%v) = %v", op, got)
		}
	}
	if want := svc.NumMethod(); n != want {
		t.Fatalf("%d ops, want one per Service method: %d", n, want)
	}
}

// Identity is what a resolver caches, keyed on its proof, so nothing in it
// may name a client or a session: those travel in Call.
func TestIdentityNamesNoClient(t *testing.T) {
	forbidden := map[reflect.Type]bool{reflect.TypeFor[vfs.ClientID](): true, reflect.TypeFor[vfs.SessionRef](): true}
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		if forbidden[typ] {
			t.Errorf("Identity%s is a %v, which belongs in Call", path, typ)
		}
		switch typ.Kind() {
		case reflect.Struct:
			for i := range typ.NumField() {
				f := typ.Field(i)
				walk(path+"."+f.Name, f.Type)
			}
		case reflect.Slice, reflect.Array, reflect.Pointer:
			walk(path+"[]", typ.Elem())
		}
	}
	walk("", reflect.TypeFor[vfs.Identity]())
}
