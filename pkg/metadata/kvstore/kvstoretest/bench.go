package kvstoretest

import (
	"context"
	"encoding/binary"
	"fmt"
	"github.com/google/uuid"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata/entity"
	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kvstore"
)

// Bench runs the store's benchmarks against KVs from open: the namespace
// targets of RFC 7 §12.4 and the offload commit of RFC 6 §11.3. Each reports
// p50 and p99 latency, since the targets are percentiles, and the records an
// operation writes or reads, since the shape that matters is a cost that does
// not grow with the directory or the file.
//
// Directory and file sizes default to 10² and 10⁴. KVSTORE_BENCH_SIZES takes
// a comma-separated list instead, such as 100,10000,1000000; the larger ones
// take minutes to fill, one create per entry.
func Bench(b *testing.B, open Opener) {
	sizes := benchSizes()
	for _, n := range sizes {
		b.Run(fmt.Sprintf("Lookup/entries=%d", n), func(b *testing.B) { benchLookup(b, open, n) })
	}
	for _, n := range sizes {
		b.Run(fmt.Sprintf("Create/entries=%d", n), func(b *testing.B) { benchCreate(b, open, n) })
	}
	b.Run("CreateOneDirectory/clients=64", func(b *testing.B) { benchParallelCreate(b, open, 1) })
	b.Run("CreateManyDirectories/clients=64", func(b *testing.B) { benchParallelCreate(b, open, 64) })
	for _, n := range sizes {
		b.Run(fmt.Sprintf("ListPage/entries=%d", n), func(b *testing.B) { benchList(b, open, n) })
	}
	b.Run("Getattr", func(b *testing.B) { benchGetattr(b, open) })
	for _, depth := range []int{1, 64} {
		b.Run(fmt.Sprintf("Resolve/depth=%d", depth), func(b *testing.B) { benchResolve(b, open, depth) })
	}
	for _, n := range sizes {
		b.Run(fmt.Sprintf("Rename/entries=%d", n), func(b *testing.B) { benchRename(b, open, n) })
	}
	for _, n := range sizes {
		b.Run(fmt.Sprintf("OffloadCommit/refs=%d", n), func(b *testing.B) { benchOffload(b, open, n) })
	}
}

func benchSizes() []int {
	env := os.Getenv("KVSTORE_BENCH_SIZES")
	if env == "" {
		return []int{100, 10_000}
	}
	var out []int
	for _, f := range strings.Split(env, ",") {
		if n, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && n > 0 {
			out = append(out, n)
		}
	}
	return out
}

// bfs is one share with an administrator, for benchmarks.
type bfs struct {
	s     *kvstore.Store
	c     *kv.Counting
	share entity.Share
	admin entity.Identity
	root  entity.Handle
}

func newBenchFS(b *testing.B, open Opener) *bfs {
	b.Helper()
	c := &kv.Counting{KV: open(b)}
	ctx := context.Background()
	if err := kvstore.Init(ctx, c, uuid.New()); err != nil {
		b.Fatal(err)
	}
	s, err := kvstore.Open(ctx, c)
	if err != nil {
		b.Fatal(err)
	}
	cp := s.ControlPlane()
	sh := entity.Share{ID: entity.NewShareID(), Name: "bench", Path: "/bench", Root: entity.NewFileID()}
	admin := entity.User{Principal: entity.NewUser(), Name: "admin"}
	for _, err := range []error{
		cp.PutShare(ctx, sh), cp.PutUser(ctx, admin),
		cp.SetGrant(ctx, entity.ShareGrant{Share: sh.ID, Principal: admin.Principal, Access: entity.AccessAdmin}),
	} {
		if err != nil {
			b.Fatal(err)
		}
	}
	id := entity.Identity{Principal: admin.Principal, Privileged: true}
	root, _, err := s.Files().Root(ctx, id, sh.ID)
	if err != nil {
		b.Fatal(err)
	}
	return &bfs{s: s, c: c, share: sh, admin: id, root: root}
}

func (f *bfs) mkdir(b *testing.B, parent entity.Handle, name string) entity.Handle {
	h, _, err := f.s.Namespace().Create(context.Background(), f.admin, parent, []byte(name), kvstore.NewFile{Type: entity.Directory, Mode: 0o777})
	if err != nil {
		b.Fatal(err)
	}
	return h
}

