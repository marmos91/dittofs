package vfs

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kv"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore/kvstoretest"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// quotaSink records the quota events it receives.
type quotaSink struct {
	mu  sync.Mutex
	got []vfs.QuotaEvent
}

func (q *quotaSink) Quota(_ context.Context, e vfs.QuotaEvent) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.got = append(q.got, e)
}
func (q *quotaSink) Access(context.Context, vfs.AccessEvent) {}

func (q *quotaSink) take() []vfs.QuotaEvent {
	q.mu.Lock()
	defer q.mu.Unlock()
	got := q.got
	q.got = nil
	return got
}

// newQuotas is newAttrs with its quota events recorded.
func newQuotas(t *testing.T) (*attrs, *quotaSink) {
	t.Helper()
	sink := &quotaSink{}
	s := newShares(t, sink, nil)
	s.must(t, s.tab.Recovery().ServeShard(t.Context(), s.share, 0, 1))
	return &attrs{
		shares: s,
		alice:  s.user(t, "alice", "", s.share, entity.AccessReadWrite),
		bob:    s.user(t, "bob", "", s.share, entity.AccessReadWrite),
	}, sink
}

// limit sets a hard limit on target in the share; a zero one deletes it.
func (s *attrs) limit(t *testing.T, target kvstore.QuotaTarget, l entity.Limit) {
	t.Helper()
	s.must(t, s.cp.SetQuota(t.Context(), s.share, entity.Quota{Principal: target.Principal, Project: target.Project, Hard: l}))
}

func quotaScope(err error) vfs.QuotaScope {
	var qe *vfs.QuotaError
	if errors.As(err, &qe) {
		return qe.Scope
	}
	return 0
}

// V10, V3: what a write grows a file by stays reserved, with the bytes
// staged, until the engine calls its Done, so writers on one primary never
// pass a hard limit while their commits are pending; a write grows a file
// only by what its staged writes have not.
func TestAWriteHoldsItsReservationUntilDone(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	alice := proven(s.alice.Principal, inLab)
	s.limit(t, kvstore.QuotaTarget{Principal: s.alice.Principal}, entity.Limit{Bytes: 10})
	s.engine.hold = true
	_, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("0123456789"), vfs.Unstable)
	s.must(t, err)
	if _, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("abcdefghij"), vfs.Unstable); err != nil {
		t.Errorf("an overwrite of staged bytes, growing nothing: %v", err)
	}
	if _, err := s.Write(ctx, alice, vfs.Anonymous(h), 10, []byte("x"), vfs.Unstable); quotaScope(err) != vfs.QuotaUser {
		t.Fatalf("a byte past 10 reserved under a limit of 10: %v, want ErrQuota (QuotaUser)", err)
	}
	for _, done := range s.engine.dones {
		done()
	}
	if _, err := s.Write(ctx, alice, vfs.Anonymous(h), 10, []byte("x"), vfs.Unstable); err != nil {
		t.Errorf("the byte once the reservations were released by Done: %v", err)
	}
}

// V3: a write refused at step 3, by open state, or at step 5, by an
// immutable file, holds no reservation: the owner's whole quota is still
// there for another file.
func TestARefusedWriteHoldsNoReservation(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	leased, frozen, other := s.file(t, "leased", s.alice.Principal, 0o666), s.file(t, "frozen", s.alice.Principal, 0o666), s.file(t, "other", s.alice.Principal, 0o666)
	// The immutable file is written through an open made before the flag
	// was set, so open state admits it and the flag refuses it at step 5.
	pc := s.smb(t, "pc", s.alice.Principal)
	ref := s.opener(t, pc, "frozen", entity.ReadData|entity.WriteData)
	_, err := s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, frozen, entity.Attrs{}.WithFlags(entity.FlagImmutable))
	s.must(t, err)
	who := openstate.Caller{Client: pc.Client, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	_, err = s.tab.Opens().Open(ctx, who, keyOf(s.record(t, leased)), openstate.OpenRequest{Access: entity.ReadData | entity.WriteData,
		Want: openstate.GrantRead | openstate.GrantWrite, Lease: openstate.LeaseKey{1}})
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{Principal: s.alice.Principal}, entity.Limit{Bytes: 5})
	s.engine.hold = true
	bob, alice := proven(s.bob.Principal, inLab), proven(s.alice.Principal, inLab)
	if _, err := s.Write(ctx, bob, vfs.Anonymous(leased), 0, []byte("12345"), vfs.Unstable); !errors.Is(err, vfs.ErrDelay) {
		t.Fatalf("a write under another client's lease: %v, want ErrDelay", err)
	}
	if _, err := s.Write(ctx, pc, ref, 0, []byte("12345"), vfs.Unstable); !errors.Is(err, vfs.ErrPerm) {
		t.Fatalf("a write to an immutable file: %v, want ErrPerm", err)
	}
	if _, err := s.Write(ctx, alice, vfs.Anonymous(other), 0, []byte("12345"), vfs.Unstable); err != nil {
		t.Errorf("the owner's whole quota after two refused writes: %v", err)
	}
}

