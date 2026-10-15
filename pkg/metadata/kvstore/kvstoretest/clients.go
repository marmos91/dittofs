package kvstoretest

import (
	"bytes"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// RFC 14 §2.1 and §8: a client record is found by ID and by its owner after
// a restart, its owner is unique under its protocol, a rewrite that changes
// nothing writes nothing, and a reboot or a delete leaves no row of the old
// record behind.
func testClients(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cs := s.Clients()
	alice := entity.NewUser()
	nfs, err := cs.Create(ctx, entity.Client{Protocol: entity.ProtocolNFSv4, Owner: []byte("host-a"), Verifier: [8]byte{1}, Principal: alice})
	must(t, err)
	if nfs.ID == 0 {
		t.Fatal("a client was given ID 0")
	}
	smb, err := cs.Create(ctx, entity.Client{Protocol: entity.ProtocolSMB, Owner: []byte("host-a")})
	must(t, err)
	if smb.ID == nfs.ID {
		t.Fatal("two clients share an ID")
	}
	if _, err := cs.Create(ctx, entity.Client{Protocol: entity.ProtocolNFSv4, Owner: []byte("host-a")}); !isExist(err) {
		t.Errorf("a second client under one owner: %v", err)
	}

	// After a restart, a negotiate finds the record by owner.
	again, err := kvstore.Open(ctx, c)
	must(t, err)
	got, err := again.Clients().ByOwner(ctx, entity.ProtocolNFSv4, []byte("host-a"))
	must(t, err)
	if got.ID != nfs.ID || got.Principal != alice || got.Verifier != nfs.Verifier {
		t.Errorf("by owner after a restart: %+v", got)
	}
	if _, err := cs.ByOwner(ctx, entity.ProtocolNLM, []byte("host-a")); !merrs.IsNotFoundError(err) {
		t.Errorf("an owner under another protocol: %v", err)
	}

	c.Take()
	must(t, cs.Put(ctx, got))
	if n := c.Take(); n.Sets != 0 {
		t.Errorf("a rewrite that changed nothing wrote %d keys", n.Sets)
	}
	got.Revoked = true
	must(t, cs.Put(ctx, got))
	if r, err := cs.Get(ctx, nfs.ID); err != nil || !r.Revoked {
		t.Errorf("after a rewrite: %+v, %v", r, err)
	}
	moved := got
	moved.Owner = []byte("host-b")
	if err := cs.Put(ctx, moved); err == nil {
		t.Error("a rewrite moved the client's owner")
	}

	must(t, cs.PutShard(ctx, nfs.ID, entity.ClientShard{Shard: 3, Reclaimed: 2}))
	must(t, cs.PutShard(ctx, nfs.ID, entity.ClientShard{Shard: 1, LossPending: true}))
	if sh, err := cs.Shards(ctx, nfs.ID); err != nil || len(sh) != 2 || sh[0].Shard != 1 || !sh[0].LossPending || sh[1].Reclaimed != 2 {
		t.Errorf("shards %+v, %v", sh, err)
	}
	if err := cs.PutShard(ctx, entity.ClientID(1<<40), entity.ClientShard{Shard: 1}); !merrs.IsNotFoundError(err) {
		t.Errorf("a shard record for no client: %v", err)
	}

	// A reboot moves the owner to a new ID and drops the old record.
	rebooted, err := cs.Reboot(ctx, nfs.ID, entity.Client{Protocol: entity.ProtocolNFSv4, Owner: []byte("host-a"), Verifier: [8]byte{2}, Principal: alice})
	must(t, err)
	if rebooted.ID == nfs.ID {
		t.Error("a reboot kept the old ID")
	}
	if got, err := cs.ByOwner(ctx, entity.ProtocolNFSv4, []byte("host-a")); err != nil || got.ID != rebooted.ID {
		t.Errorf("owner after the reboot: %+v, %v", got, err)
	}
	if _, err := cs.Get(ctx, nfs.ID); !merrs.IsNotFoundError(err) {
		t.Errorf("the old record after a reboot: %v", err)
	}
	if sh, err := cs.Shards(ctx, nfs.ID); err != nil || len(sh) != 0 {
		t.Errorf("the old record's shards after a reboot: %+v, %v", sh, err)
	}
	if _, err := cs.Reboot(ctx, smb.ID, entity.Client{Protocol: entity.ProtocolSMB, Owner: []byte("host-b")}); err == nil {
		t.Error("a reboot changed the client's owner")
	}

	must(t, cs.PutShard(ctx, smb.ID, entity.ClientShard{Shard: 1}))
	must(t, cs.Delete(ctx, smb.ID))
	if _, err := cs.ByOwner(ctx, entity.ProtocolSMB, []byte("host-a")); !merrs.IsNotFoundError(err) {
		t.Errorf("owner after a delete: %v", err)
	}
	if sh, err := cs.Shards(ctx, smb.ID); err != nil || len(sh) != 0 {
		t.Errorf("shards after a delete: %+v, %v", sh, err)
	}
	if _, err := cs.Create(ctx, entity.Client{Protocol: entity.ProtocolSMB, Owner: []byte("host-a")}); err != nil {
		t.Errorf("the owner of a deleted client is not free: %v", err)
	}
}

// RFC 14 §2.1: CREATE_SESSION is sequenced in the client's record. A replay
// of the last one applied answers its cached reply, after a restart too; the
// next one records its reply; any other is misordered.
func testCreateSession(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cs := s.Clients()
	cl, err := cs.Create(ctx, entity.Client{Protocol: entity.ProtocolNFSv4, Owner: []byte("h"), CSSeq: 4})
	must(t, err)
	if cached, err := cs.CreateSession(ctx, cl.ID, 5, []byte("first")); err != nil || cached != nil {
		t.Fatalf("the next sequence: %q, %v", cached, err)
	}
	again, err := kvstore.Open(ctx, c)
	must(t, err)
	if cached, err := again.Clients().CreateSession(ctx, cl.ID, 5, []byte("second")); err != nil || !bytes.Equal(cached, []byte("first")) {
		t.Errorf("a replay after a restart: %q, %v", cached, err)
	}
	for _, seq := range []uint32{4, 7} {
		if _, err := cs.CreateSession(ctx, cl.ID, seq, nil); !errors.Is(err, entity.ErrSeqMisordered) {
			t.Errorf("sequence %d: %v", seq, err)
		}
	}
	if got, err := cs.Get(ctx, cl.ID); err != nil || got.CSSeq != 5 || !bytes.Equal(got.CSReply, []byte("first")) {
		t.Errorf("record after a replay and two misordered: %+v, %v", got, err)
	}
}

// Two clients racing to claim one owner: whichever commits second is
// refused, and the owner names the first.
func testClientOwnerRace(t *testing.T, kvo Opener) {
	s, h := heldStore(t, kvo.backends()[0])
	cs := s.Clients()
	// Reserve the ID range first, so the race lands in the record's
	// transaction, not the allocator's.
	_, err := cs.Create(ctx, entity.Client{Protocol: entity.ProtocolSMB, Owner: []byte("warm")})
	must(t, err)
	var first entity.Client
	outer, inner := race(h,
		func() error {
			_, err := cs.Create(ctx, entity.Client{Protocol: entity.ProtocolSMB, Owner: []byte("g")})
			return err
		},
		func() (err error) {
			first, err = cs.Create(ctx, entity.Client{Protocol: entity.ProtocolSMB, Owner: []byte("g")})
			return err
		})
	must(t, inner)
	if !isExist(outer) {
		t.Fatalf("the second claim: %v", outer)
	}
	if got, err := cs.ByOwner(ctx, entity.ProtocolSMB, []byte("g")); err != nil || got.ID != first.ID {
		t.Errorf("owner names %+v, %v; want %d", got, err, first.ID)
	}
}
