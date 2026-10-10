package engine

import (
	"bytes"
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/block/local"
	remotememory "github.com/marmos91/dittofs/pkg/block/remote/memory"
	metadatabadger "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

const residentChunkSize = 1 << 20
const residentPayload = "resident-prefetch"

type residencyRemote struct {
	*remotememory.Store
	gets       atomic.Int64
	downloaded atomic.Int64
	latency    time.Duration
}

func (r *residencyRemote) ReadChunk(ctx context.Context, id string, off, length int64, hash block.ContentHash) ([]byte, error) {
	r.gets.Add(1)
	if r.latency > 0 {
		timer := time.NewTimer(r.latency)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	data, err := r.Store.ReadChunk(ctx, id, off, length, hash)
	r.downloaded.Add(int64(len(data)))
	return data, err
}

type residencyMetadata struct {
	*metadatabadger.BadgerMetadataStore
	lookups atomic.Int64
}

func (s *residencyMetadata) GetFileChunkAtOffset(ctx context.Context, pid string, off uint64) (*block.FileChunk, error) {
	s.lookups.Add(1)
	return s.BadgerMetadataStore.GetFileChunkAtOffset(ctx, pid, off)
}
func (s *residencyMetadata) ListFileChunks(ctx context.Context, pid string) ([]*block.FileChunk, error) {
	s.lookups.Add(1)
	return s.BadgerMetadataStore.ListFileChunks(ctx, pid)
}

type residencyFixture struct {
	j      *journal.Store
	md     *residencyMetadata
	remote *residencyRemote
	syncer *RemoteSync
	bs     *Store
}

func newResidencyFixture(t testing.TB, queued bool) *residencyFixture {
	t.Helper()
	ctx := context.Background()
	md, err := metadatabadger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = md.Close() })
	j, err := journal.Open(t.TempDir(), journal.Config{MaxLocalBytes: 256 << 20, MaxLogBytes: 512 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = j.Close() })
	f := &residencyFixture{j: j, md: &residencyMetadata{BadgerMetadataStore: md}, remote: &residencyRemote{Store: remotememory.New()}}
	f.syncer = newFetchSyncer(j, f.remote, f.md, f.md)
	f.syncer.hasRemote.Store(true)
	if queued {
		f.syncer.queue = NewSyncQueue(f.syncer, DefaultSyncQueueConfig())
		f.syncer.queue.Start(ctx)
		t.Cleanup(func() { f.syncer.queue.Stop(5 * time.Second) })
	}
	f.bs = &Store{local: j, remote: f.remote, syncer: f.syncer}
	return f
}

func (f *residencyFixture) seed(t testing.TB, off int64, data []byte, warm bool) {
	t.Helper()
	seedSyncedRemoteChunk(t, f.md, f.remote, f.md, residentPayload, uint64(off), data)
	if warm {
		if err := f.j.Hydrate(context.Background(), journal.FileID(residentPayload), off, data, f.j.WriteVersion()); err != nil {
			t.Fatal(err)
		}
	} else {
		if err := f.j.SeedCold(context.Background(), journal.FileID(residentPayload), [][2]int64{{off, int64(len(data))}}); err != nil {
			t.Fatal(err)
		}
	}
}
func (f *residencyFixture) seedFile(t testing.TB, chunks int) {
	t.Helper()
	for i := 0; i < chunks; i++ {
		f.seed(t, int64(i*residentChunkSize), bytes.Repeat([]byte{byte(i + 1)}, residentChunkSize), true)
	}
}
func (f *residencyFixture) drain(t testing.TB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.syncer.queue.Pending() != 0 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Microsecond)
	}
	if n := f.syncer.queue.Pending(); n != 0 {
		t.Fatalf("prefetch pending=%d", n)
	}
}
func (f *residencyFixture) read(t testing.TB, off int64, want []byte) {
	t.Helper()
	got := make([]byte, len(want))
	n, err := f.bs.ReadAt(context.Background(), residentPayload, got, uint64(off))
	if err != nil || n != len(want) || !bytes.Equal(got, want) {
		t.Fatalf("ReadAt(%d): n=%d err=%v bytes_equal=%v", off, n, err, bytes.Equal(got, want))
	}
}

