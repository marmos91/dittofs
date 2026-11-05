package vfs

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cubbit/dittofs/internal/vfs/lockorder"
	"github.com/cubbit/dittofs/pkg/metadata/entity"
	"github.com/cubbit/dittofs/pkg/metadata/kvstore"
	"github.com/cubbit/dittofs/pkg/openstate"
	"github.com/cubbit/dittofs/pkg/vfs"
)

// opener opens name in the share's root as c, asking access, and answers
// the open's reference.
func (s *attrs) opener(t *testing.T, c vfs.Call, name string, access vfs.Access) vfs.OpenRef {
	t.Helper()
	r, err := s.Open(t.Context(), c, byName(s.fs.Root, name, vfs.OpenExisting, access))
	s.must(t, err)
	return r.Ref
}

// smb is a call from a new SMB client named name, signed in as p.
func (s *attrs) smb(t *testing.T, name string, p entity.Principal) vfs.Call {
	t.Helper()
	return (&names{shares: s.shares}).smbClient(t, name, p)
}

// A write is read back, the read answering whether it reached the end, and
// a caller with no read right on the file is refused an anonymous read.
func TestAWriteIsReadBack(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o600)
	alice := proven(s.alice.Principal, inLab)
	w, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("hello"), vfs.Unstable)
	if err != nil || w.N != 5 || w.WCC.After.Size != 5 {
		t.Fatalf("Write: %+v, %v", w, err)
	}
	buf := make([]byte, 3)
	if n, eof, err := s.Read(ctx, alice, vfs.Anonymous(h), 0, buf); err != nil || n != 3 || eof || string(buf) != "hel" {
		t.Errorf("Read of the start: %d %v %q, %v", n, eof, buf[:n], err)
	}
	if n, eof, err := s.Read(ctx, alice, vfs.Anonymous(h), 3, buf); err != nil || n != 2 || !eof || string(buf[:n]) != "lo" {
		t.Errorf("Read to the end: %d %v %q, %v", n, eof, buf[:n], err)
	}
	if _, _, err := s.Read(ctx, proven(s.bob.Principal, inLab), vfs.Anonymous(h), 0, buf); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("bob's anonymous read of alice's 0600 file: %v, want ErrAccess", err)
	}
}

// V3: a write open state refuses, here while it recalls another client's
// lease, stages no byte and clears no setuid bit.
func TestAWriteRefusedByOpenStateChangesNothing(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o6777)
	pc := s.smb(t, "pc", s.alice.Principal)
	who := openstate.Caller{Client: pc.Client, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	r, err := s.tab.Opens().Open(ctx, who, keyOf(s.record(t, h)), openstate.OpenRequest{Access: entity.ReadData | entity.WriteData,
		Want: openstate.GrantRead | openstate.GrantWrite, Lease: openstate.LeaseKey{1}})
	s.must(t, err)
	if r.Grant == nil || r.Grant.Kind&openstate.GrantWrite == 0 {
		t.Fatalf("open state offered no write lease to recall: %+v", r.Grant)
	}
	s.engine.writing = func(entity.FileID) { t.Error("a refused write was staged") }
	_, err = s.Write(ctx, proven(s.bob.Principal, inLab), vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable)
	var delay *vfs.Delay
	if !errors.As(err, &delay) || delay.Cause != vfs.DelayRecall {
		t.Errorf("a write under another client's lease: %v, want ErrDelay DelayRecall", err)
	}
	if m := s.record(t, h).Mode; m != 0o6777 {
		t.Errorf("the refused write left mode %o", m)
	}
}

