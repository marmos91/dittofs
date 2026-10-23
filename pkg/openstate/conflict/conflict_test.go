package conflict

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// The access and deny table, cell by cell: a new open asking one access or
// denying one, against a held open denying one or holding one. Each open
// asks some data access, since one asking none is outside the check. The
// cells marked "—" pair a new open's access with a held open's access, or a
// deny with a deny, which never conflict.
func TestMatrix(t *testing.T) {
	read, write, del := entity.ReadData, entity.WriteData, entity.Delete
	classes := []entity.AccessMask{read, write, del}
	names := []string{"read", "write", "delete"}
	// other is a class that neither of two masks names.
	other := func(x, y entity.AccessMask) entity.AccessMask {
		for _, c := range classes {
			if c != x && c != y {
				return c
			}
		}
		panic("three classes")
	}
	for i, n := range classes {
		for j, h := range classes {
			// New asks n, held denies h: refused only on the diagonal.
			if got, want := Shares(n, 0, other(h, h), h), i != j; got != want {
				t.Errorf("new asks %s, held denies %s: shares %v, want %v", names[i], names[j], got, want)
			}
			// New denies n, held has h: refused only on the diagonal.
			if got, want := Shares(other(n, n), n, h, 0), i != j; got != want {
				t.Errorf("new denies %s, held has %s: shares %v, want %v", names[i], names[j], got, want)
			}
			// The cells marked "—".
			c := other(n, h)
			if !Shares(n, 0, h, 0) || !Shares(c, n, c, h) {
				t.Errorf("%s against %s: access against access, or deny against deny, conflicted", names[i], names[j])
			}
		}
	}
	// Execute is read and append is write; attributes are no class.
	if Shares(entity.Execute, 0, del, read) || Shares(entity.AppendData, 0, del, write) {
		t.Error("execute or append passed the deny of its class")
	}
	stat, all := entity.ReadAttributes|entity.WriteAttributes|entity.Synchronize, read|write|del
	if !Shares(stat, all, all, all) {
		t.Error("an attributes-only open denying all was refused beside an open holding and denying all")
	}
	if !Shares(all, all, stat, all) {
		t.Error("an open holding and denying all was refused beside an attributes-only open denying all")
	}
}