func TestWarmSequentialReadSkipsResidentPrefetch(t *testing.T) {
	f := newResidencyFixture(t, true)
	const chunks = 32
	f.seedFile(t, chunks)
	buf := make([]byte, residentChunkSize)
	ctx := context.Background()
	for i := 0; i < chunks; i++ {
		n, st, err := f.j.ReadAt(ctx, journal.FileID(residentPayload), int64(i*residentChunkSize), buf)
		if err != nil || n != len(buf) || st.Cold || st.Hole {
			t.Fatalf("fixture is not warm: n=%d state=%+v err=%v", n, st, err)
		}
	}
	for pass := 1; pass <= 3; pass++ {
		for i := 0; i < chunks; i++ {
			f.read(t, int64(i*residentChunkSize), bytes.Repeat([]byte{byte(i + 1)}, residentChunkSize))
		}
		f.drain(t)
		if gets, downloaded := f.remote.gets.Load(), f.remote.downloaded.Load(); gets != 0 || downloaded != 0 {
			t.Fatalf("warm pass %d: remote GETs=%d bytes=%d, want zero", pass, gets, downloaded)
		}
	}
}

func TestResidentPrefetchSkipsManifest(t *testing.T) {
	f := newResidencyFixture(t, false)
	f.seedFile(t, 8)
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); err != nil {
		t.Fatal(err)
	}
	if n := f.md.lookups.Load(); n != 0 {
		t.Fatalf("fully warm block performed %d manifest lookups, want zero", n)
	}
	if n := f.remote.gets.Load(); n != 0 {
		t.Fatalf("fully warm block performed %d remote GETs", n)
	}
}

func TestResidentPrefetchMixedBlock(t *testing.T) {
	f := newResidencyFixture(t, false)
	for i := 0; i < 8; i++ {
		f.seed(t, int64(i*residentChunkSize), bytes.Repeat([]byte{byte(i + 1)}, residentChunkSize), i != 4)
	}
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); err != nil {
		t.Fatal(err)
	}
	if n := f.remote.gets.Load(); n != 1 {
		t.Fatalf("mixed block remote GETs=%d, want one", n)
	}
	for i := 0; i < 8; i++ {
		f.read(t, int64(i*residentChunkSize), bytes.Repeat([]byte{byte(i + 1)}, residentChunkSize))
	}
}

func TestResidentPrefetchPartialChunkKeepsDirtyBytes(t *testing.T) {
	f := newResidencyFixture(t, false)
	original := bytes.Repeat([]byte{0x31}, residentChunkSize)
	f.seed(t, 0, original, false)
	dirty := bytes.Repeat([]byte{0xA7}, residentChunkSize/2)
	if err := f.j.WriteAt(context.Background(), journal.FileID(residentPayload), 0, dirty); err != nil {
		t.Fatal(err)
	}
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); err != nil {
		t.Fatal(err)
	}
	if n := f.remote.gets.Load(); n != 1 {
		t.Fatalf("partial chunk GETs=%d, want one", n)
	}
	copy(original, dirty)
	f.read(t, 0, original)
}

func TestResidentPrefetchSparsePartialFinalBlock(t *testing.T) {
	f := newResidencyFixture(t, false)
	warm := bytes.Repeat([]byte{0x51}, residentChunkSize/2)
	cold := bytes.Repeat([]byte{0xC2}, residentChunkSize/2)
	f.seed(t, residentChunkSize, warm, true)
	f.seed(t, 3*residentChunkSize, cold, false)
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); err != nil {
		t.Fatal(err)
	}
	if n := f.remote.gets.Load(); n != 1 {
		t.Fatalf("sparse block GETs=%d, want one", n)
	}
	want := make([]byte, 4*residentChunkSize)
	copy(want[residentChunkSize:], warm)
	copy(want[3*residentChunkSize:], cold)
	f.read(t, 0, want)
	before := f.remote.gets.Load()
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); err != nil {
		t.Fatal(err)
	}
	if n := f.remote.gets.Load() - before; n != 0 {
		t.Fatalf("warm sparse claims GETs=%d, want zero", n)
	}
}

func TestResidentPrefetchOverlappingClaims(t *testing.T) {
	f := newResidencyFixture(t, false)
	old := bytes.Repeat([]byte{0x31}, 4*residentChunkSize)
	newer := bytes.Repeat([]byte{0xA7}, residentChunkSize)
	f.seed(t, residentChunkSize, old, false)
	f.seed(t, 2*residentChunkSize, newer, true)
	// Only the old row's exposed claims are cold; the later row must not be
	// fetched merely because the older row's full extent includes them.
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), old...)
	copy(want[residentChunkSize:], newer)
	f.read(t, residentChunkSize, want)
	if n := f.remote.gets.Load(); n != 2 {
		t.Fatalf("old row's two disjoint claims GETs=%d, want two", n)
	}
}

func TestResidentPrefetchStraddlingChunk(t *testing.T) {
	f := newResidencyFixture(t, false)
	original := bytes.Repeat([]byte{0x31}, 2*residentChunkSize)
	f.seed(t, 7*residentChunkSize, original, false)
	dirty := bytes.Repeat([]byte{0xA7}, residentChunkSize)
	if err := f.j.WriteAt(context.Background(), journal.FileID(residentPayload), 8*residentChunkSize, dirty); err != nil {
		t.Fatal(err)
	}
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 1); err != nil {
		t.Fatal(err)
	}
	if n := f.remote.gets.Load(); n != 1 {
		t.Fatalf("straddling chunk GETs=%d, want one", n)
	}
	copy(original[residentChunkSize:], dirty)
	f.read(t, 7*residentChunkSize, original)
}

