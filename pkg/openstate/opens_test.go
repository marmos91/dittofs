package openstate

import (
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

const (
	rd  = entity.ReadData
	wr  = entity.WriteData
	del = entity.Delete
)

func smb(name string) Client {
	return Client{ClientRecord: entity.ClientRecord{Protocol: entity.ProtocolSMB, Name: []byte(name), Boot: [8]byte{1}}}
}

// caller connects c and answers a caller of it running as a new principal.
func (f *fixture) caller(t *testing.T, c Client) Caller {
	t.Helper()
	p := c.Owner
	if p.IsZero() {
		p = entity.NewUser()
	}
	return Caller{Client: f.connect(t, c), Principal: p}
}

func (f *fixture) open(who Caller, file entity.FileID, r OpenRequest) (Open, error) {
	res, err := f.Opens().Open(ctx, who, file, r)
	return res.Open, err
}

func (f *fixture) mustOpen(t *testing.T, who Caller, file entity.FileID, r OpenRequest) Open {
	t.Helper()
	o, err := f.open(who, file, r)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return o
}

func (f *fixture) close(t *testing.T, who Caller, o Open) CloseResult {
	t.Helper()
	r, err := f.Opens().Close(ctx, who, OpenRef{ID: o.ID})
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	return r
}

// durable is the records Durable lists for file, committing nothing.
func (f *fixture) durable(t *testing.T, file entity.FileID) []entity.OpenRecord {
	t.Helper()
	recs, commit, err := f.Opens().Durable(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	commit(false)
	return recs
}

// fileIn creates name in the share's root and returns its ID and the
// request that opens it through the root.
func (f *fixture) fileIn(t *testing.T, name string) (entity.FileID, OpenRequest) {
	t.Helper()
	_, file := f.fs.Create(t, f.fs.Admin, f.fs.Root, name, kvstore.NewFile{Type: entity.Regular, Mode: 0o644})
	_, root, err := f.fs.Root.Parse()
	if err != nil {
		t.Fatal(err)
	}
	return file.ID, OpenRequest{Share: f.fs.Share.ID, Shard: ShardID(file.Shard), Via: root, Name: []byte(name)}
}

// unlink removes name from the share's root the way the service does: the
// file's volatile opens are written in the unlink's transaction, and the
// table is told whether they were.
func (f *fixture) unlink(t *testing.T, file entity.FileID, name string) {
	t.Helper()
	recs, commit, err := f.Opens().Durable(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	o, err := f.fs.Store.Namespace().Unlink(ctx, f.fs.Admin, f.fs.Root, []byte(name), recs...)
	commit(err == nil && o.File == file)
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) stored(t *testing.T, file entity.FileID) []entity.OpenRecord {
	t.Helper()
	got, err := f.fs.Store.Opens().Of(ctx, f.fs.Share.ID, file)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A deny mode is checked both ways, once, at open; an anonymous read or
// write is checked against the deny modes held, per operation; a conflicting
// open is refused, never granted narrower.
func TestDenyModesAtOpen(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("alice-pc")), f.caller(t, nfs("bob", 1, entity.NewUser()))
	file := entity.NewFileID()
	held := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr, Deny: wr})

	if err := f.Checks().CheckIO(ctx, Caller{}, OpenRef{File: file}, ByteRange{}, true); !errors.Is(err, ErrShareViolation) {
		t.Errorf("an anonymous write under a deny-write: %v, want ErrShareViolation", err)
	}
	if err := f.Checks().CheckIO(ctx, Caller{}, OpenRef{File: file}, ByteRange{}, false); err != nil {
		t.Errorf("an anonymous read under a deny-write: %v", err)
	}
	if err := f.Checks().CheckIO(ctx, a, OpenRef{ID: held.ID}, ByteRange{}, true); err != nil {
		t.Errorf("a write through the deny-write open itself: %v; a granted open is not judged again", err)
	}

	if _, err := f.open(b, file, OpenRequest{Access: wr}); !errors.Is(err, ErrShareViolation) {
		t.Errorf("a write open against a deny-write: %v, want ErrShareViolation", err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd, Deny: wr}); !errors.Is(err, ErrShareViolation) {
		t.Errorf("a read open denying write against a writer: %v, want ErrShareViolation", err)
	}
	if recs := f.durable(t, file); len(recs) != 1 || recs[0].ID != held.ID {
		t.Fatalf("after the refusals the file holds %v, want the first open alone", ids(recs))
	}
	if _, err := f.open(b, file, OpenRequest{Access: rd}); err != nil {
		t.Errorf("a read open denying nothing: %v", err)
	}
}

// An open asking no data access is outside the deny check both ways: it is
// granted against any deny mode, and its own deny refuses no open and no
// anonymous I/O.
func TestStatOpenIsOutsideTheDenyCheck(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("alice-pc")), f.caller(t, smb("bob-pc"))
	all, stat := rd|wr|del, entity.ReadAttributes
	file := entity.NewFileID()
	f.mustOpen(t, a, file, OpenRequest{Access: all, Deny: all})
	if _, err := f.open(b, file, OpenRequest{Access: stat, Deny: all}); err != nil {
		t.Errorf("an attributes-only open against a deny-all: %v", err)
	}

	file = entity.NewFileID()
	f.mustOpen(t, b, file, OpenRequest{Access: stat, Deny: all})
	if _, err := f.open(a, file, OpenRequest{Access: all, Deny: all}); err != nil {
		t.Errorf("an all-access open beside an attributes-only deny-all: %v", err)
	}
	file = entity.NewFileID()
	f.mustOpen(t, b, file, OpenRequest{Access: stat, Deny: all})
	if err := f.Checks().CheckIO(ctx, Caller{}, OpenRef{File: file}, ByteRange{}, true); err != nil {
		t.Errorf("an anonymous write beside an attributes-only deny-all: %v", err)
	}
}

