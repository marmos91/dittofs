package kvstoretest

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	merrs "github.com/marmos91/dittofs/pkg/metadata/errors"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

func clientID(t *testing.T, s *kvstore.Store) entity.ClientID {
	t.Helper()
	id, err := s.NextID(ctx, kvstore.IDClient)
	must(t, err)
	return entity.ClientID(id)
}

func invalid(err error) bool { return isCode(err, merrs.ErrInvalidArgument) }

// RFC 14 §2.1 and §8: a client record keeps every field, its shard marks
// included, and is found by ID and by (protocol, name) after a restart. A
// name is unique under its protocol and an ID is recorded once; a record
// needs an ID, a protocol and a name, Adapter bytes of at most 4 KiB, and an
// owner for state protection, while an owner of zero is allowed.
func testClients(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cs := s.Clients()
	alice := entity.NewUser()
	n1, n2 := entity.NodeID{15: 1}, entity.NodeID{15: 2}
	nfs := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("host-a"), Boot: [8]byte{1},
		Owner: alice, Protect: true, Recovery: entity.RecoverRefusal, Lease: entity.ClientLease{Node: n1},
		Reach: entity.Reach{Callback: n2, Down: true, Sessions: []entity.NodeID{n1}}, Adapter: []byte{1, 2, 3}}
	must(t, cs.Create(ctx, nfs))
	nfs.Shards = []entity.ShardMark{{Shard: 1, Done: 4}}
	must(t, cs.MarkShard(ctx, nfs.ID, nfs.Shards[0]))
	smb := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolSMB, Name: []byte("host-a"), Recovery: entity.RecoverReconnect}
	must(t, cs.Create(ctx, smb))

	again, err := kvstore.Open(ctx, c)
	must(t, err)
	if got, err := again.Clients().ByName(ctx, entity.ProtocolNFSv4, []byte("host-a")); err != nil || !reflect.DeepEqual(got, nfs) {
		t.Errorf("by name after a restart:\n got %+v, %v\nwant %+v", got, err, nfs)
	}
	if got, err := again.Clients().Get(ctx, smb.ID); err != nil || got.ID != smb.ID || got.Owner != (entity.Principal{}) || got.Recovery != entity.RecoverReconnect {
		t.Errorf("by ID after a restart: %+v, %v", got, err)
	}
	if _, err := cs.ByName(ctx, entity.ProtocolNLM, []byte("host-a")); !merrs.IsNotFoundError(err) {
		t.Errorf("a name under another protocol: %v", err)
	}

	if err := cs.Create(ctx, entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("host-a")}); !isExist(err) {
		t.Errorf("a second client under one name: %v", err)
	}
	if err := cs.Create(ctx, entity.ClientRecord{ID: nfs.ID, Protocol: entity.ProtocolNFSv4, Name: []byte("host-z")}); !isExist(err) {
		t.Errorf("a second record under one ID: %v", err)
	}
	if got, err := cs.Get(ctx, nfs.ID); err != nil || !bytes.Equal(got.Name, nfs.Name) {
		t.Errorf("the record a refused create named: %+v, %v", got, err)
	}

	ok := func(name string) entity.ClientRecord {
		return entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNLM, Name: []byte(name)}
	}
	big := ok("big")
	big.Adapter = make([]byte, 4<<10)
	must(t, cs.Create(ctx, big))
	for name, cl := range map[string]entity.ClientRecord{
		"no ID":                func() entity.ClientRecord { c := ok("x"); c.ID = 0; return c }(),
		"no protocol":          func() entity.ClientRecord { c := ok("x"); c.Protocol = 0; return c }(),
		"no name":              ok(""),
		"adapter over 4 KiB":   func() entity.ClientRecord { c := ok("x"); c.Adapter = make([]byte, 4<<10+1); return c }(),
		"protection, no owner": func() entity.ClientRecord { c := ok("x"); c.Protect = true; return c }(),
	} {
		if err := cs.Create(ctx, cl); !invalid(err) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := cs.ByName(ctx, entity.ProtocolNLM, []byte("x")); !merrs.IsNotFoundError(err) {
		t.Errorf("a refused create wrote its name: %v", err)
	}
}