// V7: a write holds its file's admission lock from open state's check to
// the bytes staged, so a mandatory lock, taken under that lock as every lock
// is, is ordered wholly before the write, which it then refuses, or wholly
// after it.
func TestALockIsOrderedWhollyBeforeOrAfterAWrite(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	id := fileOf(h)
	pc := s.smb(t, "pc", s.alice.Principal)
	ref := s.opener(t, pc, "f", entity.ReadData|entity.WriteData)
	key := keyOf(s.record(t, h))
	who := openstate.Caller{Client: pc.Client, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	lock := func(name string) error {
		defer s.Admission(id)()
		_, err := s.tab.Locks().Lock(ctx, who, key, openstate.LockReq{
			Owner: openstate.LockOwner{Client: pc.Client, Name: []byte(name), Stack: true}, Open: ref,
			Ranges: []openstate.LockRange{{ByteRange: openstate.ByteRange{Offset: 0, Length: 10}, Exclusive: true}},
		})
		return err
	}
	bob := proven(s.bob.Principal, inLab)

	// After: as the write stages its bytes it still holds the admission
	// lock every lock is taken under, which a take that may not wait finds
	// held; the lock is granted once the write has answered.
	gone, cancel := context.WithCancel(ctx)
	cancel()
	s.engine.writing = func(entity.FileID) {
		if unlock, err := s.locks.Take(gone, lockorder.Set{Admit: []lockorder.Key{lockorder.Key(id)}}); err == nil {
			unlock()
			t.Error("the admission lock was free between the write's check and its bytes staged")
		}
	}
	if _, err := s.Write(ctx, bob, vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable); err != nil {
		t.Fatalf("the write before the lock: %v", err)
	}
	s.engine.writing = nil
	s.must(t, lock("after"))

	// Before: the lock held, the write meets it and stages nothing.
	s.engine.writing = func(entity.FileID) { t.Error("a write under a mandatory lock was staged") }
	if _, err := s.Write(ctx, bob, vfs.Anonymous(h), 0, []byte("y"), vfs.Unstable); !errors.Is(err, vfs.ErrLocked) {
		t.Errorf("a write under another's exclusive lock: %v, want ErrLocked", err)
	}
}

// V16: a client's GetAttr after another's acknowledged write answers a
// greater Version, before anything commits, and so after every write.
func TestEveryWriteAdvancesTheVersion(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	alice, bob := proven(s.alice.Principal, inLab), proven(s.bob.Principal, inLab)
	seen, err := s.GetAttr(ctx, alice, h)
	s.must(t, err)
	for i := range 3 {
		_, err := s.Write(ctx, bob, vfs.Anonymous(h), int64(i), []byte("x"), vfs.Unstable)
		s.must(t, err)
		got, err := s.GetAttr(ctx, alice, h)
		s.must(t, err)
		if !seen.Version.Less(got.Version) {
			t.Errorf("write %d: Version %v after one acknowledged, was %v", i, got.Version, seen.Version)
		}
		seen = got
	}
}

// V18: bob presenting alice's open is NotYours, writing a file alice may
// write and he may not, then one both may, on alice's own NFSv4.0 client,
// where the opener is the whole check, and on his own client; alice's open
// still writes after.
func TestAnOpenWritesOnlyForItsOpener(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	alice := s.user(t, "alice40", "2000", s.share, entity.AccessReadWrite)
	s.user(t, "bob40", "2001", s.share, entity.AccessReadWrite)
	s.file(t, "hers", alice.Principal, 0o644)
	s.file(t, "both", alice.Principal, 0o666)
	a := s.nfs41(t, "a-host", 2000, 0) // no session: NFSv4.0
	b := s.nfs41(t, "b-host", 2001, 0)
	onHers := a
	onHers.Identity = claimed(2001, inLab).Identity
	for _, name := range []string{"hers", "both"} {
		ref := s.opener(t, a, name, entity.ReadData|entity.WriteData)
		for how, bob := range map[string]vfs.Call{"on alice's client": onHers, "on his own": b} {
			s.engine.writing = func(entity.FileID) { t.Errorf("%s: bob's write through alice's open was staged", name) }
			if _, err := s.Write(ctx, bob, ref, 0, []byte("bob"), vfs.Unstable); !notYours(err) {
				t.Errorf("%s: bob %s writing through alice's open: %v, want NotYours", name, how, err)
			}
		}
		s.engine.writing = nil
		if _, err := s.Write(ctx, a, ref, 0, []byte("alice"), vfs.Unstable); err != nil {
			t.Errorf("%s: alice's write through her open after bob's: %v", name, err)
		}
	}
}

// V19: a write through an open on a continuously available share is
// committed over its extent before it is answered, whatever it asked; an
// unstable write elsewhere, or with no open, is not, and a stable one
// always is.
func TestAWriteOnAContinuouslyAvailableShareIsStable(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	pc := s.smb(t, "pc", s.alice.Principal)
	ref := s.opener(t, pc, "f", entity.ReadData|entity.WriteData)
	write := func(ref vfs.OpenRef, off int64, st vfs.Stability) []entity.Extent {
		t.Helper()
		s.engine.commits = nil
		_, err := s.Write(ctx, pc, ref, off, []byte("abc"), st)
		s.must(t, err)
		return s.engine.commits
	}
	if got := write(ref, 0, vfs.Unstable); len(got) != 0 {
		t.Errorf("an unstable write through an open: committed %v", got)
	}
	if got := write(vfs.Anonymous(h), 0, vfs.FileSync); len(got) != 1 || got[0] != (entity.Extent{Offset: 0, Length: 3}) {
		t.Errorf("a stable write: committed %v, want its extent", got)
	}
	p, err := s.cp.ExportPolicy(ctx, s.share)
	s.must(t, err)
	p.SMB |= entity.SMBContinuouslyAvailable
	s.must(t, s.cp.PutExportPolicy(ctx, p))
	if got := write(ref, 7, vfs.Unstable); len(got) != 1 || got[0] != (entity.Extent{Offset: 7, Length: 3}) {
		t.Errorf("an unstable write through an open on a continuously available share: committed %v, want its extent", got)
	}
	if got := write(vfs.Anonymous(h), 0, vfs.Unstable); len(got) != 0 {
		t.Errorf("an unstable write with no open there: committed %v", got)
	}
}

// V20, V25: the journal's capacity refusal reaches the caller as ErrDelay
// DelayCapacity, a retry, from a write and from a commit, never ErrNoSpace.
func TestACapacityRefusalIsADelay(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	alice := proven(s.alice.Principal, inLab)
	full := &vfs.Delay{Cause: vfs.DelayCapacity}
	s.engine.writeErr, s.engine.commitErr = full, full
	_, werr := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable)
	_, cerr := s.Commit(ctx, alice, vfs.Anonymous(h), 0, 0)
	for name, err := range map[string]error{"Write": werr, "Commit": cerr} {
		var delay *vfs.Delay
		if !errors.As(err, &delay) || delay.Cause != vfs.DelayCapacity || errors.Is(err, vfs.ErrNoSpace) {
			t.Errorf("%s with the journal full: %v, want ErrDelay DelayCapacity", name, err)
		}
	}
}

