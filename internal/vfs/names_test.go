package vfs

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
	"github.com/cubbit/dittofs/pkg/vfs/vfserr"
	"github.com/cubbit/dittofs/pkg/vfs/vfstest"
)

// names is a serving filesystem over the fixture's share with its one shard
// served, as the node's start serves it, so open state judges its changes.
type names struct {
	*shares
	admin vfs.Call
}

func newNames(t *testing.T) *names {
	t.Helper()
	return namesOver(t, newShares(t, nil, nil))
}

// newFoldNames is newNames over a share whose names fold as Windows folds
// them.
func newFoldNames(t *testing.T) *names {
	t.Helper()
	f := newFixtureOver(t, Deps{}, kvstoretest.NewFoldFS(t, memKV, entity.FoldUpcase))
	return namesOver(t, startShares(t, f, nil))
}

func namesOver(t *testing.T, s *shares) *names {
	t.Helper()
	return &names{shares: s, admin: proven(s.fs.Admin.Principal, inLab)}
}

func (n *names) create(t *testing.T, c vfs.Call, dir vfs.Handle, name string, k vfs.CreateKind) vfs.CreateResult {
	t.Helper()
	r, err := n.Create(t.Context(), c, dir, []byte(name), vfs.CreateArgs{Kind: k, Attrs: entity.Attrs{}.WithMode(0o755)})
	n.must(t, err)
	return r
}

// list is every name ReadDir lists in dir, from the cursor, a page of max at
// a time.
func (n *names) list(t *testing.T, c vfs.Call, dir vfs.Handle, from vfs.Cursor, max int) []string {
	t.Helper()
	var out []string
	for {
		p, err := n.ReadDir(t.Context(), c, dir, from, max, vfs.EntryHandle|vfs.EntryFile)
		n.must(t, err)
		for _, e := range p.Entries {
			out = append(out, string(e.Name))
		}
		if p.EOF {
			return out
		}
		from = p.Next
	}
}

// smbClient connects an SMB client and answers a call from it as p.
func (n *names) smbClient(t *testing.T, name string, p entity.Principal) vfs.Call {
	t.Helper()
	c := openstate.Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolSMB, Name: []byte(name), Boot: [8]byte{1}}}
	pr, err := n.tab.Clients().Propose(t.Context(), c, inLab)
	n.must(t, err)
	id, _, err := n.tab.Clients().Connect(t.Context(), c, &pr)
	n.must(t, err)
	call := proven(p, inLab)
	call.Protocol, call.Client = entity.ProtocolSMB, id
	return call
}

// open opens the file h names, named name in dir, as c, holding access.
func (n *names) open(t *testing.T, c vfs.Call, dir, h vfs.Handle, name string, access entity.AccessMask) openstate.Open {
	t.Helper()
	f, err := n.fs.Store.Files().Get(t.Context(), n.fs.Admin, h)
	n.must(t, err)
	who := openstate.Caller{Client: c.Client, Principal: c.Identity.Evidence.Principal, Protocol: c.Protocol}
	r, err := n.tab.Opens().Open(t.Context(), who, keyOf(f), openstate.OpenRequest{Via: fileOf(dir), Name: []byte(name), Access: access})
	n.must(t, err)
	return r.Open
}

// markDeletePending marks the directory name names in the share's root
// delete pending, through an SMB open of it that stays open.
func (n *names) markDeletePending(t *testing.T, name string, h vfs.Handle) {
	t.Helper()
	c := n.smbClient(t, "marker", n.fs.Admin.Principal)
	o := n.open(t, c, n.fs.Root, h, name, entity.ReadData|entity.Delete)
	yes := true
	who := openstate.Caller{Client: c.Client, Principal: n.fs.Admin.Principal, Protocol: c.Protocol}
	n.must(t, n.tab.Opens().Update(t.Context(), who, o.ID, openstate.OpenUpdate{Delete: &yes}))
}