// V10, V20: a write past a share-wide, project, user and group quota in
// turn is refused at once with a *QuotaError naming each, the first in that
// order while two or more are exceeded, staging nothing and never ErrDelay
// or ErrNoSpace; each refusal is emitted as a hard refusal naming its quota
// and the writer.
func TestAWriteIsRefusedByEachScopeInTurn(t *testing.T) {
	s, sink := newQuotas(t)
	ctx := t.Context()
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	s.must(t, s.cp.PutGroup(ctx, staff))
	tree, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "tree", kvstore.Dir(0o777))
	project, err := s.fs.Store.Files().SetProject(ctx, s.fs.Admin, tree)
	s.must(t, err)
	h, _ := s.fs.Create(t, s.fs.Admin, tree, "f", kvstore.Regular(0o666))
	_, err = s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithOwner(s.alice.Principal).WithGroup(staff.Principal))
	s.must(t, err)
	scopes := []struct {
		scope  vfs.QuotaScope
		target kvstore.QuotaTarget
	}{
		{vfs.QuotaShare, kvstore.QuotaTarget{}},
		{vfs.QuotaProject, kvstore.QuotaTarget{Project: project}},
		{vfs.QuotaUser, kvstore.QuotaTarget{Principal: s.alice.Principal}},
		{vfs.QuotaGroup, kvstore.QuotaTarget{Principal: staff.Principal}},
	}
	for _, sc := range scopes {
		s.limit(t, sc.target, entity.Limit{Bytes: 4})
	}
	bob := proven(s.bob.Principal, inLab)
	s.engine.writing = func(entity.FileID) { t.Error("a write past a quota was staged") }
	for _, sc := range scopes {
		_, err := s.Write(ctx, bob, vfs.Anonymous(h), 0, []byte("12345"), vfs.Unstable)
		if quotaScope(err) != sc.scope || errors.Is(err, vfs.ErrDelay) || errors.Is(err, vfs.ErrNoSpace) {
			t.Errorf("a write past the quotas from %v on: %v, want ErrQuota naming it", sc.scope, err)
		}
		got := sink.take()
		if len(got) != 1 || got[0].Kind != vfs.QuotaHardRefused || got[0].Scope != sc.scope || got[0].Share != s.share ||
			got[0].Subject != sc.target.Principal || got[0].Project != sc.target.Project || got[0].Hard != 4 ||
			got[0].Writer.Evidence.Principal != s.bob.Principal {
			t.Errorf("refused by %v: emitted %+v", sc.scope, got)
		}
		s.limit(t, sc.target, entity.Limit{})
	}
	s.engine.writing = nil
	if _, err := s.Write(ctx, bob, vfs.Anonymous(h), 0, []byte("12345"), vfs.Unstable); err != nil {
		t.Errorf("the write with every quota deleted: %v", err)
	}
}