// V25: Write answers the verifier sampled before staging, a stable one too,
// and Commit the one sampled after its sync; a stable write whose Commit
// answers another verifier is ErrDelay (DelayCommit), as a loss may have
// taken it; an SMB flush through each of two opens of a file across a loss
// fails ErrLost once, then succeeds, and a commit through no open learns
// nothing of it.
func TestTheVerifierAndAFlushAcrossALoss(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	alice := proven(s.alice.Principal, inLab)
	s.engine.verifier, s.engine.synced = vfs.Verifier{2}, vfs.Verifier{2}
	w, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("x"), vfs.FileSync)
	if err != nil || w.Verifier != (vfs.Verifier{2}) {
		t.Errorf("a stable write's verifier: %v, %v; want the one sampled before staging", w.Verifier, err)
	}
	s.engine.verifier = vfs.Verifier{1}
	_, err = s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("x"), vfs.FileSync)
	if delay := (*vfs.Delay)(nil); !errors.As(err, &delay) || delay.Cause != vfs.DelayCommit {
		t.Errorf("a stable write whose Commit answered another verifier: %v, want ErrDelay DelayCommit", err)
	}
	if v, err := s.Commit(ctx, alice, vfs.Anonymous(h), 0, 0); err != nil || v != (vfs.Verifier{2}) {
		t.Errorf("Commit's verifier: %v, %v; want the one sampled after its sync", v, err)
	}
	pc, laptop := s.smb(t, "pc", s.alice.Principal), s.smb(t, "laptop", s.alice.Principal)
	refs := map[string]struct {
		c   vfs.Call
		ref vfs.OpenRef
	}{
		"pc":     {pc, s.opener(t, pc, "f", entity.ReadData|entity.WriteData)},
		"laptop": {laptop, s.opener(t, laptop, "f", entity.ReadData|entity.WriteData)},
	}
	s.engine.mu.Lock()
	s.engine.loss = 1
	s.engine.mu.Unlock()
	if _, err := s.Commit(ctx, alice, vfs.Anonymous(h), 0, 0); err != nil {
		t.Errorf("a commit through no open across the loss: %v", err)
	}
	for name, o := range refs {
		if _, err := s.Commit(ctx, o.c, o.ref, 0, 0); !errors.Is(err, vfs.ErrLost) {
			t.Errorf("%s: the first flush across a loss: %v, want ErrLost", name, err)
		}
		if _, err := s.Commit(ctx, o.c, o.ref, 0, 0); err != nil {
			t.Errorf("%s: the second flush: %v", name, err)
		}
	}
}

