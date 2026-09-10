package storetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// runXattrOpsTests asserts cross-backend parity for the unified xattr resolver
// (pkg/metadata/xattr.go), which presents ONE xattr namespace over the two
// physical backings DittoFS maintains:
//
//   - Inline K/V  — FileAttr.EAs (the SMB EA backing).
//   - Named-stream child entities — "<base>:<stream>" siblings (the SMB ADS
//     backing), enumerated via ListChildren and read through the block store.
//
// Precedence is stream-entity-wins-else-inline. The bare store methods cover
// the inline backing and stream-name enumeration; stream-content reads (which
// require a block store) are exercised through the exported resolver with a
// test StreamContentReader so this metadata-only suite proves the read path
// without a block engine. Pins the same cross-backend parity contract EAOps
// pins for the raw EA map.
func runXattrOpsTests(t *testing.T, factory StoreFactory) {
	t.Run("InlineSetGetDelete", func(t *testing.T) { testXattrInlineSetGetDelete(t, factory) })
	t.Run("ZeroLengthValue", func(t *testing.T) { testXattrZeroLength(t, factory) })
	t.Run("CaseInsensitiveResolution", func(t *testing.T) { testXattrCaseInsensitive(t, factory) })
	t.Run("TooLarge", func(t *testing.T) { testXattrTooLarge(t, factory) })
	t.Run("RemoveMissing", func(t *testing.T) { testXattrRemoveMissing(t, factory) })
	t.Run("InlineList", func(t *testing.T) { testXattrInlineList(t, factory) })
	t.Run("MergedListInlinePlusStream", func(t *testing.T) { testXattrMergedList(t, factory) })
	t.Run("StreamBackedGet", func(t *testing.T) { testXattrStreamBackedGet(t, factory) })
	t.Run("StreamWinsPrecedence", func(t *testing.T) { testXattrStreamPrecedence(t, factory) })
	t.Run("ConcurrentDistinctNames", func(t *testing.T) { testXattrConcurrentDistinctNames(t, factory) })
	t.Run("TransactionIsNotATransactor", func(t *testing.T) { testXattrTransactionIsNotATransactor(t, factory) })
}

// testReader builds a StreamContentReader serving a fixed value for the given
// stream handle, so content-backed resolution can be exercised without a block
// store. It reads attr.Size bytes of the supplied payload.
func testReader(payload []byte) metadata.StreamContentReader {
	return func(_ context.Context, _ metadata.FileHandle, attr *metadata.FileAttr) ([]byte, error) {
		if attr == nil {
			return nil, nil
		}
		out := make([]byte, len(payload))
		copy(out, payload)
		return out, nil
	}
}

// createStreamChild creates a "<base>:<stream>" child of parentHandle with the
// given size/payload so the resolver's colon-prefix scan finds it as a named
// stream of base. Returns the stream child's handle.
func createStreamChild(t *testing.T, store metadata.Store, shareName string, parentHandle metadata.FileHandle, base, stream string, size uint64) metadata.FileHandle {
	t.Helper()
	name := base + ":" + stream
	handle := createTestFile(t, store, shareName, parentHandle, name, 0o600)
	// Stamp size + payload so a stream-content reader can materialise a value.
	file, err := store.GetFile(t.Context(), handle)
	if err != nil {
		t.Fatalf("GetFile(stream child): %v", err)
	}
	file.Size = size
	file.PayloadID = metadata.PayloadID(metadata.BuildPayloadID(shareName, file.Path))
	if err := store.UpdateAttrs(t.Context(), file); err != nil {
		t.Fatalf("UpdateAttrs(stream child): %v", err)
	}
	return handle
}

func testXattrInlineSetGetDelete(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-inline")
	handle := createTestFile(t, store, "/xattr-inline", root, "f.txt", 0o600)

	if err := store.SetXattr(ctx, handle, "color", []byte("blue")); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	val, found, err := store.GetXattr(ctx, handle, "color")
	if err != nil {
		t.Fatalf("GetXattr: %v", err)
	}
	if !found || !bytes.Equal(val, []byte("blue")) {
		t.Fatalf("GetXattr = (%q, %v), want (blue, true)", val, found)
	}

	if err := store.RemoveXattr(ctx, handle, "color"); err != nil {
		t.Fatalf("RemoveXattr: %v", err)
	}
	_, found, err = store.GetXattr(ctx, handle, "color")
	if err != nil {
		t.Fatalf("GetXattr after remove: %v", err)
	}
	if found {
		t.Fatal("xattr still present after RemoveXattr")
	}
}

func testXattrZeroLength(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-zero")
	handle := createTestFile(t, store, "/xattr-zero", root, "f.txt", 0o600)

	if err := store.SetXattr(ctx, handle, "empty", []byte{}); err != nil {
		t.Fatalf("SetXattr(zero): %v", err)
	}
	val, found, err := store.GetXattr(ctx, handle, "empty")
	if err != nil {
		t.Fatalf("GetXattr(zero): %v", err)
	}
	if !found {
		t.Fatal("zero-length xattr must round-trip as present, not absent")
	}
	if len(val) != 0 {
		t.Fatalf("zero-length xattr value = %q, want empty", val)
	}
}