// Names resolve, list, link, rename and go as the store keeps them, ".."
// at a share's root is the root, and the errors each call can meet reach
// the caller as the service's own.
func TestNamesAddAndRemoveEntries(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root := n.fs.Root
	d := n.create(t, n.admin, root, "d", vfs.CreateDir)
	f := n.create(t, n.admin, d.Handle, "f", vfs.CreateRegular)

	if h, got, err := n.Lookup(ctx, n.admin, d.Handle, []byte("f")); err != nil || !bytes.Equal(h, f.Handle) || got.ID != f.File.ID {
		t.Errorf("Lookup of a created name: %v, %v, %v", h, got.ID, err)
	}
	if h, _, err := n.Lookup(ctx, n.admin, d.Handle, []byte("..")); err != nil || !bytes.Equal(h, root) {
		t.Errorf(`Lookup of ".." in d: %v, %v`, h, err)
	}
	if h, _, err := n.Lookup(ctx, n.admin, root, []byte("..")); err != nil || !bytes.Equal(h, root) {
		t.Errorf(`Lookup of ".." at the share's root: %v, %v`, h, err)
	}
	if h, _, err := n.Lookup(ctx, n.admin, d.Handle, []byte(".")); err != nil || !bytes.Equal(h, d.Handle) {
		t.Errorf(`Lookup of "." in d: %v, %v`, h, err)
	}
	for _, name := range []string{".", ".."} {
		if _, _, err := n.Lookup(ctx, n.admin, f.Handle, []byte(name)); !errors.Is(err, vfs.ErrNotDir) {
			t.Errorf("Lookup of %q in a regular file: %v, want ErrNotDir", name, err)
		}
	}
	if _, _, err := n.Lookup(ctx, n.admin, d.Handle, []byte("nope")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("Lookup of no name: %v", err)
	}

	l, err := n.Link(ctx, n.admin, d.Handle, []byte("g"), f.Handle)
	if err != nil || l.File.Nlink != 2 || !bytes.Equal(l.Handle, f.Handle) {
		t.Errorf("Link: %+v, %v", l, err)
	}
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: d.Handle}, []byte("g"), d.Handle, []byte("h"), false); err != nil {
		t.Errorf("Rename: %v", err)
	}
	if got := n.list(t, n.admin, d.Handle, 0, 1); !slices.Equal(slices.Sorted(slices.Values(got)), []string{"f", "h"}) {
		t.Errorf("listing after link and rename, a page of one at a time: %q", got)
	}
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: d.Handle}, []byte("h"), d.Handle, []byte("f"), true); !errors.Is(err, vfs.ErrExist) {
		t.Errorf("Rename onto a name with noReplace: %v", err)
	}
	if _, err := n.Rmdir(ctx, n.admin, vfs.Target{H: d.Handle}, []byte("f")); !errors.Is(err, vfs.ErrNotDir) {
		t.Errorf("Rmdir of a file: %v", err)
	}
	if _, err := n.Rmdir(ctx, n.admin, vfs.Target{H: root}, []byte("d")); !errors.Is(err, vfs.ErrNotEmpty) {
		t.Errorf("Rmdir of a directory with entries: %v", err)
	}
	if _, err := n.Create(ctx, n.admin, d.Handle, []byte("a/b"), vfs.CreateArgs{Kind: vfs.CreateRegular}); !errors.Is(err, vfs.ErrInvalid) {
		t.Errorf("Create of a name with a slash: %v", err)
	}
	if _, err := n.Create(ctx, n.admin, d.Handle, bytes.Repeat([]byte("x"), 256), vfs.CreateArgs{Kind: vfs.CreateRegular}); !errors.Is(err, vfs.ErrNameTooLong) {
		t.Errorf("Create of a 256-byte name: %v", err)
	}

	other := n.addShare(t, "other", "/other")
	n.must(t, n.cp.SetGrant(ctx, entity.ShareGrant{Share: other, Principal: n.fs.Admin.Principal, Access: entity.AccessAdmin}))
	oroot, _, err := n.Root(ctx, n.admin, vfs.ShareRef{Name: "other"})
	n.must(t, err)
	var cross *vfs.CrossError
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: d.Handle}, []byte("f"), oroot, []byte("f"), false); !errors.As(err, &cross) || cross.Kind != vfs.CrossShare {
		t.Errorf("Rename into another share: %v", err)
	}
	if _, err := n.Link(ctx, n.admin, oroot, []byte("f"), f.Handle); !errors.As(err, &cross) || cross.Kind != vfs.CrossShare {
		t.Errorf("Link of another share's file: %v", err)
	}
	pseudo, _, err := n.Root(ctx, n.admin, vfs.ShareRef{})
	n.must(t, err)
	if _, err := n.Create(ctx, n.admin, pseudo, []byte("x"), vfs.CreateArgs{Kind: vfs.CreateDir}); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("Create in a pseudo directory: %v", err)
	}

	for _, name := range []string{"f", "h"} {
		if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: d.Handle}, []byte(name)); err != nil {
			t.Errorf("Unlink %s: %v", name, err)
		}
	}
	if _, err := n.Rmdir(ctx, n.admin, vfs.Target{H: root}, []byte("d")); err != nil {
		t.Errorf("Rmdir of the emptied directory: %v", err)
	}
	if got := n.list(t, n.admin, root, 0, 0); len(got) != 0 {
		t.Errorf("the root after every name went: %q", got)
	}
}

// A file whose last name goes is released at once when nothing holds it
// open, and kept for its last close when an open does.
func TestTheLastUnlinkReleasesOnlyAFileNoOpenHolds(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	content := n.engine
	held := n.create(t, n.admin, n.fs.Root, "held", vfs.CreateRegular)
	free := n.create(t, n.admin, n.fs.Root, "free", vfs.CreateRegular)
	for _, f := range []vfs.File{held.File, free.File} {
		_, err := content.Write(ctx, f.Share, f.ID, 0, []byte("bytes"), WriteOpts{})
		n.must(t, err)
	}
	smb := n.smbClient(t, "holder", n.fs.Admin.Principal)
	n.open(t, smb, n.fs.Root, held.Handle, "held", entity.ReadData)
	for _, name := range []string{"held", "free"} {
		_, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte(name))
		n.must(t, err)
	}
	content.mu.Lock()
	defer content.mu.Unlock()
	if _, ok := content.files[held.File.ID]; !ok {
		t.Error("the content of a file an open holds was released at its unlink")
	}
	if _, ok := content.files[free.File.ID]; ok {
		t.Error("the content of a file nothing holds was kept past its last unlink")
	}
}

// An open made through a name follows the name's rename: a delete on close
// marked through it after the rename removes the new name.
func TestARenamedNamesOpensFollowIt(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	a := n.create(t, n.admin, n.fs.Root, "a", vfs.CreateRegular)
	d := n.create(t, n.admin, n.fs.Root, "d", vfs.CreateDir)
	smb := n.smbClient(t, "renamer", n.fs.Admin.Principal)
	o := n.open(t, smb, n.fs.Root, a.Handle, "a", entity.ReadData|entity.Delete)
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("a"), d.Handle, []byte("b"), false); err != nil {
		t.Fatal(err)
	}
	who := openstate.Caller{Client: smb.Client, Principal: n.fs.Admin.Principal, Protocol: smb.Protocol}
	yes := true
	n.must(t, n.tab.Opens().Update(ctx, who, o.ID, openstate.OpenUpdate{Delete: &yes}))
	r, err := n.tab.Opens().Close(ctx, who, openstate.OpenRef{Kind: openstate.RefOpened, State: o.Ref, Open: o.ID}, openstate.Seqs{})
	n.must(t, err)
	if r.Unlink == nil || r.Unlink.Parent != d.File.ID || string(r.Unlink.Name) != "b" {
		t.Errorf("the delete on close after the rename unlinks %+v, want b in d", r.Unlink)
	}
}