// V22: an unprivileged write or deallocate clears setuid and setgid, the
// owner's included, in a change committed before the engine stages it, so
// GetAttr shows them clear before anything commits, and so does the change's
// WCC.Before, read after the clear; a privileged one keeps them. An append to
// a setuid, append-only file clears them too, and a write below its size,
// or a deallocate from it, is refused and clears nothing.
func TestAWriteClearsSetIDBeforeItIsStaged(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	for _, c := range []struct {
		name string
		who  entity.Principal
		dea  bool
		want uint32
	}{
		{"bob", s.bob.Principal, false, 0o0777},
		{"alice", s.alice.Principal, false, 0o0777},
		{"admin", s.fs.Admin.Principal, false, 0o6777},
		{"bob's deallocate", s.bob.Principal, true, 0o0777},
	} {
		h := s.file(t, c.name, s.alice.Principal, 0o6777)
		staged := -1
		stage := func(entity.FileID) { staged = int(s.record(t, h).Mode) }
		s.engine.writing = stage
		var err error
		var w vfs.WCC
		if c.dea {
			s.engine.writing = nil
			_, err = s.engine.Write(ctx, h.Share(), fileOf(h), 0, []byte("data"), WriteOpts{})
			s.must(t, err)
			s.engine.dealloc = stage
			w, err = s.Deallocate(ctx, proven(c.who, inLab), vfs.Anonymous(h), 0, 2)
		} else {
			var wr vfs.WriteResult
			wr, err = s.Write(ctx, proven(c.who, inLab), vfs.Anonymous(h), 0, []byte("x"), vfs.Unstable)
			w = wr.WCC
		}
		s.must(t, err)
		if w.Before == nil || w.Before.Mode != c.want {
			t.Errorf("%s: WCC.Before %+v, want mode %o", c.name, w.Before, c.want)
		}
		if staged != int(c.want) {
			t.Errorf("%s: the record's mode as the change was staged: %o, want %o", c.name, staged, c.want)
		}
		if got, err := s.GetAttr(ctx, proven(c.who, inLab), h); err != nil || got.Mode != c.want {
			t.Errorf("%s: GetAttr mode %o, %v; want %o", c.name, got.Mode, err, c.want)
		}
	}
	s.engine.writing, s.engine.dealloc = nil, nil

	h := s.file(t, "log", s.alice.Principal, 0o666)
	_, err := s.Write(ctx, proven(s.fs.Admin.Principal, inLab), vfs.Anonymous(h), 0, []byte("old"), vfs.Unstable)
	s.must(t, err)
	files := s.fs.Store.Files()
	_, err = files.SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithMode(0o4666))
	s.must(t, err)
	// An open taken before the flag is set holds write data, so the flags
	// alone refuse its write below the size; one holding append data alone
	// is refused it by its grant first.
	bob := s.smb(t, "bob-pc", s.bob.Principal)
	rw := s.opener(t, bob, "log", entity.WriteData|entity.AppendData)
	ref := s.opener(t, bob, "log", entity.AppendData)
	_, err = files.SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithFlags(entity.FlagAppendOnly))
	s.must(t, err)
	for _, c := range []struct {
		ref  vfs.OpenRef
		want error
	}{{vfs.Anonymous(h), vfs.ErrPerm}, {rw, vfs.ErrPerm}, {ref, vfs.ErrAccess}} {
		if _, err := s.Write(ctx, bob, c.ref, 0, []byte("new"), vfs.Unstable); !errors.Is(err, c.want) {
			t.Errorf("a write below an append-only file's size through %v: %v, want %v", c.ref.Kind, err, c.want)
		}
		if m := s.record(t, h).Mode; m != 0o4666 {
			t.Errorf("the refused write through %v left mode %o", c.ref.Kind, m)
		}
	}
	// A deallocate never appends, so one from the size is refused too.
	s.engine.dealloc = func(entity.FileID) { t.Error("a deallocate of an append-only file was staged") }
	if _, err := s.Deallocate(ctx, bob, rw, 3, 100); !errors.Is(err, vfs.ErrPerm) {
		t.Errorf("a deallocate from an append-only file's size: %v, want ErrPerm", err)
	}
	s.engine.dealloc = nil
	if m := s.record(t, h).Mode; m != 0o4666 {
		t.Errorf("the refused deallocate left mode %o", m)
	}
	if _, err := s.Write(ctx, bob, ref, 3, []byte("new"), vfs.Unstable); err != nil {
		t.Errorf("an append to a setuid, append-only file: %v", err)
	}
	if m := s.record(t, h).Mode; m != 0o0666 {
		t.Errorf("the append left mode %o, want 666", m)
	}
}