// V10: StatFS inside a project whose quota is tighter than the share's
// reports the project's, for bytes and for files each, and the share's once
// the project's quota is deleted; outside the project, the share's.
func TestStatFSInsideAProjectReportsTheTighter(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	tree, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "tree", kvstore.Dir(0o777))
	project, err := s.fs.Store.Files().SetProject(ctx, s.fs.Admin, tree)
	s.must(t, err)
	s.fs.Create(t, s.fs.Admin, tree, "f", kvstore.Regular(0o644))
	s.limit(t, kvstore.QuotaTarget{}, entity.Limit{Bytes: 1 << 30, Files: 3})
	s.limit(t, kvstore.QuotaTarget{Project: project}, entity.Limit{Bytes: 100, Files: 1000})
	admin := proven(s.fs.Admin.Principal, inLab)
	info, err := s.StatFS(ctx, admin, tree)
	if err != nil || info.Quota.Hard.Bytes != 100 || info.Quota.Hard.Files != 3 || info.Usage.Files != 3 {
		t.Errorf("StatFS inside the project: %+v, %v; want its 100 bytes and the share's 3 files of 3", info, err)
	}
	s.limit(t, kvstore.QuotaTarget{Project: project}, entity.Limit{Files: 1})
	if info, err := s.StatFS(ctx, admin, tree); err != nil || info.Quota.Hard.Bytes != 1<<30 || info.Quota.Hard.Files != 1 || info.Usage.Files != 2 {
		t.Errorf("StatFS inside a project limiting files alone: %+v, %v; want the share's bytes and its 2 files of 1", info, err)
	}
	if info, err := s.StatFS(ctx, admin, s.fs.Root); err != nil || info.Quota.Hard.Files != 3 {
		t.Errorf("StatFS outside the project: %+v, %v; want the share's", info, err)
	}
	s.limit(t, kvstore.QuotaTarget{Project: project}, entity.Limit{})
	if info, err := s.StatFS(ctx, admin, tree); err != nil || info.Quota.Hard.Bytes != 1<<30 || info.Quota.Hard.Files != 3 {
		t.Errorf("StatFS inside the project once its quota is deleted: %+v, %v; want the share's", info, err)
	}
}

// V37: bob, at his inode quota, creating in alice's directory, which has
// room under alice's, is refused ErrQuota (QuotaUser): nothing is created
// and nothing charged to alice. A create in a setgid directory is charged to
// the directory's group, the new file's, never the creator's.
func TestACreateChargesTheNewFilesOwner(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	dir, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "hers", kvstore.Dir(0o777))
	files := s.fs.Store.Files()
	_, err := files.SetAttrs(ctx, s.fs.Admin, dir, entity.Attrs{}.WithOwner(s.alice.Principal))
	s.must(t, err)
	bob := proven(s.bob.Principal, inLab)
	regular := vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)}
	_, err = s.Create(ctx, bob, dir, []byte("first"), regular)
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{Principal: s.bob.Principal}, entity.Limit{Files: 1})
	s.limit(t, kvstore.QuotaTarget{Principal: s.alice.Principal}, entity.Limit{Files: 100})
	usage := func(p entity.Principal) entity.Usage {
		u, err := s.fs.Store.Capacity().PrincipalUsage(ctx, s.share, p)
		s.must(t, err)
		return u
	}
	hers := usage(s.alice.Principal)
	if _, err := s.Create(ctx, bob, dir, []byte("second"), regular); quotaScope(err) != vfs.QuotaUser {
		t.Errorf("bob's create at his inode quota in alice's directory: %v, want ErrQuota (QuotaUser)", err)
	}
	if _, _, err := s.Lookup(ctx, bob, dir, []byte("second")); !errors.Is(err, vfs.ErrNotFound) {
		t.Errorf("the refused create left its name: %v", err)
	}
	if got := usage(s.alice.Principal); got != hers {
		t.Errorf("alice's usage moved from %+v to %+v", hers, got)
	}

	// The group: staff's quota is spent by the directory itself, so a
	// create there by alice, who is not in staff, is refused once the
	// directory is setgid, and admitted before.
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	s.must(t, s.cp.PutGroup(ctx, staff))
	shared, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "shared", kvstore.Dir(0o777))
	_, err = files.SetAttrs(ctx, s.fs.Admin, shared, entity.Attrs{}.WithGroup(staff.Principal))
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{Principal: staff.Principal}, entity.Limit{Files: 1})
	alice := proven(s.alice.Principal, inLab)
	if _, err := s.Create(ctx, alice, shared, []byte("plain"), regular); err != nil {
		t.Errorf("a create charged to alice's own group: %v", err)
	}
	_, err = files.SetAttrs(ctx, s.fs.Admin, shared, entity.Attrs{}.WithMode(0o2777))
	s.must(t, err)
	if _, err := s.Create(ctx, alice, shared, []byte("setgid"), regular); quotaScope(err) != vfs.QuotaGroup {
		t.Errorf("a create in a setgid directory of a full group: %v, want ErrQuota (QuotaGroup)", err)
	}
}