// A downgrade narrows an open to masks within its own, applies at once, and
// raises its reference's sequence, so the reference as it was is old; I/O
// through the open is held to what it holds now.
func TestDowngrade(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, smb("alice-pc")), f.caller(t, smb("bob-pc"))
	file := entity.NewFileID()
	o := f.mustOpen(t, a, file, OpenRequest{Access: rd | wr, Deny: wr})
	ref := OpenRef{Ref: o.Ref, File: file}

	if err := f.Opens().Downgrade(ctx, a, ref, rd|wr|del, wr); !errors.Is(err, ErrInvalid) {
		t.Errorf("a downgrade to more access: %v, want ErrInvalid", err)
	}
	if err := f.Opens().Downgrade(ctx, a, ref, rd, 0); err != nil {
		t.Fatal(err)
	}
	if err := f.Checks().CheckIO(ctx, a, ref, ByteRange{}, false); !errors.Is(err, ErrOldState) {
		t.Errorf("the reference as it was before the downgrade: %v, want ErrOldState", err)
	}
	now := OpenRef{Ref: StateRef{Type: o.Ref.Type, Shard: o.Ref.Shard, Token: o.Ref.Token}, File: file}
	if err := f.Checks().CheckIO(ctx, a, now, ByteRange{}, true); !errors.Is(err, ErrAccess) {
		t.Errorf("a write through the open downgraded to read: %v, want ErrAccess", err)
	}
	if err := f.Checks().CheckIO(ctx, a, now, ByteRange{}, false); err != nil {
		t.Errorf("a read through the open downgraded to read: %v", err)
	}
	if _, err := f.open(b, file, OpenRequest{Access: wr}); err != nil {
		t.Errorf("a write open once the deny-write was downgraded away: %v", err)
	}
}

// An open is used only by its opener, or by its client's owner
// where the client protects its state.
func TestOpenUsedOnlyByItsOpener(t *testing.T) {
	f := newFixture(t)
	owner := entity.NewUser()
	id := f.connect(t, nfs("host-a", 1, owner))
	opener := Caller{Client: id, Principal: entity.NewUser()}
	file := entity.NewFileID()
	o := f.mustOpen(t, opener, file, OpenRequest{Access: rd | del})

	other := Caller{Client: id, Principal: entity.NewUser()}
	if err := f.Checks().CheckIO(ctx, other, OpenRef{ID: o.ID}, ByteRange{}, false); !errors.Is(err, ErrNotYours) {
		t.Errorf("I/O through the open by another principal: %v, want ErrNotYours", err)
	}
	if _, err := f.Opens().Close(ctx, other, OpenRef{ID: o.ID}); !errors.Is(err, ErrNotYours) {
		t.Errorf("a close by another principal: %v, want ErrNotYours", err)
	}
	if err := f.Checks().CheckIO(ctx, Caller{Client: id, Principal: owner}, OpenRef{ID: o.ID}, ByteRange{}, false); err != nil {
		t.Errorf("I/O through the open by the protecting owner: %v", err)
	}
}