// V28: a create, a link and a rename into a directory marked delete on close
// are refused, over SMB and over NFS, and add no name.
func TestADeletePendingDirectoryTakesNoNewName(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	doomed := n.create(t, n.admin, n.fs.Root, "doomed", vfs.CreateDir)
	f := n.create(t, n.admin, n.fs.Root, "f", vfs.CreateRegular)
	n.markDeletePending(t, "doomed", doomed.Handle)
	nfs := n.admin
	nfs.Protocol = entity.ProtocolNFSv4
	for proto, c := range map[string]vfs.Call{"smb": n.smbClient(t, "adder", n.fs.Admin.Principal), "nfs": nfs} {
		calls := map[string]error{}
		_, calls["create"] = n.Create(ctx, c, doomed.Handle, []byte("new"), vfs.CreateArgs{Kind: vfs.CreateRegular})
		_, calls["link"] = n.Link(ctx, c, doomed.Handle, []byte("new"), f.Handle)
		_, calls["rename"] = n.Rename(ctx, c, vfs.Target{H: n.fs.Root}, []byte("f"), doomed.Handle, []byte("new"), false)
		for op, err := range calls {
			if !errors.Is(err, vfs.ErrDeletePending) {
				t.Errorf("%s over %s into the delete-pending directory: %v", op, proto, err)
			}
		}
	}
	if _, _, err := n.Lookup(ctx, n.admin, doomed.Handle, []byte("new")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("a refused change left a name: %v", err)
	}
}

// V3: a create refused the add right is refused ErrAccess, though its
// parent is delete pending too, and charges no inode and adds no entry.
func TestACreateRefusedAddFileChangesNothing(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	bob := n.user(t, "bob", "", n.share, entity.AccessReadWrite)
	doomed := n.create(t, n.admin, n.fs.Root, "doomed", vfs.CreateDir)
	n.markDeletePending(t, "doomed", doomed.Handle)
	before, err := n.fs.Store.Capacity().Usage(ctx, n.share)
	n.must(t, err)
	if _, err := n.Create(ctx, proven(bob.Principal, inLab), doomed.Handle, []byte("x"), vfs.CreateArgs{Kind: vfs.CreateRegular}); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's create in admin's 0755 delete-pending directory: %v, want ErrAccess", err)
	}
	after, err := n.fs.Store.Capacity().Usage(ctx, n.share)
	n.must(t, err)
	if after != before {
		t.Errorf("a refused create changed the share's usage from %+v to %+v", before, after)
	}
	if _, _, err := n.Lookup(ctx, n.admin, doomed.Handle, []byte("x")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("a refused create left its name: %v", err)
	}
}

// V31: each name change answers its directories' attributes either side of
// it, the After past the Before, and over NFSv3 no directory's Before.
func TestANameChangeAnswersItsDirectoriesWCC(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root := vfs.Target{H: n.fs.Root}
	d := n.create(t, n.admin, n.fs.Root, "d", vfs.CreateDir)
	f := n.create(t, n.admin, n.fs.Root, "f", vfs.CreateRegular)
	changes := map[string]func(c vfs.Call) ([]vfs.WCC, error){
		"create": func(c vfs.Call) ([]vfs.WCC, error) {
			r, err := n.Create(ctx, c, n.fs.Root, []byte("c"), vfs.CreateArgs{Kind: vfs.CreateDir})
			return []vfs.WCC{r.Dir}, err
		},
		"link": func(c vfs.Call) ([]vfs.WCC, error) {
			r, err := n.Link(ctx, c, n.fs.Root, []byte("l"), f.Handle)
			return []vfs.WCC{r.Dir}, err
		},
		"rename": func(c vfs.Call) ([]vfs.WCC, error) {
			r, err := n.Rename(ctx, c, root, []byte("l"), d.Handle, []byte("l"), false)
			return []vfs.WCC{r.From, r.To}, err
		},
		"unlink": func(c vfs.Call) ([]vfs.WCC, error) {
			r, err := n.Unlink(ctx, c, vfs.Target{H: d.Handle}, []byte("l"))
			return []vfs.WCC{r.Dir}, err
		},
		"rmdir": func(c vfs.Call) ([]vfs.WCC, error) {
			r, err := n.Rmdir(ctx, c, root, []byte("c"))
			return []vfs.WCC{r.Dir}, err
		},
	}
	order := []string{"create", "link", "rename", "unlink", "rmdir"}
	nfs3 := n.admin
	nfs3.Protocol = entity.ProtocolNFSv3
	for _, c := range []vfs.Call{n.admin, nfs3} {
		for _, op := range order {
			ws, err := changes[op](c)
			if err != nil {
				t.Fatalf("%s over %v: %v", op, c.Protocol, err)
			}
			for _, w := range ws {
				switch {
				case c.Protocol == entity.ProtocolNFSv3 && w.Before != nil:
					t.Errorf("%s over NFSv3 answered a directory's Before", op)
				case c.Protocol != entity.ProtocolNFSv3 && (w.Before == nil || w.Before.Version.Counter() >= w.After.Version.Counter()):
					t.Errorf("%s: Before %+v, After version %v", op, w.Before, w.After.Version)
				case w.After.ID.IsZero():
					t.Errorf("%s answered no After", op)
				}
			}
		}
	}
}