// RFC 14 §2.1: Put writes only over the record the caller read, and refuses
// ErrRecordChanged with the current record otherwise; a rewrite that changes
// nothing writes nothing, and the ID, protocol, name and Boot are fixed.
func testClientPut(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cs := s.Clients()
	cl := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("h"), Boot: [8]byte{1}}
	must(t, cs.Create(ctx, cl))
	must(t, cs.MarkShard(ctx, cl.ID, entity.ShardMark{Shard: 2, Done: 1}))
	read, err := cs.Get(ctx, cl.ID)
	must(t, err)

	c.Take()
	if _, err := cs.Put(ctx, read, read); err != nil {
		t.Fatal(err)
	}
	if n := c.Take(); n.Sets != 0 {
		t.Errorf("a rewrite that changed nothing wrote %d keys", n.Sets)
	}

	next := read
	next.Ended, next.Reach.Callback, next.Shards = true, entity.NodeID{15: 9}, nil
	got, err := cs.Put(ctx, read, next)
	must(t, err)
	if stored, err := cs.Get(ctx, cl.ID); err != nil || !stored.Ended || stored.Reach.Callback != next.Reach.Callback ||
		len(stored.Shards) != 1 || !reflect.DeepEqual(stored, got) {
		t.Errorf("after a put: %+v, %v; returned %+v", stored, err, got)
	}

	stale := read
	stale.Reach.Down = true
	cur, err := cs.Put(ctx, read, stale)
	if !errors.Is(err, kvstore.ErrRecordChanged) || !cur.Ended {
		t.Errorf("a put over a stale read: %+v, %v", cur, err)
	}
	if stored, _ := cs.Get(ctx, cl.ID); stored.Reach.Down {
		t.Error("a put over a stale read wrote")
	}

	// A put whose prev and next name two live clients is the caller's bug,
	// not a lost race: a retry on ErrRecordChanged would swap the other's.
	other := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("o"), Boot: [8]byte{1}}
	must(t, cs.Create(ctx, other))
	if _, err := cs.Put(ctx, got, other); !invalid(err) {
		t.Errorf("a put naming two clients: %v", err)
	}

	for name, f := range map[string]func(*entity.ClientRecord){
		"name": func(r *entity.ClientRecord) { r.Name = []byte("g") },
		"boot": func(r *entity.ClientRecord) { r.Boot = [8]byte{2} },
	} {
		moved := got
		f(&moved)
		if _, err := cs.Put(ctx, got, moved); !invalid(err) {
			t.Errorf("a put changed the %s: %v", name, err)
		}
	}
}

// RFC 14 §2.1: AddSession records the node's session and writes the new
// Adapter bytes only while the stored bytes are those the caller read, so a
// replay of a setup call, after a restart too, finds the first call's bytes
// and records no second session; a node is recorded once.
func testClientAddSession(t *testing.T, kvo Opener) {
	s, c := newStore(t, kvo)
	cs := s.Clients()
	cl := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("h"), Adapter: []byte("v0")}
	must(t, cs.Create(ctx, cl))
	n1, n2 := entity.NodeID{15: 1}, entity.NodeID{15: 2}
	got, err := cs.AddSession(ctx, cl.ID, n1, []byte("v0"), []byte("first"))
	if err != nil || !bytes.Equal(got.Adapter, []byte("first")) || !slices.Equal(got.Reach.Sessions, []entity.NodeID{n1}) {
		t.Fatalf("the first session: %+v, %v", got, err)
	}

	again, err := kvstore.Open(ctx, c)
	must(t, err)
	cur, err := again.Clients().AddSession(ctx, cl.ID, n2, []byte("v0"), []byte("second"))
	if !errors.Is(err, kvstore.ErrRecordChanged) || !bytes.Equal(cur.Adapter, []byte("first")) {
		t.Errorf("a replay after a restart: %+v, %v", cur, err)
	}
	if got, err := cs.AddSession(ctx, cl.ID, n1, []byte("first"), []byte("third")); err != nil ||
		!slices.Equal(got.Reach.Sessions, []entity.NodeID{n1}) {
		t.Errorf("a node's second session: %+v, %v", got, err)
	}
	if _, err := cs.AddSession(ctx, cl.ID, n2, []byte("third"), make([]byte, 4<<10+1)); !invalid(err) {
		t.Errorf("adapter bytes over 4 KiB: %v", err)
	}
	if got, err := cs.Get(ctx, cl.ID); err != nil || !bytes.Equal(got.Adapter, []byte("third")) || len(got.Reach.Sessions) != 1 {
		t.Errorf("after a replay and a refusal: %+v, %v", got, err)
	}
}