// fill creates n regular files in dir, named f0 … f(n-1).
func (f *bfs) fill(b *testing.B, dir entity.Handle, n int) []entity.Handle {
	ctx := context.Background()
	hs := make([]entity.Handle, n)
	for i := range n {
		h, _, err := f.s.Namespace().Create(ctx, f.admin, dir, []byte("f"+strconv.Itoa(i)), kvstore.NewFile{Type: entity.Regular, Mode: 0o644})
		if err != nil {
			b.Fatal(err)
		}
		hs[i] = h
	}
	return hs
}

// latencies records one duration per operation and reports p50 and p99.
type latencies []time.Duration

func (l latencies) report(b *testing.B) {
	if len(l) == 0 {
		return
	}
	s := slices.Clone(l)
	slices.Sort(s)
	q := func(p float64) float64 { return float64(s[int(p*float64(len(s)-1))].Microseconds()) }
	b.ReportMetric(q(0.50), "p50-µs")
	b.ReportMetric(q(0.99), "p99-µs")
}

func timed(l *latencies, fn func()) {
	start := time.Now()
	fn()
	*l = append(*l, time.Since(start))
}

// perOp reports what the timed operations read and wrote, per operation.
func perOp(b *testing.B, c kv.Counts, n int) {
	if n == 0 {
		return
	}
	b.ReportMetric(float64(c.Gets+c.Befores+c.Scanned)/float64(n), "reads/op")
	b.ReportMetric(float64(c.Sets+c.Deletes)/float64(n), "writes/op")
}

func benchLookup(b *testing.B, open Opener, n int) {
	f := newBenchFS(b, open)
	dir := f.mkdir(b, f.root, "d")
	f.fill(b, dir, n)
	ctx := context.Background()
	var l latencies
	f.c.Take()
	b.ResetTimer()
	for i := range b.N {
		timed(&l, func() {
			if _, _, err := f.s.Namespace().Lookup(ctx, f.admin, dir, []byte("f"+strconv.Itoa(i%n))); err != nil {
				b.Fatal(err)
			}
		})
	}
	b.StopTimer()
	l.report(b)
	perOp(b, f.c.Take(), b.N)
}

func benchCreate(b *testing.B, open Opener, n int) {
	f := newBenchFS(b, open)
	dir := f.mkdir(b, f.root, "d")
	f.fill(b, dir, n)
	ctx := context.Background()
	var l latencies
	f.c.Take()
	b.ResetTimer()
	for i := range b.N {
		timed(&l, func() {
			if _, _, err := f.s.Namespace().Create(ctx, f.admin, dir, []byte("new"+strconv.Itoa(i)), kvstore.NewFile{Type: entity.Regular, Mode: 0o644}); err != nil {
				b.Fatal(err)
			}
		})
	}
	b.StopTimer()
	l.report(b)
	perOp(b, f.c.Take(), b.N)
}