// V31: Write's and Deallocate's WCC are read in the one hold of the file's
// accept lock that stages the change: a write staged while the call waits
// for the lock is in its Before, and one staged after it answers is in
// neither side.
func TestADataChangesWCCIsReadInOneHold(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	for name, change := range map[string]func(vfs.Handle) (vfs.WCC, error){
		"Write": func(h vfs.Handle) (vfs.WCC, error) {
			w, err := s.Write(ctx, admin, vfs.Anonymous(h), 17, []byte("x"), vfs.Unstable)
			return w.WCC, err
		},
		"Deallocate": func(h vfs.Handle) (vfs.WCC, error) {
			return s.Deallocate(ctx, admin, vfs.Anonymous(h), 0, 4)
		},
	} {
		h := s.file(t, name, s.alice.Principal, 0o644)
		id := fileOf(h)
		unlock, err := s.locks.Take(ctx, lockorder.Set{Accept: []lockorder.Key{lockorder.Key(id)}})
		s.must(t, err)
		var w vfs.WCC
		answered := make(chan struct{})
		go func() {
			defer close(answered)
			w, err = change(h)
		}()
		time.Sleep(20 * time.Millisecond)
		_, _ = s.engine.Write(ctx, h.Share(), id, 0, []byte("staged while held"), WriteOpts{})
		unlock()
		<-answered
		s.must(t, err)
		if w.Before == nil || w.Before.Size != 17 || w.After.Size < 17 {
			t.Errorf("%s: WCC %+v, want a Before and an After both after the held write", name, w)
		}
		_, _ = s.engine.Write(ctx, h.Share(), id, 18, []byte("later"), WriteOpts{})
		if got, _ := s.GetAttr(ctx, admin, h); got.Size == w.After.Size {
			t.Errorf("%s: a later write did not move GetAttr past the WCC", name)
		}
	}
}

// V42: Seek finds data and holes from where it is asked, the file's size
// when it finds none before the end, and ErrPastEnd from at or past the end.
func TestSeekFindsDataAndHoles(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "sparse", s.alice.Principal, 0o644)
	alice := proven(s.alice.Principal, inLab)
	content := append(append([]byte("abc"), make([]byte, 5)...), "de"...)
	_, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, content, vfs.Unstable)
	s.must(t, err)
	tail := s.file(t, "tail", s.alice.Principal, 0o644)
	_, err = s.Write(ctx, alice, vfs.Anonymous(tail), 0, append([]byte("ab"), make([]byte, 3)...), vfs.Unstable)
	s.must(t, err)
	for _, c := range []struct {
		h    vfs.Handle
		off  int64
		what vfs.SeekWhat
		want int64
		err  error
	}{
		{h, 0, vfs.SeekData, 0, nil}, {h, 0, vfs.SeekHole, 3, nil}, {h, 3, vfs.SeekData, 8, nil},
		{h, 5, vfs.SeekHole, 5, nil}, {h, 8, vfs.SeekHole, 10, nil}, {tail, 2, vfs.SeekData, 5, nil},
		{h, 10, vfs.SeekData, 0, vfs.ErrPastEnd}, {h, 10, vfs.SeekHole, 0, vfs.ErrPastEnd},
		{h, 40, vfs.SeekData, 0, vfs.ErrPastEnd}, {h, 40, vfs.SeekHole, 0, vfs.ErrPastEnd},
	} {
		if got, err := s.Seek(ctx, alice, vfs.Anonymous(c.h), c.off, c.what); !errors.Is(err, c.err) || err == nil && got != c.want {
			t.Errorf("Seek(%d, %v): %d, %v; want %d, %v", c.off, c.what, got, err, c.want, c.err)
		}
	}
	buf := make([]byte, 16)
	n, _, err := s.Read(ctx, alice, vfs.Anonymous(h), 0, buf)
	if err != nil || !bytes.Equal(buf[:n], content) {
		t.Errorf("the sparse file read back %q, %v", buf[:n], err)
	}
}