func testXattrCaseInsensitive(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-case")
	handle := createTestFile(t, store, "/xattr-case", root, "f.txt", 0o600)

	if err := store.SetXattr(ctx, handle, "MixedCase", []byte("v")); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	for _, probe := range []string{"MixedCase", "mixedcase", "MIXEDCASE"} {
		val, found, err := store.GetXattr(ctx, handle, probe)
		if err != nil {
			t.Fatalf("GetXattr(%q): %v", probe, err)
		}
		if !found || !bytes.Equal(val, []byte("v")) {
			t.Fatalf("GetXattr(%q) = (%q, %v), want (v, true) — names must resolve case-insensitively", probe, val, found)
		}
	}

	// An upsert under a different casing must not create a duplicate.
	if err := store.SetXattr(ctx, handle, "MIXEDCASE", []byte("v2")); err != nil {
		t.Fatalf("SetXattr(case-diff upsert): %v", err)
	}
	names, err := store.ListXattr(ctx, handle)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	if len(names) != 1 {
		t.Fatalf("case-different upsert created a duplicate: %v", names)
	}
}

func testXattrTooLarge(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-big")
	handle := createTestFile(t, store, "/xattr-big", root, "f.txt", 0o600)

	big := make([]byte, metadata.XattrInlineMaxBytes+1)
	err := store.SetXattr(ctx, handle, "big", big)
	if !errors.Is(err, metadata.ErrXattrTooLarge) {
		t.Fatalf("SetXattr(oversized) err = %v, want ErrXattrTooLarge", err)
	}

	// A value exactly at the limit must succeed (boundary).
	atLimit := make([]byte, metadata.XattrInlineMaxBytes)
	if err := store.SetXattr(ctx, handle, "atlimit", atLimit); err != nil {
		t.Fatalf("SetXattr(at-limit) err = %v, want nil", err)
	}
}

func testXattrRemoveMissing(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-rm")
	handle := createTestFile(t, store, "/xattr-rm", root, "f.txt", 0o600)

	err := store.RemoveXattr(ctx, handle, "absent")
	if !metadata.IsNotFoundError(err) {
		t.Fatalf("RemoveXattr(absent) err = %v, want ErrNotFound", err)
	}
}

func testXattrInlineList(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-list")
	handle := createTestFile(t, store, "/xattr-list", root, "f.txt", 0o600)

	for _, n := range []string{"a", "b", "c"} {
		if err := store.SetXattr(ctx, handle, n, []byte("x")); err != nil {
			t.Fatalf("SetXattr(%q): %v", n, err)
		}
	}
	names, err := store.ListXattr(ctx, handle)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	want := []string{"a", "b", "c"}
	if !equalNames(names, want) {
		t.Fatalf("ListXattr = %v, want %v", names, want)
	}
}

func testXattrMergedList(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-merge")
	handle := createTestFile(t, store, "/xattr-merge", root, "doc.txt", 0o600)

	// Inline xattr on the file.
	if err := store.SetXattr(ctx, handle, "inline", []byte("i")); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	// Manually-created named stream sibling "doc.txt:streamx".
	createStreamChild(t, store, "/xattr-merge", root, "doc.txt", "streamx", 3)

	names, err := store.ListXattr(ctx, handle)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	want := []string{"streamx", "inline"}
	if !equalNames(names, want) {
		t.Fatalf("merged ListXattr = %v, want %v (inline + stream names merged)", names, want)
	}
}

func testXattrStreamBackedGet(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-stream")
	handle := createTestFile(t, store, "/xattr-stream", root, "doc.txt", 0o600)

	content := []byte("STREAMVAL")
	createStreamChild(t, store, "/xattr-stream", root, "doc.txt", "sv", uint64(len(content)))

	// The bare store method has no reader -> stream-only name reports absent.
	_, found, err := store.GetXattr(ctx, handle, "sv")
	if err != nil {
		t.Fatalf("store.GetXattr(stream): %v", err)
	}
	if found {
		t.Fatal("bare store GetXattr surfaced a stream value without a reader")
	}

	// With a reader, the resolver returns the stream's content as the value.
	val, found, err := metadata.ResolveGetXattr(ctx, store, handle, "sv", testReader(content))
	if err != nil {
		t.Fatalf("ResolveGetXattr(stream, reader): %v", err)
	}
	if !found || !bytes.Equal(val, content) {
		t.Fatalf("stream-backed Get = (%q, %v), want (%q, true)", val, found, content)
	}

	// A request whose casing differs from the stored stream name resolves to the
	// same stream: the exact-name lookup misses and the parent scan matches it
	// case-insensitively.
	val, found, err = metadata.ResolveGetXattr(ctx, store, handle, "SV", testReader(content))
	if err != nil {
		t.Fatalf("ResolveGetXattr(stream, mixed case): %v", err)
	}
	if !found || !bytes.Equal(val, content) {
		t.Fatalf("case-insensitive stream Get = (%q, %v), want (%q, true)", val, found, content)
	}
}