// V3: a write reserves what its commit will charge, the bytes it lands in
// holes as well as those past the size: filling a hole a truncate made, or
// rewriting a range deallocated, is refused past a hard limit, and a write
// leaving a gap past the size reserves only the bytes it writes.
func TestAWriteReservesTheHolesItFills(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	alice := proven(s.alice.Principal, inLab)
	data := bytes.Repeat([]byte{'x'}, 1000)
	ut := kvstore.QuotaTarget{Principal: s.alice.Principal}

	h := s.file(t, "truncated", s.alice.Principal, 0o666)
	_, err := s.SetAttr(ctx, alice, vfs.Target{H: h}, size(1000))
	s.must(t, err)
	s.limit(t, ut, entity.Limit{Bytes: 10})
	if _, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, data, vfs.Unstable); quotaScope(err) != vfs.QuotaUser {
		t.Errorf("1000 bytes into a hole a truncate made, under a limit of 10: %v, want ErrQuota (QuotaUser)", err)
	}

	s.limit(t, ut, entity.Limit{})
	d := s.file(t, "deallocated", s.alice.Principal, 0o666)
	_, err = s.Write(ctx, alice, vfs.Anonymous(d), 0, data, vfs.Unstable)
	s.must(t, err)
	_, err = s.Deallocate(ctx, alice, vfs.Anonymous(d), 0, 1000)
	s.must(t, err)
	s.limit(t, ut, entity.Limit{Bytes: 10})
	if _, err := s.Write(ctx, alice, vfs.Anonymous(d), 0, data, vfs.Unstable); quotaScope(err) != vfs.QuotaUser {
		t.Errorf("1000 bytes rewritten over a deallocated range, under a limit of 10: %v, want ErrQuota (QuotaUser)", err)
	}
	if _, err := s.Write(ctx, alice, vfs.Anonymous(d), 0, data[:10], vfs.Unstable); err != nil {
		t.Errorf("10 bytes into the hole, under a limit of 10: %v", err)
	}

	s.limit(t, ut, entity.Limit{Bytes: 1})
	g := s.file(t, "gap", s.alice.Principal, 0o666)
	if _, err := s.Write(ctx, alice, vfs.Anonymous(g), 1000, data[:1], vfs.Unstable); err != nil {
		t.Errorf("1 byte at 1000, under a limit of 1: %v", err)
	}
}

// V37: a create that adds no file reserves nothing: at her inode quota, an
// exclusive create's retry with its verifier answers the file it made, and a
// guarded create of the same name ErrExist, never ErrQuota. A create's
// reservation is released once the store has answered, so a quota is spent
// only by the files made.
func TestACreateAddingNoFileReservesNothing(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	dir, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "d", kvstore.Dir(0o777))
	alice := proven(s.alice.Principal, inLab)
	used := func() int64 {
		u, err := s.fs.Store.Capacity().PrincipalUsage(ctx, s.share, s.alice.Principal)
		s.must(t, err)
		return u.Files
	}
	regular := vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)}
	s.limit(t, kvstore.QuotaTarget{Principal: s.alice.Principal}, entity.Limit{Files: used() + 2})
	for _, name := range []string{"a", "b"} {
		if _, err := s.Create(ctx, alice, dir, []byte(name), regular); err != nil {
			t.Fatalf("create %s, the quota's room for two: %v", name, err)
		}
	}
	excl := regular
	excl.Verifier = &[8]byte{7}
	if _, err := s.Create(ctx, alice, dir, []byte("x"), excl); quotaScope(err) != vfs.QuotaUser {
		t.Fatalf("an exclusive create past the inode quota: %v, want ErrQuota", err)
	}
	s.limit(t, kvstore.QuotaTarget{Principal: s.alice.Principal}, entity.Limit{Files: used() + 1})
	made, err := s.Create(ctx, alice, dir, []byte("x"), excl)
	s.must(t, err)
	if again, err := s.Create(ctx, alice, dir, []byte("x"), excl); err != nil || again.File.ID != made.File.ID {
		t.Errorf("the exclusive create's retry at the inode quota: %v, %v; want the file it made", again.File.ID, err)
	}
	if _, err := s.Create(ctx, alice, dir, []byte("x"), regular); !errors.Is(err, vfs.ErrExist) {
		t.Errorf("a guarded create of an existing name at the inode quota: %v, want ErrExist", err)
	}
}