// An open holding append data without write data appends and overwrites
// nothing: bob, whom alice's setuid 0600 file's ACL allows read and append
// data, opens it for append, and his write and his deallocate from offset 0
// are ErrAccess and change no byte, while his write at its end appends,
// clearing setuid.
func TestAnAppendDataOpenOnlyAppends(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o600)
	alice := proven(s.alice.Principal, inLab)
	_, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("hello"), vfs.Unstable)
	s.must(t, err)
	_, err = s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithMode(0o4600))
	s.must(t, err)
	acl := entity.ACLFromMode(0o600)
	acl.Entries = append(acl.Entries, entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.AppendData, Who: s.bob.Principal})
	_, err = s.fs.Store.Files().SetACL(ctx, s.fs.Admin, h, acl)
	s.must(t, err)
	if m := s.record(t, h).Mode; m != 0o4600 {
		t.Fatalf("the file's mode %o, want 4600", m)
	}
	pc := s.smb(t, "bob-pc", s.bob.Principal)
	ref := s.opener(t, pc, "f", entity.AppendData)
	if _, err := s.Write(ctx, pc, ref, 0, []byte("HELLO"), vfs.Unstable); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a write at 0 through an append-data open: %v, want ErrAccess", err)
	}
	if _, err := s.Deallocate(ctx, pc, ref, 0, 5); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a deallocate at 0 through an append-data open: %v, want ErrAccess", err)
	}
	if _, err := s.Write(ctx, pc, ref, 5, []byte("!"), vfs.Unstable); err != nil {
		t.Errorf("an append through an append-data open: %v", err)
	}
	if m := s.record(t, h).Mode; m != 0o600 {
		t.Errorf("the append left mode %o, want 600", m)
	}
	buf := make([]byte, 8)
	if n, _, err := s.Read(ctx, alice, vfs.Anonymous(h), 0, buf); err != nil || string(buf[:n]) != "hello!" {
		t.Errorf("the file read back %q, %v; want hello!", buf[:n], err)
	}
}

// An anonymous write at or past a file's size is admitted by append data
// alone, one below it only by write data: bob, whom the ACL of alice's 0600
// file allows read and append data but not write data, appends to it, plain
// or append-only, and is refused a write below its size, staging nothing.
func TestAnAnonymousAppendAsksAppendData(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	files := s.fs.Store.Files()
	alice, bob := proven(s.alice.Principal, inLab), proven(s.bob.Principal, inLab)
	for _, c := range []struct {
		name  string
		flags entity.FileFlags
		below error
	}{{"plain", 0, vfs.ErrAccess}, {"append-only", entity.FlagAppendOnly, vfs.ErrPerm}} {
		h := s.file(t, c.name, s.alice.Principal, 0o600)
		_, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("hello"), vfs.Unstable)
		s.must(t, err)
		acl := entity.ACLFromMode(0o600)
		acl.Entries = append(acl.Entries, entity.ACE{Type: entity.ACEAllow, Mask: entity.ReadData | entity.AppendData, Who: s.bob.Principal})
		_, err = files.SetACL(ctx, s.fs.Admin, h, acl)
		s.must(t, err)
		_, err = files.SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithFlags(c.flags))
		s.must(t, err)
		if _, err := s.Write(ctx, bob, vfs.Anonymous(h), 5, []byte("!"), vfs.Unstable); err != nil {
			t.Errorf("%s: an anonymous append by a caller holding append data alone: %v", c.name, err)
		}
		s.engine.writing = func(entity.FileID) { t.Errorf("%s: a refused write below the size was staged", c.name) }
		if _, err := s.Write(ctx, bob, vfs.Anonymous(h), 0, []byte("H"), vfs.Unstable); !errors.Is(err, c.below) {
			t.Errorf("%s: an anonymous write below the size by a caller holding append data alone: %v, want %v", c.name, err, c.below)
		}
		s.engine.writing = nil
	}
}

// A share turned read-only refuses a write and a deallocate through an open
// taken while it was not, ErrReadOnly, staging nothing.
func TestAReadOnlyShareRefusesWritesThroughAnOpen(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	s.file(t, "f", s.alice.Principal, 0o666)
	pc := s.smb(t, "pc", s.alice.Principal)
	ref := s.opener(t, pc, "f", entity.ReadData|entity.WriteData)
	p, err := s.cp.ExportPolicy(ctx, s.share)
	s.must(t, err)
	p.ReadOnly = true
	s.must(t, s.cp.PutExportPolicy(ctx, p))
	s.engine.writing = func(entity.FileID) { t.Error("a write on a read-only share was staged") }
	s.engine.dealloc = func(entity.FileID) { t.Error("a deallocate on a read-only share was staged") }
	if _, err := s.Write(ctx, pc, ref, 0, []byte("x"), vfs.Unstable); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("a write through an open on a share turned read-only: %v, want ErrReadOnly", err)
	}
	if _, err := s.Deallocate(ctx, pc, ref, 0, 1); !errors.Is(err, vfs.ErrReadOnly) {
		t.Errorf("a deallocate through an open on a share turned read-only: %v, want ErrReadOnly", err)
	}
}