// RFC 14 §4.4 and §5.3: a client's shard marks hold each shard's Done, are
// listed in shard order with the record, and are updated in place; a revoke
// drops every one. Only MarkShard writes one: a create's Shards are not
// written. A shard past 24 bits, or a mark for no client, is refused.
func testClientShards(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cs := s.Clients()
	cl := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("h"),
		Shards: []entity.ShardMark{{Shard: 9, Done: 1}}}
	must(t, cs.Create(ctx, cl))
	must(t, cs.MarkShard(ctx, cl.ID, entity.ShardMark{Shard: 3, Done: 2}))
	must(t, cs.MarkShard(ctx, cl.ID, entity.ShardMark{Shard: entity.ShardLimit - 1, Done: 2}))
	must(t, cs.MarkShard(ctx, cl.ID, entity.ShardMark{Shard: 1, Done: 5}))
	must(t, cs.MarkShard(ctx, cl.ID, entity.ShardMark{Shard: 3, Done: 7}))
	want := []entity.ShardMark{{Shard: 1, Done: 5}, {Shard: 3, Done: 7}, {Shard: entity.ShardLimit - 1, Done: 2}}
	if got, err := cs.Get(ctx, cl.ID); err != nil || !slices.Equal(got.Shards, want) {
		t.Errorf("shards %+v, %v; want %+v", got.Shards, err, want)
	}
	if err := cs.MarkShard(ctx, cl.ID, entity.ShardMark{Shard: entity.ShardLimit}); !invalid(err) {
		t.Errorf("a shard past 24 bits: %v", err)
	}
	if err := cs.MarkShard(ctx, cl.ID+1000, entity.ShardMark{Shard: 1}); !merrs.IsNotFoundError(err) {
		t.Errorf("a mark for no client: %v", err)
	}
	must(t, cs.DropShards(ctx, cl.ID))
	if got, err := cs.Get(ctx, cl.ID); err != nil || len(got.Shards) != 0 {
		t.Errorf("after a revoke: %+v, %v", got, err)
	}

	// ByShard finds the clients with a mark in one shard, and none other.
	other := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNLM, Name: []byte("o")}
	must(t, cs.Create(ctx, other))
	must(t, cs.MarkShard(ctx, other.ID, entity.ShardMark{Shard: 4, Done: 1}))
	must(t, cs.MarkShard(ctx, cl.ID, entity.ShardMark{Shard: 3, Done: 2}))
	for shard, want := range map[entity.ShardID][]entity.ClientID{3: {cl.ID}, 4: {other.ID}, 5: nil} {
		var got []entity.ClientID
		for rec, err := range cs.ByShard(ctx, shard) {
			must(t, err)
			got = append(got, rec.ID)
		}
		if !slices.Equal(got, want) {
			t.Errorf("ByShard(%d) = %v, want %v", shard, got, want)
		}
	}
}

// RFC 14 §2.1: a reboot moves the name to the new ID under a new Boot and
// drops the old record; a delete frees the name.
func testClientReboot(t *testing.T, kvo Opener) {
	s, _ := newStore(t, kvo)
	cs := s.Clients()
	old := entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("h"), Boot: [8]byte{1}}
	must(t, cs.Create(ctx, old))

	for name, f := range map[string]func(*entity.ClientRecord){
		"same boot": func(r *entity.ClientRecord) { r.Boot = old.Boot },
		"same ID":   func(r *entity.ClientRecord) { r.ID = old.ID },
		"new name":  func(r *entity.ClientRecord) { r.Name = []byte("g") },
	} {
		cl := old
		cl.ID, cl.Boot = clientID(t, s), [8]byte{2}
		f(&cl)
		if err := cs.Reboot(ctx, old.ID, cl); !invalid(err) {
			t.Errorf("a reboot with %s: %v", name, err)
		}
	}

	next := old
	next.ID, next.Boot = clientID(t, s), [8]byte{2}
	must(t, cs.Reboot(ctx, old.ID, next))
	if got, err := cs.ByName(ctx, entity.ProtocolNFSv4, []byte("h")); err != nil || got.ID != next.ID || got.Boot != next.Boot {
		t.Errorf("name after the reboot: %+v, %v", got, err)
	}
	if _, err := cs.Get(ctx, old.ID); !merrs.IsNotFoundError(err) {
		t.Errorf("the old record after a reboot: %v", err)
	}

	must(t, cs.Delete(ctx, next.ID))
	if _, err := cs.ByName(ctx, entity.ProtocolNFSv4, []byte("h")); !merrs.IsNotFoundError(err) {
		t.Errorf("name after a delete: %v", err)
	}
	must(t, cs.Create(ctx, entity.ClientRecord{ID: clientID(t, s), Protocol: entity.ProtocolNFSv4, Name: []byte("h")}))
}

// Two clients racing to claim one name: whichever commits second is
// refused, and the name names the first.
func testClientNameRace(t *testing.T, kvo Opener) {
	s, h := heldStore(t, kvo.backends()[0])
	cs := s.Clients()
	// Reserve the ID range first, so the race lands in the record's
	// transaction, not the allocator's.
	a, b := clientID(t, s), clientID(t, s)
	outer, inner := race(h,
		func() error {
			return cs.Create(ctx, entity.ClientRecord{ID: a, Protocol: entity.ProtocolSMB, Name: []byte("g")})
		},
		func() error {
			return cs.Create(ctx, entity.ClientRecord{ID: b, Protocol: entity.ProtocolSMB, Name: []byte("g")})
		})
	must(t, inner)
	if !isExist(outer) {
		t.Fatalf("the second claim: %v", outer)
	}
	if got, err := cs.ByName(ctx, entity.ProtocolSMB, []byte("g")); err != nil || got.ID != b {
		t.Errorf("name names %+v, %v; want %d", got, err, b)
	}
}
