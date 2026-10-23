package kvstore

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
)

// RFC 14 §9.1 and L36: an open of a named stream keeps its base file alive.
// The unlink of the base's last name writes the stream's open durable under
// the stream, indexed in the stream's shard, and its close reports the base
// orphaned only once no open of the base or of any of its streams is left.
// Streams have no public create yet, so the stream is written as raw rows.
func TestStreamOpenKeepsBaseAlive(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	admin, id, root := adminRoot(t, f)
	_, base, err := f.s.Namespace().Create(ctx, id, root, []byte("a"), NewFile{Type: entity.Regular, Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	stream := entity.File{ID: entity.NewFileID(), Share: f.share, Shard: 7, Type: entity.Stream, StreamOf: base.ID, Version: entity.VersionOf(1)}
	rows(t, f.s, fileKey(f.share, stream.ID), encodeFile(stream), append(streamsPrefix(f.share, base.ID), stream.ID[:]...), encodeEmpty())

	onStream := entity.OpenRecord{ID: 1, File: stream.ID, Client: 1, Principal: admin.Principal}
	onBase := entity.OpenRecord{ID: 2, File: base.ID, Client: 1, Principal: admin.Principal}
	if _, err := f.s.Namespace().Unlink(ctx, id, root, []byte("a"), onStream, onBase); err != nil {
		t.Fatal(err)
	}
	if got, err := f.s.Opens().Of(ctx, f.share, stream.ID); err != nil || len(got) != 1 || got[0].ID != onStream.ID {
		t.Fatalf("the stream's durable opens after the base's unlink: %+v, %v", got, err)
	}
	var indexed []DurableOpen
	for d, err := range f.s.Opens().Durable(ctx, f.share, stream.Shard) {
		if err != nil {
			t.Fatal(err)
		}
		indexed = append(indexed, d)
	}
	if len(indexed) != 1 || indexed[0] != (DurableOpen{File: stream.ID, Open: onStream.ID}) {
		t.Errorf("the stream's shard's index: %+v", indexed)
	}

	closeOpen := func(file entity.FileID, op entity.OpenRecord, want Orphaned) {
		t.Helper()
		if got, err := f.s.Opens().Delete(ctx, f.share, file, op.ID); err != nil || got != want {
			t.Fatalf("closing open %d: orphaned %v, %v; want %v", op.ID, got, err, want)
		}
	}
	closeOpen(stream.ID, onStream, Orphaned{}) // the base's open still holds it
	again := entity.OpenRecord{ID: 3, File: stream.ID, Client: 1, Principal: admin.Principal}
	if err := f.s.Opens().Put(ctx, f.share, again); err != nil {
		t.Fatal(err)
	}
	closeOpen(base.ID, onBase, Orphaned{}) // the stream's open still holds it
	closeOpen(stream.ID, again, Orphaned{File: base.ID})
}

// An unlink's opens are of the file or of its named streams: an open of any
// other file, existing or gone, is a caller's error, refused before the
// name goes.
func TestUnlinkRefusesAnUnrelatedOpen(t *testing.T) {
	f := newContentFix(t, nil)
	ctx := context.Background()
	admin, id, root := adminRoot(t, f)
	if _, _, err := f.s.Namespace().Create(ctx, id, root, []byte("a"), NewFile{Type: entity.Regular, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	_, other, err := f.s.Namespace().Create(ctx, id, root, []byte("b"), NewFile{Type: entity.Regular, Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	for name, file := range map[string]entity.FileID{"another file's": other.ID, "a missing file's": entity.NewFileID()} {
		op := entity.OpenRecord{ID: 1, File: file, Client: 1, Principal: admin.Principal}
		if _, err := f.s.Namespace().Unlink(ctx, id, root, []byte("a"), op); !isInvalid(err) {
			t.Errorf("an unlink carrying %s open: %v", name, err)
		}
	}
	if _, _, err := f.s.Namespace().Lookup(ctx, id, root, []byte("a")); err != nil {
		t.Errorf("the name after the refused unlinks: %v", err)
	}
}

// adminRoot makes a privileged administrator of f's share and returns it,
// its identity and the share's root.
func adminRoot(t *testing.T, f *cfix) (entity.User, entity.Identity, entity.Handle) {
	t.Helper()
	ctx := context.Background()
	cp := f.s.ControlPlane()
	admin := entity.User{Principal: entity.Principal{Kind: entity.PrincipalUser, ID: entity.NewPrincipalID()}, Name: "admin"}
	if err := cp.PutUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if err := cp.SetGrant(ctx, entity.ShareGrant{Share: f.share, Principal: admin.Principal, Access: entity.AccessAdmin}); err != nil {
		t.Fatal(err)
	}
	id := entity.Identity{Principal: admin.Principal, Privileged: true}
	root, _, err := f.s.Files().Root(ctx, id, f.share)
	if err != nil {
		t.Fatal(err)
	}
	return admin, id, root
}