// V37: a create's inode is charged to the new file's project, to the owner
// it names rather than its creator, and, with no group named or inherited
// and none on the credential, to the creator's primary group.
func TestACreateChargesItsProjectNamedOwnerAndPrimaryGroup(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	files := s.fs.Store.Files()
	regular := vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)}
	alice, bob, admin := proven(s.alice.Principal, inLab), proven(s.bob.Principal, inLab), proven(s.fs.Admin.Principal, inLab)

	// The project's quota is spent by its root alone.
	tree, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "tree", kvstore.Dir(0o777))
	project, err := files.SetProject(ctx, s.fs.Admin, tree)
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{Project: project}, entity.Limit{Files: 1})
	if _, err := s.Create(ctx, alice, tree, []byte("f"), regular); quotaScope(err) != vfs.QuotaProject {
		t.Errorf("a create in a project at its inode quota: %v, want ErrQuota (QuotaProject)", err)
	}

	// bob's quota is spent by his first file; the admin creating one for him
	// is charged to him.
	open, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "open", kvstore.Dir(0o777))
	_, err = s.Create(ctx, bob, open, []byte("bobs"), regular)
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{Principal: s.bob.Principal}, entity.Limit{Files: 1})
	forBob := regular
	forBob.Attrs = forBob.Attrs.WithOwner(s.bob.Principal)
	if _, err := s.Create(ctx, admin, open, []byte("given"), forBob); quotaScope(err) != vfs.QuotaUser {
		t.Errorf("a create naming bob its owner at his inode quota: %v, want ErrQuota (QuotaUser)", err)
	}

	// staff's quota is spent by a directory it owns; carol, whose primary
	// group is staff, is charged to it.
	staff := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	s.must(t, s.cp.PutGroup(ctx, staff))
	carol := entity.User{Principal: entity.NewUser(), Name: "carol", Primary: staff.Principal,
		IDs: entity.ProtocolIDs{{Scheme: entity.SchemeUID, Value: "61999"}}}
	s.must(t, s.cp.PutUser(ctx, carol))
	s.must(t, s.cp.SetGrant(ctx, entity.ShareGrant{Share: s.share, Principal: carol.Principal, Access: entity.AccessReadWrite}))
	theirs, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "theirs", kvstore.Dir(0o777))
	_, err = files.SetAttrs(ctx, s.fs.Admin, theirs, entity.Attrs{}.WithGroup(staff.Principal))
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{Principal: staff.Principal}, entity.Limit{Files: 1})
	if _, err := s.Create(ctx, proven(carol.Principal, inLab), open, []byte("carols"), regular); quotaScope(err) != vfs.QuotaGroup {
		t.Errorf("a create by a member whose primary group is at its inode quota: %v, want ErrQuota (QuotaGroup)", err)
	}
}

// fullGroup is newQuotas with staff, a group whose one-inode hard limit a
// directory it owns spends, and f, alice's file in her own group.
func fullGroup(t *testing.T) (s *attrs, sink *quotaSink, staff entity.Principal, f vfs.Handle) {
	t.Helper()
	s, sink = newQuotas(t)
	ctx := t.Context()
	g := entity.Group{Principal: entity.NewGroup(), Name: "staff"}
	s.must(t, s.cp.PutGroup(ctx, g))
	theirs, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "theirs", kvstore.Dir(0o777))
	_, err := s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, theirs, entity.Attrs{}.WithGroup(g.Principal))
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{Principal: g.Principal}, entity.Limit{Files: 1})
	return s, sink, g.Principal, s.file(t, "f", s.alice.Principal, 0o644)
}