// V31: creates racing in one directory each answer a Before and After from
// one hold of its lock: in commit order, each one's After is the next one's
// Before, and no two share a Before.
func TestRacingCreatesAnswerWCCFromOneHold(t *testing.T) {
	n := newNames(t)
	const racers = 16
	var wg sync.WaitGroup
	wccs := make([]vfs.WCC, racers)
	for i := range racers {
		wg.Go(func() {
			r, err := n.Create(t.Context(), n.admin, n.fs.Root, []byte{'a' + byte(i)}, vfs.CreateArgs{Kind: vfs.CreateRegular})
			if err != nil {
				t.Error(err)
			}
			wccs[i] = r.Dir
		})
	}
	wg.Wait()
	slices.SortFunc(wccs, func(a, b vfs.WCC) int { return bytes.Compare(a.Before.Version[:], b.Before.Version[:]) })
	for i := 1; i < racers; i++ {
		if wccs[i].Before.Version != wccs[i-1].After.Version {
			t.Fatalf("create %d's Before %v is not create %d's After %v: another create landed between one's Before and After",
				i, wccs[i].Before.Version.Counter(), i-1, wccs[i-1].After.Version.Counter())
		}
	}
}

// V40: a create held between its admission and its commit holds the share's
// removal until it answers, and a create counted after the mark is refused
// stale.
func TestARemovalJoinsACreateAdmittedBeforeIt(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root := lockorder.Key(fileOf(n.fs.Root))
	held, err := n.locks.Take(ctx, lockorder.Set{Accept: []lockorder.Key{root}})
	n.must(t, err)
	unlock := sync.OnceFunc(held)
	defer unlock()
	created := make(chan error)
	go func() {
		_, err := n.Create(context.Background(), n.admin, n.fs.Root, []byte("held"), vfs.CreateArgs{Kind: vfs.CreateRegular})
		created <- err
	}()
	// The create holds the root's admission lock only once it is admitted,
	// and then waits at the accept lock held above: the mark below comes
	// after its admission, whatever the scheduling.
	n.admitted(t, root, created)
	n.must(t, n.cp.DeleteShare(ctx, n.share, entity.SnapshotsRefuse))
	joined := make(chan error)
	go func() { joined <- n.JoinShare(context.Background(), n.share) }()
	select {
	case err := <-joined:
		t.Fatalf("the removal's join returned while an admitted create waited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := n.Create(ctx, n.admin, n.fs.Root, []byte("late"), vfs.CreateArgs{Kind: vfs.CreateRegular}); !errors.Is(err, vfs.ErrStale) {
		t.Errorf("a create counted after the mark: %v", err)
	}
	unlock()
	if err := <-created; err != nil {
		t.Logf("the held create, refused by the mark it met in the store: %v", err)
	}
	if err := <-joined; err != nil {
		t.Error(err)
	}
	if _, _, err := n.fs.Store.Namespace().Lookup(ctx, n.fs.Admin, n.fs.Root, []byte("late")); err == nil {
		t.Error("the create refused after the mark committed its name")
	}
}

// admitted returns once a call holds the admission lock of k, as a change
// does once it is admitted, and fails the test if the call answers on done
// first or is not admitted within a second. It probes the lock without
// waiting on it.
func (n *names) admitted(t *testing.T, k lockorder.Key, done <-chan error) {
	t.Helper()
	now, cancel := context.WithCancel(context.Background())
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("the call was not admitted within a second")
		}
		release, err := n.locks.Take(now, lockorder.Set{Admit: []lockorder.Key{k}})
		if err != nil {
			return
		}
		release()
		select {
		case err := <-done:
			t.Fatalf("the call answered %v before it was admitted", err)
		default:
			runtime.Gosched()
		}
	}
}

// counted waits, a second at most, for a call to be counted against the
// share, then a little more for it to make its first, unlocked lookup.
func (n *names) counted(t *testing.T) {
	t.Helper()
	for end := time.Now().Add(time.Second); time.Now().Before(end); time.Sleep(time.Millisecond) {
		n.mu.Lock()
		j := n.joins[n.share]
		n.mu.Unlock()
		if j != nil {
			break
		}
	}
	time.Sleep(20 * time.Millisecond)
}

// hold takes the accept lock of the file h names, as a change to it would,
// and answers its release, which may be called again.
func (n *names) hold(t *testing.T, h vfs.Handle) func() {
	t.Helper()
	unlock, err := n.locks.Take(t.Context(), lockorder.Set{Accept: []lockorder.Key{lockorder.Key(fileOf(h))}})
	n.must(t, err)
	release := sync.OnceFunc(unlock)
	t.Cleanup(release)
	return release
}

// waits fails if done answers within 50ms: the call is still waiting.
func waits(t *testing.T, what string, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s answered %v while a lock it needs was held", what, err)
	case <-time.After(50 * time.Millisecond):
	}
}

// An unlink whose name comes to name another file while it waits for its
// locks starts over, and so waits for that file's lock too; a rename whose
// target name does the same, likewise.
func TestANameThatMovesUnderItsLocksStartsTheChangeOver(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root := n.fs.Root
	for _, c := range []struct {
		name   string
		change func() error
	}{
		{"an unlink", func() error {
			_, err := n.Unlink(context.Background(), n.admin, vfs.Target{H: root}, []byte("x"))
			return err
		}},
		{"a rename onto it", func() error {
			_, err := n.Rename(context.Background(), n.admin, vfs.Target{H: root}, []byte("src"), root, []byte("x"), false)
			return err
		}},
	} {
		n.create(t, n.admin, root, "src", vfs.CreateRegular)
		n.create(t, n.admin, root, "x", vfs.CreateRegular)
		later := n.create(t, n.admin, root, "y", vfs.CreateRegular)
		releaseRoot := n.hold(t, root)
		done := make(chan error, 1)
		go func() { done <- c.change() }()
		n.counted(t)
		_, err := n.fs.Store.Namespace().Rename(ctx, n.fs.Admin, root, []byte("y"), root, []byte("x"))
		n.must(t, err)
		releaseLater := n.hold(t, later.Handle)
		releaseRoot()
		waits(t, c.name+" of a name now naming another file", done)
		releaseLater()
		if err := <-done; err != nil {
			t.Errorf("%s, once the lock was released: %v", c.name, err)
		}
		for _, name := range []string{"src", "x"} {
			_, _ = n.Unlink(ctx, n.admin, vfs.Target{H: root}, []byte(name))
		}
	}
}