// A sync holds none of the file's locks: while a Commit, or a stable
// write's commit, waits on the engine, another open and close of the file
// run.
func TestASyncBlocksNoOpenOrClose(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	s.file(t, "f", s.alice.Principal, 0o666)
	pc := s.smb(t, "pc", s.alice.Principal)
	ref := s.opener(t, pc, "f", entity.ReadData|entity.WriteData)
	for name, sync := range map[string]func() error{
		"Commit": func() error { _, err := s.Commit(ctx, pc, ref, 0, 0); return err },
		"a stable Write": func() error {
			_, err := s.Write(ctx, pc, ref, 0, []byte("x"), vfs.FileSync)
			return err
		},
	} {
		entered, unblock := make(chan struct{}), make(chan struct{})
		s.engine.committing = func(entity.FileID) {
			close(entered)
			<-unblock
		}
		done := make(chan error, 1)
		go func() { done <- sync() }()
		<-entered
		short, cancel := context.WithTimeout(ctx, 5*time.Second)
		r, err := s.Open(short, pc, byName(s.fs.Root, "f", vfs.OpenExisting, entity.ReadData))
		if err == nil {
			_, err = s.Close(short, pc, r.Ref, vfs.Seqs{})
		}
		cancel()
		if err != nil {
			t.Errorf("an open and close while %s syncs: %v", name, err)
		}
		close(unblock)
		if err := <-done; err != nil {
			t.Errorf("%s: %v", name, err)
		}
		s.engine.committing = nil
	}
}

// A write through a delegation is decided against the caller's right on the
// file as it is now: alice's write delegation writes nothing once bob's file
// is made 0644.
func TestADelegatedWriteIsDecided(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	s.user(t, "alice41", "2000", s.share, entity.AccessReadWrite)
	bob := s.user(t, "bob41", "2001", s.share, entity.AccessReadWrite)
	h := s.file(t, "f", bob.Principal, 0o666)
	c := s.nfs41(t, "a-host", 2000, 1)
	q := byName(s.fs.Root, "f", vfs.OpenExisting, entity.ReadData|entity.WriteData)
	q.Want = vfs.GrantRead | vfs.GrantWrite
	r, err := s.Open(ctx, c, q)
	if err != nil || r.Grant == nil {
		t.Fatalf("the delegated open answered %+v, %v", r.Grant, err)
	}
	ref := vfs.Delegated(r.Grant.ID, h)
	if _, err := s.Write(ctx, c, ref, 0, []byte("x"), vfs.Unstable); err != nil {
		t.Fatalf("a write through the delegation: %v", err)
	}
	_, err = s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithMode(0o644))
	s.must(t, err)
	if _, err := s.Write(ctx, c, ref, 0, []byte("y"), vfs.Unstable); !errors.Is(err, vfs.ErrAccess) {
		t.Errorf("a write through the delegation once the file is 0644: %v, want ErrAccess", err)
	}
}

// An immutable file takes no write through an open taken before it was made
// so, ErrPerm, though the open holds write data.
func TestAnImmutableFileRefusesAWriteThroughAnOpen(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	pc := s.smb(t, "pc", s.alice.Principal)
	ref := s.opener(t, pc, "f", entity.ReadData|entity.WriteData)
	_, err := s.fs.Store.Files().SetAttrs(ctx, s.fs.Admin, h, entity.Attrs{}.WithFlags(entity.FlagImmutable))
	s.must(t, err)
	s.engine.writing = func(entity.FileID) { t.Error("a write to an immutable file was staged") }
	if _, err := s.Write(ctx, pc, ref, 0, []byte("x"), vfs.Unstable); !errors.Is(err, vfs.ErrPerm) {
		t.Errorf("a write through an open to an immutable file: %v, want ErrPerm", err)
	}
}

// I/O and a commit refuse a file that holds no content: ErrIsDir for a
// directory, ErrInvalid for a symbolic link.
func TestDataNeedsContent(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	admin := proven(s.fs.Admin.Principal, inLab)
	d, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "d", kvstore.Dir(0o777))
	ln, _ := s.fs.Create(t, s.fs.Admin, s.fs.Root, "ln", kvstore.Symlink("d"))
	for _, c := range []struct {
		h    vfs.Handle
		want error
	}{{d, vfs.ErrIsDir}, {ln, vfs.ErrInvalid}} {
		h, want := c.h, c.want
		if _, _, err := s.Read(ctx, admin, vfs.Anonymous(h), 0, make([]byte, 1)); !errors.Is(err, want) {
			t.Errorf("Read: %v, want %v", err, want)
		}
		if _, err := s.Commit(ctx, admin, vfs.Anonymous(h), 0, 0); !errors.Is(err, want) {
			t.Errorf("Commit: %v, want %v", err, want)
		}
	}
}

