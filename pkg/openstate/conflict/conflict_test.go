package conflict

import (
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// The access and deny table, cell by cell: a new open asking one access or
// denying one, against a held open denying one or holding one. The cells
// marked "—" pair a new open's access with a held open's access, or a deny
// with a deny, which never conflict.
func TestMatrix(t *testing.T) {
	read, write, del := entity.ReadData, entity.WriteData, entity.Delete
	classes := []entity.AccessMask{read, write, del}
	names := []string{"read", "write", "delete"}
	for i, n := range classes {
		for j, h := range classes {
			// New asks n, held denies h: refused only on the diagonal.
			if got, want := Shares(n, 0, 0, h), i != j; got != want {
				t.Errorf("new asks %s, held denies %s: shares %v, want %v", names[i], names[j], got, want)
			}
			// New denies n, held has h: refused only on the diagonal.
			if got, want := Shares(0, n, h, 0), i != j; got != want {
				t.Errorf("new denies %s, held has %s: shares %v, want %v", names[i], names[j], got, want)
			}
			// The cells marked "—".
			if !Shares(n, 0, h, 0) || !Shares(0, n, 0, h) {
				t.Errorf("%s against %s: access against access, or deny against deny, conflicted", names[i], names[j])
			}
		}
	}
	// Execute is read and append is write; attributes are no class.
	if Shares(entity.Execute, 0, 0, read) || Shares(entity.AppendData, 0, 0, write) {
		t.Error("execute or append passed the deny of its class")
	}
	if !Shares(entity.ReadAttributes|entity.WriteAttributes|entity.Synchronize, 0, 0, read|write|del) {
		t.Error("an attributes-only open was refused by a deny mode")
	}
}