func (s *attrs) usage(t *testing.T, p entity.Principal) entity.Usage {
	t.Helper()
	u, err := s.fs.Store.Capacity().PrincipalUsage(t.Context(), s.share, p)
	s.must(t, err)
	return u
}

// V37: a chgrp of a file into a group at its hard limit is refused ErrQuota
// (QuotaGroup), even for the admin, emitted as a hard refusal of the group,
// and the file keeps its group.
func TestAChgrpIntoAFullGroupIsRefused(t *testing.T) {
	s, sink, staff, h := fullGroup(t)
	group := s.record(t, h).Group
	chgrp := vfs.AttrChange{Attrs: entity.Attrs{}.WithGroup(staff)}
	if _, err := s.SetAttr(t.Context(), proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, chgrp); quotaScope(err) != vfs.QuotaGroup {
		t.Errorf("a chgrp into a group at its hard limit: %v, want ErrQuota (QuotaGroup)", err)
	}
	if got := s.record(t, h).Group; got != group {
		t.Errorf("the refused chgrp left the group %v, want %v", got, group)
	}
	if got := sink.take(); len(got) != 1 || got[0].Kind != vfs.QuotaHardRefused || got[0].Scope != vfs.QuotaGroup || got[0].Subject != staff {
		t.Errorf("the refused chgrp emitted %+v, want one hard refusal of staff", got)
	}
}

// V37: a chown and chgrp together, the new owner with room and the new
// group at its hard limit, is refused, and changes neither.
func TestAChownAndChgrpWithOneOverIsRefused(t *testing.T) {
	s, _, staff, h := fullGroup(t)
	group := s.record(t, h).Group
	both := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(s.bob.Principal).WithGroup(staff)}
	if _, err := s.SetAttr(t.Context(), proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, both); quotaScope(err) != vfs.QuotaGroup {
		t.Errorf("a chown and chgrp, the group alone over: %v, want ErrQuota (QuotaGroup)", err)
	}
	if got := s.record(t, h); got.Owner != s.alice.Principal || got.Group != group {
		t.Errorf("the refused change left owner %v group %v", got.Owner, got.Group)
	}
}

// V37: a chown to an owner with room for the file raises the owner's usage
// by it, and is not judged against the share's quota, at its limit, since
// the charge stays in the share.
func TestAChownToAnOwnerWithRoomMovesTheCharge(t *testing.T) {
	s, _, _, h := fullGroup(t)
	ctx := t.Context()
	bobs := s.usage(t, s.bob.Principal)
	s.limit(t, kvstore.QuotaTarget{Principal: s.bob.Principal}, entity.Limit{Files: bobs.Files + 1})
	share, err := s.fs.Store.Capacity().Usage(ctx, s.share)
	s.must(t, err)
	s.limit(t, kvstore.QuotaTarget{}, entity.Limit{Files: share.Files})
	chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(s.bob.Principal)}
	if _, err := s.SetAttr(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, chown); err != nil {
		t.Fatalf("a chown to bob, with room for the file, the share at its limit: %v", err)
	}
	if got := s.usage(t, s.bob.Principal); got.Files != bobs.Files+1 {
		t.Errorf("bob's usage after the chown: %+v, want %d files", got, bobs.Files+1)
	}
}

// V37: a chown and chgrp naming the owner and group the file has reserves
// nothing, so its owner, at his limit, is not refused.
func TestAChownChangingNothingReservesNothing(t *testing.T) {
	s, _, _, h := fullGroup(t)
	f := s.record(t, h)
	s.limit(t, kvstore.QuotaTarget{Principal: s.alice.Principal}, entity.Limit{Files: s.usage(t, s.alice.Principal).Files})
	same := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(f.Owner).WithGroup(f.Group)}
	if _, err := s.SetAttr(t.Context(), proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, same); err != nil {
		t.Errorf("a chown and chgrp to what the file has, its owner at her limit: %v", err)
	}
}