// A rename moving a directory to another parent drains the directory's fold
// admission: it waits for a change writing the directory's deltas.
func TestARenameMovingADirectoryDrainsIt(t *testing.T) {
	n := newNames(t)
	d := n.create(t, n.admin, n.fs.Root, "d", vfs.CreateDir)
	e := n.create(t, n.admin, n.fs.Root, "e", vfs.CreateDir)
	unlock, err := n.locks.Take(t.Context(), lockorder.Set{Enter: []lockorder.Key{lockorder.Key(d.File.ID)}})
	n.must(t, err)
	release := sync.OnceFunc(unlock)
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := n.Rename(context.Background(), n.admin, vfs.Target{H: n.fs.Root}, []byte("d"), e.Handle, []byte("d"), false)
		done <- err
	}()
	waits(t, "the rename moving d", done)
	release()
	if err := <-done; err != nil {
		t.Error(err)
	}
}

// Open state is asked before a name goes: an unlink a lease of another
// client conflicts with is ErrDelay, a rename of a delete-pending file
// ErrDeletePending, and an SMB rename onto a file another open holds
// ErrAccess; each leaves every name where it was.
func TestOpenStateIsAskedBeforeANameGoes(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root := n.fs.Root
	leasedFile := n.create(t, n.admin, root, "leased", vfs.CreateRegular)
	holder := n.smbClient(t, "holder", n.fs.Admin.Principal)
	leased(t, n.shares, holder.Client, n.fs.Admin.Principal, 1, leasedFile.File.ID, 1)
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: root}, []byte("leased")); !errors.Is(err, vfs.ErrDelay) {
		t.Errorf("an unlink under another client's lease: %v, want ErrDelay", err)
	}
	doomed := n.create(t, n.admin, root, "doomed", vfs.CreateRegular)
	n.markDeletePending(t, "doomed", doomed.Handle)
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: root}, []byte("doomed"), root, []byte("saved"), false); !errors.Is(err, vfs.ErrDeletePending) {
		t.Errorf("a rename of a delete-pending file: %v, want ErrDeletePending", err)
	}
	target := n.create(t, n.admin, root, "target", vfs.CreateRegular)
	n.create(t, n.admin, root, "src", vfs.CreateRegular)
	n.open(t, holder, root, target.Handle, "target", entity.ReadData)
	renamer := n.smbClient(t, "renamer", n.fs.Admin.Principal)
	if _, err := n.Rename(ctx, renamer, vfs.Target{H: root}, []byte("src"), root, []byte("target"), false); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("an SMB rename onto a file another open holds: %v, want ErrAccess", err)
	}
	for name, h := range map[string]vfs.Handle{"leased": leasedFile.Handle, "doomed": doomed.Handle, "target": target.Handle} {
		if got, _, err := n.Lookup(ctx, n.admin, root, []byte(name)); err != nil || !bytes.Equal(got, h) {
			t.Errorf("%s after its refused change: %v, %v", name, got, err)
		}
	}
}

// committing records each commit an unlink tells open state.
type committing struct {
	openstate.Opens
	mu  sync.Mutex
	got []bool
}

func (c *committing) Durable(ctx context.Context, f openstate.FileKey) ([]entity.OpenRecord, func(bool), error) {
	recs, commit, err := c.Opens.Durable(ctx, f)
	return recs, func(took bool) {
		c.mu.Lock()
		c.got = append(c.got, took)
		c.mu.Unlock()
		commit(took)
	}, err
}

// An unlink tells open state its opens went durable only when it took the
// file's last name, and that unlink writes the open's record to the store.
func TestOnlyTheLastNameMakesTheOpensDurable(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	rec := &committing{Opens: n.opens}
	n.opens = rec
	f := n.create(t, n.admin, n.fs.Root, "f", vfs.CreateRegular)
	_, err := n.Link(ctx, n.admin, n.fs.Root, []byte("g"), f.Handle)
	n.must(t, err)
	n.open(t, n.smbClient(t, "holder", n.fs.Admin.Principal), n.fs.Root, f.Handle, "f", entity.ReadData)
	for i, name := range []string{"f", "g"} {
		_, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte(name))
		n.must(t, err)
		recs, err := n.fs.Store.Opens().Of(ctx, n.share, f.File.ID)
		n.must(t, err)
		if len(recs) != i {
			t.Errorf("durable opens after unlinking %s: %v, want %d", name, recs, i)
		}
	}
	if !slices.Equal(rec.got, []bool{false, true}) {
		t.Errorf("the unlinks of a file's two names told open state %v, want [false true]", rec.got)
	}
}