// benchParallelCreate runs 64 clients creating at once, into one directory or
// one directory each. Creates in one directory share a guard and do not
// serialise, so the two should be within 20% of each other (RFC 7 §12.2).
func benchParallelCreate(b *testing.B, open Opener, dirs int) {
	f := newBenchFS(b, open)
	ds := make([]entity.Handle, dirs)
	for i := range ds {
		ds[i] = f.mkdir(b, f.root, "d"+strconv.Itoa(i))
	}
	ctx := context.Background()
	var next atomic.Int64
	b.ResetTimer()
	start := time.Now()
	var wg sync.WaitGroup
	for c := range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			dir := ds[c%dirs]
			for {
				i := next.Add(1)
				if i > int64(b.N) {
					return
				}
				if _, _, err := f.s.Namespace().Create(ctx, f.admin, dir, []byte(fmt.Sprintf("c%d-%d", c, i)), kvstore.NewFile{Type: entity.Regular, Mode: 0o644}); err != nil {
					b.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	b.StopTimer()
	b.ReportMetric(float64(b.N)/time.Since(start).Seconds(), "creates/s")
}

func benchList(b *testing.B, open Opener, n int) {
	f := newBenchFS(b, open)
	dir := f.mkdir(b, f.root, "d")
	f.fill(b, dir, n)
	ctx := context.Background()
	var l latencies
	b.ResetTimer()
	for range b.N {
		timed(&l, func() {
			got := 0
			for _, err := range f.s.Namespace().Entries(ctx, f.admin, dir, 0) {
				if err != nil {
					b.Fatal(err)
				}
				if got++; got == 1000 {
					break
				}
			}
		})
	}
	b.StopTimer()
	l.report(b)
}

func benchGetattr(b *testing.B, open Opener) {
	f := newBenchFS(b, open)
	h := f.fill(b, f.root, 1)[0]
	ctx := context.Background()
	var l latencies
	f.c.Take()
	b.ResetTimer()
	for range b.N {
		timed(&l, func() {
			if _, err := f.s.Files().Get(ctx, f.admin, h); err != nil {
				b.Fatal(err)
			}
		})
	}
	b.StopTimer()
	l.report(b)
	perOp(b, f.c.Take(), b.N)
}

// benchResolve resolves a handle at a path depth: the cost must not depend
// on it, since a handle names a file and never a path.
func benchResolve(b *testing.B, open Opener, depth int) {
	f := newBenchFS(b, open)
	dir := f.root
	for i := range depth {
		dir = f.mkdir(b, dir, "d"+strconv.Itoa(i))
	}
	h := f.fill(b, dir, 1)[0]
	ctx := context.Background()
	var l latencies
	f.c.Take()
	b.ResetTimer()
	for range b.N {
		timed(&l, func() {
			if _, err := f.s.Files().Resolve(ctx, f.share.ID, h); err != nil {
				b.Fatal(err)
			}
		})
	}
	b.StopTimer()
	l.report(b)
	perOp(b, f.c.Take(), b.N)
}

// benchRename moves one file back and forth between two directories of n
// entries each.
func benchRename(b *testing.B, open Opener, n int) {
	f := newBenchFS(b, open)
	a, c := f.mkdir(b, f.root, "a"), f.mkdir(b, f.root, "c")
	f.fill(b, a, n)
	f.fill(b, c, n)
	if _, _, err := f.s.Namespace().Create(context.Background(), f.admin, a, []byte("mover"), kvstore.NewFile{Type: entity.Regular, Mode: 0o644}); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	var l latencies
	b.ResetTimer()
	for i := range b.N {
		from, to := a, c
		if i%2 == 1 {
			from, to = c, a
		}
		timed(&l, func() {
			if _, err := f.s.Namespace().Rename(ctx, f.admin, from, []byte("mover"), to, []byte("mover")); err != nil {
				b.Fatal(err)
			}
		})
	}
	b.StopTimer()
	l.report(b)
}

// benchOffload commits blocks of 64 one-MiB chunks into a file that already
// holds n refs, each block replacing 64 of them: the records a commit writes
// must stay constant as the file grows (RFC 6 §11.3).
func benchOffload(b *testing.B, open Opener, n int) {
	f := newBenchFS(b, open)
	h := f.fill(b, f.root, 1)[0]
	_, file, _ := h.Parse()
	ctx := context.Background()
	nsID := shareNamespace(b, f)
	const per = 64
	const mib = 1 << 20
	seq := uint64(0)
	commit := func(round int, base int64) {
		seq++
		var name entity.BlockName
		binary.BigEndian.PutUint64(name[24:], seq)
		chunks := make([]kvstore.ChunkAt, per)
		refs := make([]entity.ChunkRef, per)
		for i := range per {
			var hash entity.ChunkHash
			binary.BigEndian.PutUint64(hash[24:], seq*per+uint64(i))
			chunks[i] = kvstore.ChunkAt{Hash: hash, Position: int64(i) * mib, Length: mib}
			v := entity.VersionOf(uint64(round) + 1)
			refs[i] = entity.ChunkRef{Offset: base + int64(i)*mib, Length: mib, Chunk: hash, Oldest: v, Newest: v}
		}
		if err := f.s.Content().Intend(ctx, nsID, name, kvstore.Intent{Domain: kvstore.DomainShard}); err != nil {
			b.Fatal(err)
		}
		if _, err := f.s.Content().Commit(ctx, kvstore.BlockCommit{Namespace: nsID, Name: name, Size: per * mib, Chunks: chunks,
			Files: []kvstore.FileCommit{{Share: f.share.ID, File: file, Refs: refs}}}); err != nil {
			b.Fatal(err)
		}
	}
	for i := 0; i < n; i += per {
		commit(0, int64(i)*mib)
	}
	var l latencies
	f.c.Take()
	b.ResetTimer()
	for i := range b.N {
		base := int64((i*per)%max(n, per)) * mib
		timed(&l, func() { commit(i+1, base) })
	}
	b.StopTimer()
	l.report(b)
	perOp(b, f.c.Take(), b.N)
}

// shareNamespace is the namespace the bench share counts its content in.
func shareNamespace(b *testing.B, f *bfs) entity.NamespaceID {
	for sh, err := range f.s.ControlPlane().Shares(context.Background()) {
		if err != nil {
			b.Fatal(err)
		}
		if sh.ID == f.share.ID && len(sh.Namespaces) > 0 {
			return sh.Namespaces[0].Namespace
		}
	}
	b.Fatal("the bench share has no namespace")
	return entity.NamespaceID{}
}