func testXattrStreamPrecedence(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-prec")
	handle := createTestFile(t, store, "/xattr-prec", root, "doc.txt", 0o600)

	// Same name in BOTH backings: inline + stream.
	if err := store.SetXattr(ctx, handle, "dup", []byte("INLINE")); err != nil {
		t.Fatalf("SetXattr: %v", err)
	}
	streamContent := []byte("STREAM")
	createStreamChild(t, store, "/xattr-prec", root, "doc.txt", "dup", uint64(len(streamContent)))

	// Stream wins: Get returns the stream content.
	val, found, err := metadata.ResolveGetXattr(ctx, store, handle, "dup", testReader(streamContent))
	if err != nil {
		t.Fatalf("ResolveGetXattr(dup): %v", err)
	}
	if !found || !bytes.Equal(val, streamContent) {
		t.Fatalf("precedence Get = (%q, %v), want stream content %q", val, found, streamContent)
	}

	// List de-dupes the colliding name to a single entry.
	names, err := store.ListXattr(ctx, handle)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	count := 0
	for _, n := range names {
		if n == "dup" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("collision name appears %d times in ListXattr, want 1: %v", count, names)
	}
}

// equalNames compares two name slices order-insensitively.
func equalNames(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// testXattrConcurrentDistinctNames pins that a SetXattr which reported success
// is still readable afterwards, when several writers name *different* xattrs on
// one file at the same time.
//
// The resolver writes the whole EA map: it loads the file, applies the mutation
// to FileAttr.EAs and persists the file. Two writers that both load before
// either persists each hold a map missing the other's name, so the second write
// reinstates the pre-image and silently drops the first — the write returned nil
// and the value is gone.
//
// Asserting on "returned nil implies readable" rather than "all names present"
// keeps the case mechanism-independent: a backend is free to refuse a colliding
// write (sqlite's BUSY, a serialization failure), and the caller can retry that.
// What no backend may do is claim success and lose the value.
//
// The case discriminates on the persistent backends, which lose 13-15 of the 16
// writes when the pair is not atomic. It does not discriminate on the in-memory
// store: its critical sections are short enough that sixteen writers queued on
// one mutex serialise by timing rather than by design, so it survives the
// non-atomic form by luck. The contract is the same for all of them.
func testXattrConcurrentDistinctNames(t *testing.T, factory StoreFactory) {
	store := factory(t)
	ctx := t.Context()
	root := createTestShare(t, store, "/xattr-concurrent")
	handle := createTestFile(t, store, "/xattr-concurrent", root, "f.txt", 0o600)

	const writers = 16
	names := make([]string, writers)
	for i := range names {
		names[i] = fmt.Sprintf("attr%02d", i)
	}

	// One gate released at once, so the loads overlap rather than queueing.
	var gate sync.WaitGroup
	var done sync.WaitGroup
	gate.Add(1)
	errs := make([]error, writers)
	for i := range writers {
		done.Add(1)
		go func(i int) {
			defer done.Done()
			gate.Wait()
			errs[i] = store.SetXattr(ctx, handle, names[i], []byte("v"))
		}(i)
	}
	gate.Done()
	done.Wait()

	present, err := store.ListXattr(ctx, handle)
	if err != nil {
		t.Fatalf("ListXattr: %v", err)
	}
	have := make(map[string]bool, len(present))
	for _, n := range present {
		have[n] = true
	}

	var lost []string
	accepted := 0
	for i, name := range names {
		if errs[i] != nil {
			continue
		}
		accepted++
		if !have[name] {
			lost = append(lost, name)
		}
	}
	if len(lost) > 0 {
		t.Errorf("SetXattr reported success for %d names but %d are absent afterwards: %v\nListXattr returned: %v",
			accepted, len(lost), lost, present)
	}
	if accepted == 0 {
		t.Fatalf("every concurrent SetXattr failed; the case proves nothing about lost updates: %v", errs)
	}
}

// testXattrTransactionIsNotATransactor pins the invariant the xattr write path
// depends on: a Transaction must NOT open transactions of its own.
//
// The resolvers wrap themselves in a transaction by asking whether the target
// is a metadata.Transactor. An in-transaction caller has to fall through that
// check unwrapped, which it does only because no backend's transaction type
// carries WithTransaction. Nothing in the type system enforces that — a
// transaction that grew the method later (savepoints, say) would silently make
// every in-transaction SetXattr nest, and nesting is implementation-defined
// per the Transactor doc, so the symptom would be a deadlock or a discarded
// write rather than a compile error.
func testXattrTransactionIsNotATransactor(t *testing.T, factory StoreFactory) {
	store := factory(t)

	err := store.WithTransaction(t.Context(), func(tx metadata.Transaction) error {
		if _, ok := tx.(metadata.Transactor); ok {
			t.Error("this backend's Transaction implements Transactor: the xattr resolvers " +
				"would open a nested transaction for an in-transaction caller")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithTransaction: %v", err)
	}
}