// Evict after the snapshot but before its caller acts on it. This represents
// the legal interleaving where speculative prefetch skips a range that a later
// demand read must fetch after all.
type evictAfterResidency struct {
	local.LocalStore
	once atomic.Bool
}

func (l *evictAfterResidency) IsRangeResident(ctx context.Context, id journal.FileID, off, n int64) (bool, error) {
	resident, err := l.LocalStore.IsRangeResident(ctx, id, off, n)
	if resident && l.once.CompareAndSwap(false, true) {
		result, evictErr := l.Evict(ctx, math.MaxInt64)
		if evictErr != nil {
			return false, evictErr
		}
		if result.SegmentsEvicted == 0 {
			return false, errors.New("fixture evicted no segment")
		}
	}
	return resident, err
}
func TestResidentPrefetchEvictionFallsBackToDemand(t *testing.T) {
	f := newResidencyFixture(t, false)
	f.seedFile(t, 8)
	f.syncer.local = &evictAfterResidency{LocalStore: f.j}
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); err != nil {
		t.Fatal(err)
	}
	if n := f.remote.gets.Load(); n != 0 {
		t.Fatalf("prefetch GETs=%d, want zero", n)
	}
	f.read(t, 0, bytes.Repeat([]byte{1}, residentChunkSize))
	if n := f.remote.gets.Load(); n != 1 {
		t.Fatalf("demand GETs=%d, want one", n)
	}
}

type failedResidency struct {
	local.LocalStore
	err error
}

func (l failedResidency) IsRangeResident(context.Context, journal.FileID, int64, int64) (bool, error) {
	return false, l.err
}
func TestResidentPrefetchProbeFailure(t *testing.T) {
	f := newResidencyFixture(t, false)
	boom := errors.New("residency unavailable")
	f.syncer.local = failedResidency{LocalStore: f.j, err: boom}
	if err := f.syncer.fetchBlock(context.Background(), residentPayload, 0); !errors.Is(err, boom) {
		t.Fatalf("fetch error=%v, want %v", err, boom)
	}
	if f.md.lookups.Load() != 0 || f.remote.gets.Load() != 0 {
		t.Fatal("failed probe started a fetch")
	}
}

// Measure the complete read plus its speculative work, using a real journal
// and Badger manifest. Preparation and remote-only demotion are untimed.
func BenchmarkSequentialReadResidency(b *testing.B) {
	silenceLoggerForBench(b)
	for _, mode := range []string{"warm", "cold", "mixed"} {
		b.Run(mode, func(b *testing.B) {
			f := newResidencyFixture(b, true)
			const chunks = 32
			f.seedFile(b, chunks)
			f.remote.latency = time.Millisecond
			ctx := context.Background()
			buf := make([]byte, residentChunkSize)
			b.SetBytes(chunks * residentChunkSize)
			b.ReportAllocs()
			b.ResetTimer()
			var gets, downloaded int64
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				switch mode {
				case "cold":
					if err := f.j.Invalidate(ctx, journal.FileID(residentPayload), 0, chunks*residentChunkSize); err != nil {
						b.Fatal(err)
					}
				case "mixed":
					for chunk := 0; chunk < chunks; chunk += 2 {
						if err := f.j.Invalidate(ctx, journal.FileID(residentPayload), int64(chunk*residentChunkSize), residentChunkSize); err != nil {
							b.Fatal(err)
						}
					}
				}
				beforeGets, beforeBytes := f.remote.gets.Load(), f.remote.downloaded.Load()
				b.StartTimer()
				for chunk := 0; chunk < chunks; chunk++ {
					n, err := f.bs.ReadAt(ctx, residentPayload, buf, uint64(chunk*residentChunkSize))
					if err != nil || n != len(buf) {
						b.Fatalf("read %d: n=%d err=%v", chunk, n, err)
					}
					if buf[0] != byte(chunk+1) || buf[len(buf)-1] != byte(chunk+1) {
						b.Fatal("incorrect read bytes")
					}
				}
				f.drain(b)
				gets += f.remote.gets.Load() - beforeGets
				downloaded += f.remote.downloaded.Load() - beforeBytes
			}
			b.StopTimer()
			b.ReportMetric(float64(gets)/float64(b.N), "GETs/op")
			b.ReportMetric(float64(downloaded)/float64(b.N), "remote-B/op")
		})
	}
}