// A caller who may list a directory but not traverse it gets its names
// alone from ReadDir, no handle and no File, whichever of the two it asks
// for, so no entry hands them what Lookup refuses.
func TestReadDirWithoutTraverseListsNamesAlone(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	bob := proven(n.user(t, "bob", "", n.share, entity.AccessReadWrite).Principal, inLab)
	d, err := n.Create(ctx, n.admin, n.fs.Root, []byte("d"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o744)})
	n.must(t, err)
	n.create(t, n.admin, d.Handle, "secret", vfs.CreateRegular)
	if _, _, err := n.Lookup(ctx, bob, d.Handle, []byte("secret")); !errors.Is(err, vfs.ErrAccess) {
		t.Fatalf("bob's Lookup in admin's 0744 directory: %v, want ErrAccess", err)
	}
	for _, want := range []vfs.EntryFields{vfs.EntryHandle | vfs.EntryFile, vfs.EntryHandle} {
		p, err := n.ReadDir(ctx, bob, d.Handle, 0, 0, want)
		n.must(t, err)
		if len(p.Entries) != 1 || string(p.Entries[0].Name) != "secret" {
			t.Fatalf("bob's ReadDir of the 0744 directory: %+v, want secret", p.Entries)
		}
		if e := p.Entries[0]; e.Handle != nil || e.File != nil {
			t.Errorf("bob's ReadDir asking %v answered secret's handle %v and File %v without traverse", want, e.Handle, e.File)
		}
	}
}

// A rename between two names of one file changes nothing, as POSIX has it:
// both names stay, and an open made through the source keeps its name, so a
// delete on close marked through it removes the source's name, not the
// target's.
func TestARenameBetweenTwoNamesOfOneFileLeavesItsOpensAlone(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	root := n.fs.Root
	a := n.create(t, n.admin, root, "a", vfs.CreateRegular)
	_, err := n.Link(ctx, n.admin, root, []byte("b"), a.Handle)
	n.must(t, err)
	smb := n.smbClient(t, "opener", n.fs.Admin.Principal)
	o := n.open(t, smb, root, a.Handle, "a", entity.ReadData|entity.Delete)
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: root}, []byte("a"), root, []byte("b"), false); err != nil {
		t.Fatalf("rename a to b, two names of one file: %v", err)
	}
	for _, name := range []string{"a", "b"} {
		if h, _, err := n.Lookup(ctx, n.admin, root, []byte(name)); err != nil || !bytes.Equal(h, a.Handle) {
			t.Errorf("%s after the rename: %v, %v", name, h, err)
		}
	}
	who := openstate.Caller{Client: smb.Client, Principal: n.fs.Admin.Principal, Protocol: smb.Protocol}
	yes := true
	n.must(t, n.tab.Opens().Update(ctx, who, o.ID, openstate.OpenUpdate{Delete: &yes}))
	r, err := n.tab.Opens().Close(ctx, who, openstate.OpenRef{Kind: openstate.RefOpened, State: o.Ref, Open: o.ID}, openstate.Seqs{})
	n.must(t, err)
	if r.Unlink == nil || r.Unlink.Parent != fileOf(root) || string(r.Unlink.Name) != "a" {
		t.Errorf("the delete on close after the rename unlinks %+v, want a in the root", r.Unlink)
	}
}

// Every right a rename needs is decided before open state is asked: a
// rename the sticky rule refuses on the file it would replace recalls no
// lease on the file it would move.
func TestARefusedRenameRecallsNoLease(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	bob := proven(n.user(t, "bob", "", n.share, entity.AccessReadWrite).Principal, inLab)
	tmp, err := n.Create(ctx, n.admin, n.fs.Root, []byte("tmp"), vfs.CreateArgs{Kind: vfs.CreateDir, Attrs: entity.Attrs{}.WithMode(0o1777)})
	n.must(t, err)
	src, err := n.Create(ctx, bob, tmp.Handle, []byte("src"), vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o666)})
	n.must(t, err)
	n.create(t, n.admin, tmp.Handle, "target", vfs.CreateRegular)
	cb := &vfstest.RecordingCallbacks{}
	holder, _, err := n.Connect(ctx, signedIn(n.fs.Admin.Principal, 1), smb("holder", 1), cb)
	n.must(t, err)
	leased(t, n.shares, holder, n.fs.Admin.Principal, 1, src.File.ID, 1)
	if _, err := n.Rename(ctx, bob, vfs.Target{H: tmp.Handle}, []byte("src"), tmp.Handle, []byte("target"), false); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's rename onto admin's file in a sticky directory: %v, want ErrAccess", err)
	}
	none(t, "the lease holder", cb)
}

// A ReadDir page holds max entries at most, the last page what is left, and
// the pages list every name once.
func TestReadDirPagesHoldMaxEntries(t *testing.T) {
	n := newNames(t)
	var want []string
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		n.create(t, n.admin, n.fs.Root, name, vfs.CreateRegular)
		want = append(want, name)
	}
	var sizes []int
	var got []string
	for from := vfs.Cursor(0); ; {
		p, err := n.ReadDir(t.Context(), n.admin, n.fs.Root, from, 2, 0)
		n.must(t, err)
		sizes = append(sizes, len(p.Entries))
		for _, e := range p.Entries {
			got = append(got, string(e.Name))
		}
		if p.EOF {
			break
		}
		from = p.Next
	}
	if !slices.Equal(sizes, []int{2, 2, 1}) {
		t.Errorf("five entries in pages of two came back in pages of %v, want [2 2 1]", sizes)
	}
	if !slices.Equal(slices.Sorted(slices.Values(got)), want) {
		t.Errorf("the pages listed %q, want %q", got, want)
	}
}

// On a share whose names fold, a rename changing only a name's case is the
// entry's respelling, not a replace, so noReplace lets it through and the
// listing answers the new spelling.
func TestACaseOnlyRenameIsNotRefusedAsAReplace(t *testing.T) {
	n := newFoldNames(t)
	root := n.fs.Root
	n.create(t, n.admin, root, "Foo", vfs.CreateRegular)
	if _, err := n.Rename(t.Context(), n.admin, vfs.Target{H: root}, []byte("Foo"), root, []byte("foo"), true); err != nil {
		t.Fatalf("rename Foo to foo with noReplace: %v", err)
	}
	if got := n.list(t, n.admin, root, 0, 0); !slices.Equal(got, []string{"foo"}) {
		t.Errorf("the listing after the respelling: %q, want [foo]", got)
	}
}