// An open that keeps an unlinked file alive is durable by the time the
// unlink commits, and an aborted unlink leaves it volatile. An open of the
// file after it has no entry is written durable too, and the last close
// reports the file orphaned.
func TestUnlinkMakesOpensDurable(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	file, req := f.fileIn(t, "a")
	first := f.mustOpen(t, a, file, req)

	_, commit, err := f.Opens().Durable(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	commit(false)
	if recs := f.durable(t, file); len(recs) != 1 || recs[0].ID != first.ID {
		t.Fatalf("after an aborted unlink the volatile opens are %v, want the open", ids(recs))
	}

	f.unlink(t, file, "a")
	if got := f.stored(t, file); len(got) != 1 || got[0].ID != first.ID || got[0].Principal != a.Principal {
		t.Fatalf("stored opens after the unlink: %v", ids(got))
	}
	byHandle := f.mustOpen(t, a, file, req)
	if got := f.stored(t, file); len(got) != 2 {
		t.Fatalf("stored opens after an open of the unlinked file: %v, want both", ids(got))
	}
	if r := f.close(t, a, first); r.Orphan {
		t.Error("the first close reported the file orphaned while an open holds it")
	}
	if r := f.close(t, a, byHandle); !r.Orphan {
		t.Error("the last close did not report the file orphaned")
	}
	if got := f.stored(t, file); len(got) != 0 {
		t.Errorf("stored opens after both closes: %v", ids(got))
	}
}

// An open of a file with no entry is written durable whether or not this
// table saw the unlink: after the entry was freed with its last open, and
// after a restart.
func TestOpenOfAnUnlinkedFileIsDurable(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	file, req := f.fileIn(t, "a")
	first := f.mustOpen(t, a, file, req)
	f.unlink(t, file, "a")
	if r := f.close(t, a, first); !r.Orphan {
		t.Fatal("the last close did not report the file orphaned")
	}

	req.Unlinked = true
	again := f.mustOpen(t, a, file, req)
	if got := f.stored(t, file); len(got) != 1 || got[0].ID != again.ID {
		t.Errorf("stored opens after an open of the file once its entry was freed: %v", ids(got))
	}

	g := reopen(t, f.fs, f.clock)
	b := g.caller(t, nfs("host-b", 1, entity.NewUser()))
	later := g.mustOpen(t, b, file, req)
	if got := f.stored(t, file); len(got) != 2 || got[1].ID != later.ID {
		t.Errorf("stored opens after an open of the file after a restart: %v", ids(got))
	}
}

// Opening and closing a linked file writes no open record, and writes
// the client record's shard list once per client and shard, even across a
// restart; only the first open reads the record.
func TestLinkedOpensWriteOnlyTheShardList(t *testing.T) {
	f := newFixture(t)
	a, b := f.caller(t, nfs("host-a", 1, entity.NewUser())), f.caller(t, nfs("host-b", 1, entity.NewUser()))
	file, req := f.fileIn(t, "a")
	f.close(t, a, f.mustOpen(t, a, file, req)) // reserves the open IDs' range

	f.fs.Counts.Take()
	for range 1000 {
		f.close(t, b, f.mustOpen(t, b, file, req))
	}
	// The client record is read for the first open only: a later one finds
	// the shard listed in memory.
	if got := f.fs.Counts.Take(); got.Sets != 1 || got.Deletes != 0 || got.Gets > 10 {
		t.Errorf("1000 opens and closes of a linked file: %+v, want the shard list's one write and its reads", got)
	}

	g := reopen(t, f.fs, f.clock)
	if _, err := g.Renew(ctx, b.Client); err != nil {
		t.Fatal(err)
	}
	f.fs.Counts.Take()
	g.close(t, b, g.mustOpen(t, b, file, req))
	if got := f.fs.Counts.Take(); got.Sets != 0 || got.Deletes != 0 {
		t.Errorf("an open after a restart by a client already listed: %+v, want no write", got)
	}
}

// A file is delete pending once an open with delete on close
// closes; then every new open and every rename is refused, and the last
// close hands the service the unlink of the marking open's entry, run as the
// principal that marked it, never as the last closer. A delete-pending
// directory gains no name. Only an open holding
// delete access may mark the file, and a disposition set through one is
// cleared through one.
func TestDeletePending(t *testing.T) {
	f := newFixture(t)
	p, q := f.caller(t, smb("p-pc")), f.caller(t, smb("q-pc"))
	nfsCaller := f.caller(t, nfs("host-n", 1, entity.NewUser()))
	file := entity.NewFileID()
	dir := entity.NewFileID()
	marker := f.mustOpen(t, p, file, OpenRequest{Via: dir, Name: []byte("a"), Access: rd | del, DeleteOnClose: true})
	other := f.mustOpen(t, q, file, OpenRequest{Via: dir, Name: []byte("a"), Access: rd})

	yes, no := true, false
	if err := f.Opens().Update(ctx, q, other.ID, OpenUpdate{Delete: &yes}); !errors.Is(err, ErrAccess) {
		t.Errorf("a disposition set through an open without delete access: %v, want ErrAccess", err)
	}
	if _, err := f.open(q, file, OpenRequest{Access: rd, DeleteOnClose: true}); !errors.Is(err, ErrAccess) {
		t.Errorf("delete on close without delete access: %v, want ErrAccess", err)
	}
	if r := f.close(t, p, marker); r.Unlink != nil {
		t.Fatalf("a close with another open left: unlink %+v", r.Unlink)
	}
	if _, err := f.open(nfsCaller, file, OpenRequest{Access: rd}); !errors.Is(err, ErrDeletePending) {
		t.Errorf("an open of the delete-pending file: %v, want ErrDeletePending", err)
	}
	if err := f.Checks().CheckChange(ctx, nfsCaller, file, ChangeRename); !errors.Is(err, ErrDeletePending) {
		t.Errorf("a rename of the delete-pending file: %v, want ErrDeletePending", err)
	}
	if err := f.Checks().CheckChange(ctx, nfsCaller, file, ChangeAddEntry); !errors.Is(err, ErrDeletePending) {
		t.Errorf("a name added to a delete-pending directory: %v, want ErrDeletePending", err)
	}
	if err := f.Checks().CheckChange(ctx, nfsCaller, file, ChangeRemove); err != nil {
		t.Errorf("an unlink of the delete-pending file's name: %v", err)
	}
	r := f.close(t, q, other)
	if u := r.Unlink; u == nil || u.Parent != dir || string(u.Name) != "a" || u.By != p.Principal || u.Access != rd|del {
		t.Fatalf("the last close's unlink %+v, want a's entry, run as the marker with its grant", u)
	}

	// A disposition is set and cleared through an open holding delete access.
	setter := f.mustOpen(t, p, file, OpenRequest{Access: rd | del})
	if err := f.Opens().Update(ctx, p, setter.ID, OpenUpdate{Delete: &yes}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.open(q, file, OpenRequest{Access: rd}); !errors.Is(err, ErrDeletePending) {
		t.Errorf("an open after the disposition was set: %v, want ErrDeletePending", err)
	}
	if err := f.Opens().Update(ctx, p, setter.ID, OpenUpdate{Delete: &no}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.open(q, file, OpenRequest{Access: rd}); err != nil {
		t.Errorf("an open after the disposition was cleared: %v", err)
	}
}

// An open's suspended times are its own: suspending one open's modify
// time leaves another's alone, and a resume clears it. They are carried in
// the durable record an unlink writes.
func TestSuspendedTimesArePerOpen(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, smb("alice-pc"))
	file := entity.NewFileID()
	first, second := f.mustOpen(t, a, file, OpenRequest{Access: wr}), f.mustOpen(t, a, file, OpenRequest{Access: wr})
	suspended := func() map[entity.OpenID]entity.TimeMask {
		m := map[entity.OpenID]entity.TimeMask{}
		for _, r := range f.durable(t, file) {
			m[r.ID] = r.Suspended
		}
		return m
	}

	if err := f.Opens().Update(ctx, a, first.ID, OpenUpdate{Suspend: entity.TimeModify}); err != nil {
		t.Fatal(err)
	}
	if got := suspended(); got[first.ID] != entity.TimeModify || got[second.ID] != 0 {
		t.Errorf("after suspending the first open's modify time: %v", got)
	}
	if err := f.Opens().Update(ctx, a, first.ID, OpenUpdate{Resume: entity.TimeModify}); err != nil {
		t.Fatal(err)
	}
	if got := suspended(); got[first.ID] != 0 {
		t.Errorf("after the resume: %v", got)
	}
}

// An NFS remove or rename of a file is refused
// against a deny-delete, which an SMB caller's own open was judged against at
// open. An SMB rename or delete of a directory is refused while an open made
// through it exists, an NFS one is not, and an open two levels below does
// not count.
func TestChangeChecks(t *testing.T) {
	f := newFixture(t)
	s, n := f.caller(t, smb("alice-pc")), f.caller(t, nfs("bob", 1, entity.NewUser()))
	file := entity.NewFileID()
	f.mustOpen(t, s, file, OpenRequest{Access: rd | del, Deny: del})
	if err := f.Checks().CheckChange(ctx, n, file, ChangeRemove); !errors.Is(err, ErrShareViolation) {
		t.Errorf("an NFS remove against a deny-delete: %v, want ErrShareViolation", err)
	}
	if err := f.Checks().CheckChange(ctx, s, file, ChangeRename); err != nil {
		t.Errorf("an SMB rename through its own deny-delete open: %v", err)
	}

	d, e := entity.NewFileID(), entity.NewFileID()
	child := f.mustOpen(t, s, entity.NewFileID(), OpenRequest{Via: d, Access: rd})
	f.mustOpen(t, s, entity.NewFileID(), OpenRequest{Via: e, Access: rd}) // e is a directory in d
	for _, what := range []ChangeMask{ChangeRename, ChangeRemove} {
		if err := f.Checks().CheckChange(ctx, s, d, what); !errors.Is(err, ErrShareViolation) {
			t.Errorf("an SMB change %d of a directory with an open child: %v, want ErrShareViolation", what, err)
		}
	}
	if err := f.Checks().CheckChange(ctx, n, d, ChangeRename); err != nil {
		t.Errorf("an NFS rename of a directory with an open child: %v", err)
	}
	f.close(t, s, child)
	if err := f.Checks().CheckChange(ctx, s, d, ChangeRename); err != nil {
		t.Errorf("an SMB rename of a directory whose only open is two levels below: %v", err)
	}
}

// An open of a named stream is an open of its base file for deny
// modes, and its durable record names the stream.
func TestStreamOpenIsAnOpenOfItsBase(t *testing.T) {
	f := newFixture(t)
	s, n := f.caller(t, smb("alice-pc")), f.caller(t, nfs("bob", 1, entity.NewUser()))
	base, stream := entity.NewFileID(), entity.NewFileID()
	f.mustOpen(t, s, base, OpenRequest{Stream: stream, Access: rd, Deny: wr})
	if _, err := f.open(n, base, OpenRequest{Access: wr}); !errors.Is(err, ErrShareViolation) {
		t.Errorf("a write open of the base under the stream's deny-write: %v, want ErrShareViolation", err)
	}
	if recs := f.durable(t, base); len(recs) != 1 || recs[0].File != stream {
		t.Errorf("the base's durable records %d records, want one naming the stream", len(recs))
	}
}

// An expiry closes the client's opens itself, and hands the
// service, through Closed, each file it left orphaned or to unlink; a client
// whose lease has ended is granted no open, swept or not.
func TestExpiryClosesOpens(t *testing.T) {
	f := newFixture(t)
	a := f.caller(t, nfs("host-a", 1, entity.NewUser()))
	unlinked, req := f.fileIn(t, "a")
	f.mustOpen(t, a, unlinked, req)
	f.mustOpen(t, a, unlinked, req)
	f.unlink(t, unlinked, "a")
	marked := entity.NewFileID()
	dir := entity.NewFileID()
	f.mustOpen(t, a, marked, OpenRequest{Via: dir, Name: []byte("m"), Access: del, DeleteOnClose: true})

	if err := f.Expire(ctx, a.Client); err != nil {
		t.Fatal(err)
	}
	got := map[entity.FileID]CloseResult{}
	for _, c := range f.sink.closed {
		got[c.f] = c.r
	}
	if r := got[unlinked]; !r.Orphan || len(got) != 2 {
		t.Errorf("Closed for the unlinked file: %+v, of %d files", r, len(got))
	}
	if r := got[marked]; r.Unlink == nil || r.Unlink.By != a.Principal || r.Unlink.Parent != dir {
		t.Errorf("Closed for the marked file: %+v", r)
	}
	if s := f.stored(t, unlinked); len(s) != 0 {
		t.Errorf("stored opens after the expiry: %v", ids(s))
	}

	b := f.caller(t, nfs("host-b", 1, entity.NewUser()))
	f.clock.mu.Lock()
	f.clock.now += 2 * leasePeriod // past the lease, firing no sweep
	f.clock.mu.Unlock()
	if _, err := f.open(b, entity.NewFileID(), OpenRequest{Access: rd}); !errors.Is(err, ErrStaleClient) {
		t.Errorf("an open by a client whose lease ended: %v, want ErrStaleClient", err)
	}
}

func ids(recs []entity.OpenRecord) []entity.OpenID {
	out := make([]entity.OpenID, len(recs))
	for i, r := range recs {
		out[i] = r.ID
	}
	return out
}