// A negative offset or length is ErrInvalid, and nothing is read, staged or
// synced.
func TestANegativeOffsetOrLengthIsInvalid(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	alice := proven(s.alice.Principal, inLab)
	read := func() error { _, _, err := s.Read(ctx, alice, vfs.Anonymous(h), -1, make([]byte, 1)); return err }
	write := func() error {
		_, err := s.Write(ctx, alice, vfs.Anonymous(h), -1, []byte("x"), vfs.Unstable)
		return err
	}
	commit := func(off, n int64) error { _, err := s.Commit(ctx, alice, vfs.Anonymous(h), off, n); return err }
	dealloc := func() error { _, err := s.Deallocate(ctx, alice, vfs.Anonymous(h), 0, -1); return err }
	s.engine.commits = nil
	for name, err := range map[string]error{
		"Read's offset": read(), "Write's offset": write(), "Commit's offset": commit(-1, 0),
		"Commit's length": commit(0, -1), "Deallocate's length": dealloc(),
	} {
		if !errors.Is(err, vfs.ErrInvalid) {
			t.Errorf("%s negative: %v, want ErrInvalid", name, err)
		}
	}
	if len(s.engine.commits) != 0 {
		t.Errorf("a negative commit synced %v", s.engine.commits)
	}
}

// A read the engine fails after some bytes answers those bytes and the
// error, and never the end of the file.
func TestAFailedReadAnswersItsBytesAndNoEnd(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	h := s.file(t, "f", s.alice.Principal, 0o666)
	alice := proven(s.alice.Principal, inLab)
	_, err := s.Write(ctx, alice, vfs.Anonymous(h), 0, []byte("hello"), vfs.Unstable)
	s.must(t, err)
	s.engine.readErr = vfs.ErrLost
	buf := make([]byte, 5)
	if n, eof, err := s.Read(ctx, alice, vfs.Anonymous(h), 0, buf); n != 5 || eof || !errors.Is(err, vfs.ErrLost) {
		t.Errorf("a read failing after its bytes: %d %v %v; want 5, no end, ErrLost", n, eof, err)
	}
}

// Only a commit through an SMB open is a flush that learns of a loss: one
// through an NFS open, or through an SMB lock state, answers the verifier.
// An unstable write through a lock state on a continuously available share
// is committed, as one through its open is.
func TestOnlyAnSMBOpenFlushesAndALockStateWritesStable(t *testing.T) {
	s := newAttrs(t)
	ctx := t.Context()
	s.user(t, "alice41", "2000", s.share, entity.AccessReadWrite)
	h := s.file(t, "f", s.alice.Principal, 0o666)
	nfs := s.nfs41(t, "a-host", 2000, 1)
	nref := s.opener(t, nfs, "f", entity.ReadData|entity.WriteData)
	pc := s.smb(t, "pc", s.alice.Principal)
	sref := s.opener(t, pc, "f", entity.ReadData|entity.WriteData)
	who := openstate.Caller{Client: pc.Client, Principal: s.alice.Principal, Protocol: entity.ProtocolSMB}
	unlock := s.Admission(fileOf(h))
	res, err := s.tab.Locks().Lock(ctx, who, keyOf(s.record(t, h)), openstate.LockReq{
		Owner: openstate.LockOwner{Client: pc.Client, Name: []byte("lo"), Stack: true}, Open: sref,
		Ranges: []openstate.LockRange{{ByteRange: openstate.ByteRange{Offset: 100, Length: 10}, Exclusive: true}},
	})
	unlock()
	s.must(t, err)
	lref := vfs.OpenRef{Kind: openstate.RefLocked, State: res.Ref, Handle: h}

	s.engine.mu.Lock()
	s.engine.loss = 1
	s.engine.mu.Unlock()
	for name, o := range map[string]struct {
		c   vfs.Call
		ref vfs.OpenRef
	}{"an NFS open": {nfs, nref}, "an SMB lock state": {pc, lref}} {
		if _, err := s.Commit(ctx, o.c, o.ref, 0, 0); err != nil {
			t.Errorf("a commit through %s across a loss: %v", name, err)
		}
	}

	p, err := s.cp.ExportPolicy(ctx, s.share)
	s.must(t, err)
	p.SMB |= entity.SMBContinuouslyAvailable
	s.must(t, s.cp.PutExportPolicy(ctx, p))
	s.engine.commits = nil
	if _, err := s.Write(ctx, pc, lref, 0, []byte("abc"), vfs.Unstable); err != nil {
		t.Fatalf("a write through the lock state: %v", err)
	}
	if got := s.engine.commits; len(got) != 1 || got[0] != (entity.Extent{Offset: 0, Length: 3}) {
		t.Errorf("an unstable write through a lock state on a continuously available share: committed %v, want its extent", got)
	}
}