// V37: a chown first commits the file's pending existence, so bytes staged
// unstable and not yet charged count against the new owner: one near his
// byte limit is refused, though the file's committed charge alone fits.
func TestAChownCommitsStagedBytesBeforeItReserves(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	data := []byte("0123456789")
	if _, err := s.Write(ctx, proven(s.alice.Principal, inLab), vfs.Anonymous(h), 0, data, vfs.Unstable); err != nil {
		t.Fatal(err)
	}
	f := s.record(t, h)
	s.engine.existence = func(id entity.FileID) error {
		_, err := s.fs.Store.Existence().Commit(ctx, []kvstore.PendingExistence{{Share: f.Share, File: id, Ops: []kvstore.PendingOp{
			{Version: entity.VersionOf(f.Applied.Epoch() + 1), Kind: kvstore.OpWrite, Extent: entity.Extent{Length: int64(len(data))}, Mtime: f.Modify, Ctime: f.Modify},
		}}}, nil)
		return err
	}
	bobs := s.usage(t, s.bob.Principal)
	s.limit(t, kvstore.QuotaTarget{Principal: s.bob.Principal}, entity.Limit{Bytes: bobs.Bytes + int64(len(data)) - 1})
	chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(s.bob.Principal)}
	if _, err := s.SetAttr(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, chown); quotaScope(err) != vfs.QuotaUser {
		t.Errorf("a chown of %d staged bytes to bob, %d under his limit: %v, want ErrQuota (QuotaUser)", len(data), len(data)-1, err)
	}
	if got := s.record(t, h); got.Owner != s.alice.Principal || got.Charged != int64(len(data)) {
		t.Errorf("after the refused chown: owner %v, charged %d; want alice's, its staged bytes committed", got.Owner, got.Charged)
	}
}

// V37: a chown's existence commit holds no lock, so a write may be staged
// while it runs; that write's reservation, outstanding until its commit,
// moves to the new owner, so a chown to an owner with room for the
// committed charge alone, and not for the write, is refused.
func TestAWriteStagedAfterAChownsCommitCountsAgainstTheNewOwner(t *testing.T) {
	s, _ := newQuotas(t)
	s.engine.hold = true
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	bobs := s.usage(t, s.bob.Principal)
	s.limit(t, kvstore.QuotaTarget{Principal: s.bob.Principal}, entity.Limit{Bytes: bobs.Bytes + 9})
	data := []byte("0123456789")
	wrote := make(chan error, 1)
	s.engine.existence = func(entity.FileID) error {
		go func() {
			_, err := s.Write(ctx, proven(s.alice.Principal, inLab), vfs.Anonymous(h), 0, data, vfs.Unstable)
			wrote <- err
		}()
		select {
		case err := <-wrote:
			wrote <- err
		case <-time.After(time.Second):
		}
		return nil
	}
	chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(s.bob.Principal)}
	if _, err := s.SetAttr(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, chown); quotaScope(err) != vfs.QuotaUser {
		t.Errorf("a chown to bob, 9 bytes under his limit, of a file with %d bytes staged during its commit: %v, want ErrQuota (QuotaUser)", len(data), err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the write staged during the chown's commit: %v", err)
	}
	if got := s.record(t, h).Owner; got != s.alice.Principal {
		t.Errorf("after the refused chown: owner %v, want alice", got)
	}
}

