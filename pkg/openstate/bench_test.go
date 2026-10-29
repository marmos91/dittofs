package openstate

import (
	"fmt"
	"runtime"
	"testing"

	"github.com/google/uuid"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv/badgerkv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// BenchmarkHeapPerState reports the heap the table holds per open, per lock
// range and per client, each the growth of the live heap over n of them
// made in one table, divided by n. An open is each on a file of its own, so
// it counts its file's entry; the ranges are one owner's, on one file.
func BenchmarkHeapPerState(b *testing.B) {
	const n = 4096
	for _, c := range []struct {
		unit string
		make func(b *testing.B, tab *Table, who Caller)
	}{
		{"B/open", func(b *testing.B, tab *Table, who Caller) {
			for range n {
				if _, err := tab.Opens().Open(ctx, who, in(1), OpenRequest{Access: rd}); err != nil {
					b.Fatal(err)
				}
			}
		}},
		{"B/range", func(b *testing.B, tab *Table, who Caller) {
			file, lo := in(1), owner(who, "lo")
			for i := range uint64(n) {
				if _, err := tab.Locks().Lock(ctx, who, file, LockReq{Owner: lo, Open: none, Ranges: []LockRange{x(2*i, 1)}}); err != nil {
					b.Fatal(err)
				}
			}
		}},
		{"B/client", func(b *testing.B, tab *Table, _ Caller) {
			for i := range n {
				benchClient(b, tab, fmt.Sprint("c", i))
			}
		}},
	} {
		b.Run(c.unit[2:], func(b *testing.B) {
			var grew int64
			for range b.N {
				tab := benchTable(b)
				who := benchClient(b, tab, "who")
				before := liveHeap()
				c.make(b, tab, who)
				grew += int64(liveHeap()) - int64(before)
				runtime.KeepAlive(tab)
				tab.Stop()
			}
			b.ReportMetric(float64(grew)/float64(b.N*n), c.unit)
		})
	}
}

// benchTable is a started table over a fresh in-memory store, serving shard
// 1, with no cap a benchmark reaches.
func benchTable(b *testing.B) *Table {
	db, err := badgerkv.Open(badgerkv.Options{})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	if err := kvstore.Init(ctx, db, uuid.New()); err != nil {
		b.Fatal(err)
	}
	st, err := kvstore.Open(ctx, db)
	if err != nil {
		b.Fatal(err)
	}
	tab, err := New(Deps{Records: st, Clock: &fakeClock{}, Rand: newSeeded(b.Name())})
	if err != nil {
		b.Fatal(err)
	}
	for k := range tab.caps {
		tab.caps[k] = 1 << 30
	}
	if err := tab.Start(ctx, &recordingSink{}, (&orderLog{}).order); err != nil {
		b.Fatal(err)
	}
	if err := tab.Recovery().ServeShard(ctx, entity.ShareID{}, 1, instances.Add(1)); err != nil {
		b.Fatal(err)
	}
	return tab
}

// benchClient connects an NFSv4 client named name and answers a caller of it.
func benchClient(b *testing.B, tab *Table, name string) Caller {
	c := nfs(name, 1, entity.NewUser())
	p, err := tab.Clients().Propose(ctx, c, addr)
	if err != nil {
		b.Fatal(err)
	}
	id, err := tab.Clients().Connect(ctx, c, &p)
	if err != nil {
		b.Fatal(err)
	}
	return Caller{Client: id, Principal: c.Owner, Protocol: c.Protocol}
}

// liveHeap is the heap still reachable after a collection.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}