// A rename of a name to its own spelling changes nothing, so it recalls no
// lease on the file.
func TestARenameToTheSameSpellingRecallsNoLease(t *testing.T) {
	n := newFoldNames(t)
	ctx := t.Context()
	root := n.fs.Root
	a := n.create(t, n.admin, root, "a", vfs.CreateRegular)
	cb := &vfstest.RecordingCallbacks{}
	holder, _, err := n.Connect(ctx, signedIn(n.fs.Admin.Principal, 1), smb("holder", 1), cb)
	n.must(t, err)
	leased(t, n.shares, holder, n.fs.Admin.Principal, 1, a.File.ID, 1)
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: root}, []byte("a"), root, []byte("a"), false); err != nil {
		t.Errorf("rename a to a: %v", err)
	}
	none(t, "the lease holder", cb)
}

// An unlink, and a rename over a file, whose release fails still succeed:
// the name is gone. The file's pending release stays for the next start,
// whose recovery runs it.
func TestARemovedNameSucceedsWhateverItsReleaseAnswers(t *testing.T) {
	n := newNames(t)
	ctx := t.Context()
	a := n.create(t, n.admin, n.fs.Root, "a", vfs.CreateRegular)
	n.create(t, n.admin, n.fs.Root, "b", vfs.CreateRegular)
	c := n.create(t, n.admin, n.fs.Root, "c", vfs.CreateRegular)
	for _, f := range []vfs.File{a.File, c.File} {
		n.stored(t, f.ID)
	}
	n.engine.mu.Lock()
	n.engine.deleteErr = errors.New("the journal is read-only")
	n.engine.mu.Unlock()
	if _, err := n.Unlink(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("a")); err != nil {
		t.Errorf("an unlink whose release failed: %v, want success", err)
	}
	if _, err := n.Rename(ctx, n.admin, vfs.Target{H: n.fs.Root}, []byte("b"), n.fs.Root, []byte("c"), false); err != nil {
		t.Errorf("a rename over a file whose release failed: %v, want success", err)
	}
	n.engine.mu.Lock()
	n.engine.deleteErr = nil
	n.engine.mu.Unlock()
	g := n.restart(t)
	g.must(t, g.Start(ctx))
	for _, f := range []vfs.File{a.File, c.File} {
		if !g.released(f.ID, 11*time.Second) {
			t.Errorf("file %v's release that failed at its name's removal never ran after a restart", f.ID)
		}
	}
}

// pendingRelease is n with the fake engine leaving the first release pending
// on the channel it answers and committing every later one, a recording mark
// in place of open state's, and a stored file unlinked, so that its release
// is that first one. The unlink answers success with its release pending.
func pendingRelease(t *testing.T) (*names, *failMark, chan error, entity.FileID) {
	t.Helper()
	n := newNames(t)
	retry := make(chan error, 1)
	var first sync.Once
	n.engine.pending = func() <-chan error {
		var r <-chan error
		first.Do(func() { r = retry })
		return r
	}
	m := &failMark{}
	n.marks = m
	f := n.create(t, n.admin, n.fs.Root, "f", vfs.CreateRegular).File
	n.stored(t, f.ID)
	if _, err := n.Unlink(t.Context(), n.admin, vfs.Target{H: n.fs.Root}, []byte("f")); err != nil {
		t.Fatalf("an unlink whose release is pending: %v, want success", err)
	}
	return n, m, retry, f.ID
}

// ended is the outcomes m's done was called with, once they number want or a
// second has passed.
func (m *failMark) ended(want int) []bool {
	for range 100 {
		m.mu.Lock()
		got := slices.Clone(m.got)
		m.mu.Unlock()
		if len(got) >= want {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.got)
}

// RFC 17 §5.3: a pending release keeps its mark until its outcome, which
// goes to the mark's done when the release committed or a holder aborted
// it; a clone's delay ends the mark and runs the release again, marked anew;
// any other outcome keeps the mark for the next start's recovery.
func TestAPendingReleasesOutcomeDecidesItsMark(t *testing.T) {
	for _, tc := range []struct {
		name    string
		outcome error
		marks   []bool // the done calls, in order
		deletes int
	}{
		{"committed", nil, []bool{true}, 1},
		{"aborted by a holder", vfserr.ErrFileOpen, []bool{false}, 1},
		{"held by a clone", &vfs.Delay{Cause: vfs.DelayClone}, []bool{false, true}, 2},
		{"delayed by a commit", &vfs.Delay{Cause: vfs.DelayCommit}, nil, 1},
		{"ended by the engine", vfs.ErrUnavailable, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, m, retry, f := pendingRelease(t)
			if got := m.ended(0); len(got) != 0 {
				t.Fatalf("the mark ended %v while its release is pending, want kept", got)
			}
			retry <- tc.outcome
			got := m.ended(len(tc.marks))
			if len(tc.marks) == 0 {
				time.Sleep(50 * time.Millisecond)
				got = m.ended(0)
			}
			if !slices.Equal(got, tc.marks) {
				t.Errorf("the mark ended %v, want %v", got, tc.marks)
			}
			if d := n.engine.deleted(); d != tc.deletes {
				t.Errorf("%d Deletes, want %d", d, tc.deletes)
			}
			if tc.deletes == 2 && n.held(f) {
				t.Error("the release run again after a clone's delay left the content held")
			}
		})
	}
}