// V37: a write filling a hole adds bytes no size shows, so only its own
// reservation knows them; staged while a chown's existence commit runs, it
// is counted against the new owner, so a chown to an owner with room for
// the file's committed charge, nothing, and not for the hole filled, is
// refused.
func TestAHoleFillStagedBeforeAChownCountsAgainstTheNewOwner(t *testing.T) {
	s, _ := newQuotas(t)
	s.engine.hold = true
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	const hole = 10
	if _, err := s.SetAttr(ctx, proven(s.alice.Principal, inLab), vfs.Target{H: h}, size(hole)); err != nil {
		t.Fatal(err)
	}
	bobs := s.usage(t, s.bob.Principal)
	s.limit(t, kvstore.QuotaTarget{Principal: s.bob.Principal}, entity.Limit{Bytes: bobs.Bytes + hole - 1})
	wrote := make(chan error, 1)
	s.engine.existence = func(entity.FileID) error {
		_, err := s.Write(ctx, proven(s.alice.Principal, inLab), vfs.Anonymous(h), 0, []byte("0123456789"), vfs.Unstable)
		wrote <- err
		return nil
	}
	chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(s.bob.Principal)}
	if _, err := s.SetAttr(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Target{H: h}, chown); quotaScope(err) != vfs.QuotaUser {
		t.Errorf("a chown to bob, %d bytes under his limit, of a file whose %d-byte hole a staged write fills: %v, want ErrQuota (QuotaUser)", hole-1, hole, err)
	}
	if err := <-wrote; err != nil {
		t.Fatalf("the hole fill staged during the chown's commit: %v", err)
	}
	if got := s.record(t, h).Owner; got != s.alice.Principal {
		t.Errorf("after the refused chown: owner %v, want alice", got)
	}
}

// V37: only a change admitted to the share, naming the owner or group,
// commits the file's pending existence: a chown refused at admission, here
// by a quiesced share, and a chmod commit nothing.
func TestOnlyAnAdmittedChownCommitsExistence(t *testing.T) {
	s, _ := newQuotas(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	commits := 0
	s.engine.existence = func(entity.FileID) error {
		commits++
		return nil
	}
	admin := proven(s.fs.Admin.Principal, inLab)
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, vfs.AttrChange{Attrs: entity.Attrs{}.WithMode(0o600)}); err != nil {
		t.Fatal(err)
	}
	if commits != 0 {
		t.Errorf("a chmod committed the file's existence %d times, want none", commits)
	}
	s.must(t, s.cp.SetShareState(ctx, s.share, entity.ShareEnabled, entity.ShareQuiesced))
	chown := vfs.AttrChange{Attrs: entity.Attrs{}.WithOwner(s.bob.Principal)}
	if _, err := s.SetAttr(ctx, admin, vfs.Target{H: h}, chown); err == nil {
		t.Fatal("a chown on a quiesced share succeeded")
	}
	if commits != 0 {
		t.Errorf("a chown refused at admission committed the file's existence %d times, want none", commits)
	}
}

// A create whose check for the name fails in the store, for a cause other
// than the name being absent, fails with it, and holds no inode reserved:
// the same create, the store recovered, fits a quota with room for one.
func TestACreateWhoseLookupFailsReservesNothing(t *testing.T) {
	db := &failKV{}
	s := startShares(t, newFixtureOver(t, Deps{}, kvstoretest.NewFS(t, func(tb testing.TB) kv.KV { db.KV = memKV(tb); return db })), nil)
	s.must(t, s.tab.Recovery().ServeShard(t.Context(), s.share, 0, 1))
	ctx := t.Context()
	alice := s.user(t, "alice", "", s.share, entity.AccessReadWrite)
	a := &attrs{shares: s, alice: alice}
	dir, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "hers", kvstore.Dir(0o777))
	a.limit(t, kvstore.QuotaTarget{Principal: alice.Principal}, entity.Limit{Files: a.usage(t, alice.Principal).Files + 1})
	boom := errors.New("disk gone")
	db.failing(func(update bool) error {
		if !update && onStack("kvstore.Namespace.Lookup") {
			return boom
		}
		return nil
	})
	call := proven(alice.Principal, inLab)
	regular := vfs.CreateArgs{Kind: vfs.CreateRegular, Attrs: entity.Attrs{}.WithMode(0o644)}
	if _, err := s.Create(ctx, call, dir, []byte("f"), regular); !errors.Is(err, vfs.ErrUnavailable) {
		t.Fatalf("a create whose name check fails in the store: %v, want ErrUnavailable", err)
	}
	db.failing(nil)
	if _, err := s.Create(ctx, call, dir, []byte("f"), regular); err != nil {
		t.Errorf("the create once the store recovered, with room for one inode: %v", err)
	}
}

// onStack reports whether a function whose name ends in fn is on the stack.
func onStack(fn string) bool {
	pc := make([]uintptr, 64)
	frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, fn) {
			return true
		}
		if !more {
			return false
		}
	}
}