// RFC 17 §5.3: a release that fails within its call keeps its mark unless a
// holder aborted it, and the name's removal succeeds either way.
func TestAReleaseFailingWithinItsCallKeepsItsMark(t *testing.T) {
	for _, tc := range []struct {
		name  string
		err   error
		marks []bool
	}{
		{"aborted by a holder", vfserr.ErrFileOpen, []bool{false}},
		{"failed", errors.New("the journal is read-only"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newNames(t)
			m := &failMark{}
			n.marks = m
			n.create(t, n.admin, n.fs.Root, "f", vfs.CreateRegular)
			n.engine.mu.Lock()
			n.engine.deleteErr = tc.err
			n.engine.mu.Unlock()
			if _, err := n.Unlink(t.Context(), n.admin, vfs.Target{H: n.fs.Root}, []byte("f")); err != nil {
				t.Fatalf("an unlink whose release failed: %v, want success", err)
			}
			if got := m.ended(0); !slices.Equal(got, tc.marks) {
				t.Errorf("the mark ended %v, want %v", got, tc.marks)
			}
		})
	}
}

// RFC 17 §2.5: from Drain on no release is run again, and Stop ends the wait
// on one still pending without its done, leaving it to the next start, with
// no goroutine of the filesystem's left.
func TestADrainedFilesystemRunsNoReleaseAgain(t *testing.T) {
	n, m, retry, _ := pendingRelease(t)
	n.must(t, n.Drain(t.Context()))
	retry <- &vfs.Delay{Cause: vfs.DelayClone}
	if got := m.ended(1); !slices.Equal(got, []bool{false}) {
		t.Errorf("the mark ended %v after a clone's delay, want [false]", got)
	}
	time.Sleep(50 * time.Millisecond)
	if d := n.engine.deleted(); d != 1 {
		t.Errorf("%d Deletes after Drain, want the release not run again", d)
	}
}

// RFC 17 §2.5: a release run again after a clone's delay is a call the
// filesystem admits, so Drain joins it rather than returning while it waits
// for the file's admission lock and only then reaches the engine.
func TestDrainJoinsAReleaseRunAgain(t *testing.T) {
	n, m, retry, f := pendingRelease(t)
	unlock := n.Admission(f)
	retry <- &vfs.Delay{Cause: vfs.DelayClone}
	if got := m.ended(1); !slices.Equal(got, []bool{false}) {
		t.Fatalf("the mark ended %v after a clone's delay, want [false]", got)
	}
	time.Sleep(50 * time.Millisecond) // the re-run reaches the admission lock
	drained := make(chan int, 1)
	go func() {
		n.must(t, n.Drain(t.Context()))
		drained <- n.engine.deleted()
	}()
	time.Sleep(50 * time.Millisecond)
	unlock()
	atDrain := <-drained
	time.Sleep(50 * time.Millisecond)
	if d := n.engine.deleted(); d != atDrain {
		t.Errorf("%d Deletes when Drain returned, %d after, want none after", atDrain, d)
	}
}

// gateMark is a mark whose done closes entered and then waits for gate.
type gateMark struct{ entered, gate chan struct{} }

func (m *gateMark) Releasing(context.Context, openstate.FileKey) (func(bool), bool, error) {
	return func(bool) { close(m.entered); <-m.gate }, false, nil
}

// RFC 17 §2.5: Stop joins a wait on a release's outcome that is still
// running, so no waiter outlives it.
func TestStopJoinsTheWaitOnAPendingRelease(t *testing.T) {
	n := newNames(t)
	retry := make(chan error, 1)
	var first sync.Once
	n.engine.pending = func() <-chan error {
		var r <-chan error
		first.Do(func() { r = retry })
		return r
	}
	m := &gateMark{entered: make(chan struct{}), gate: make(chan struct{})}
	n.marks = m
	f := n.create(t, n.admin, n.fs.Root, "f", vfs.CreateRegular).File
	n.stored(t, f.ID)
	if _, err := n.Unlink(t.Context(), n.admin, vfs.Target{H: n.fs.Root}, []byte("f")); err != nil {
		t.Fatal(err)
	}
	retry <- nil
	<-m.entered
	n.must(t, n.Drain(t.Context()))
	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop() }()
	select {
	case err := <-stopped:
		close(m.gate)
		t.Fatalf("Stop returned (%v) while the wait on a release's outcome still ran", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(m.gate)
	n.must(t, <-stopped)
}

func TestStopEndsTheWaitOnAPendingRelease(t *testing.T) {
	n, m, _, _ := pendingRelease(t)
	n.must(t, n.Drain(t.Context()))
	stopped := make(chan error, 1)
	go func() { stopped <- n.Stop() }()
	select {
	case err := <-stopped:
		n.must(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Stop still waits on a pending release after 5s")
	}
	if !settles(0) {
		t.Errorf("%d of the filesystem's goroutines run after Stop", filesystemGoroutines())
	}
	if got := m.ended(0); len(got) != 0 {
		t.Errorf("the mark ended %v at Stop, want no done", got)
	}
}

// RFC 17 §5.3: the release after a name's removal runs within the call's
// deadline. An unlink whose release stalls returns by its deadline, the
// unlink done and the release left pending, its mark kept.
func TestARemovedNamesReleaseEndsAtItsCallsDeadline(t *testing.T) {
	n := newNames(t)
	m := &failMark{}
	n.marks = m
	f := n.create(t, n.admin, n.fs.Root, "f", vfs.CreateRegular).File
	n.stored(t, f.ID)
	free := make(chan struct{})
	n.engine.mu.Lock()
	n.engine.stall = free
	n.engine.mu.Unlock()
	call, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := n.Unlink(call, n.admin, vfs.Target{H: n.fs.Root}, []byte("f"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("an unlink whose release stalled: %v, want success", err)
		}
	case <-time.After(5 * time.Second):
		close(free)
		t.Fatalf("an unlink still ran 5s past its deadline behind a stalled release (it then answered %v)", <-done)
	}
	if got := m.ended(0); len(got) != 0 {
		t.Errorf("the mark ended %v, want kept for the next start", got)
	}
}
