# Trash / Recycle Bin Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship a per-share, opt-in `#recycle` bin that moves deleted files into a real, browsable directory instead of destroying them, with retention/quota/exclude controls, restore, and `dfsctl` management — across NFS and SMB, on macOS/Linux/Windows clients.

**Architecture:** The recycle trap lives inside `MetadataService.RemoveFile`/`RemoveDirectory`/`Move`, gated by an injected `metadata.TrashPolicy` that reads per-share config through a **locked accessor** on the shares service (no shared-pointer data race). When recycling, those methods `Move` the node into `#recycle/<original-path>`, stamp deletion metadata, and return `PayloadID=""` so every protocol adapter automatically **skips** its `blockStore.Delete` — block deletion is deferred for free. A `trash.Service` in the runtime owns restore/empty/accounting and a reaper goroutine (retention + max-size) wired into the lifecycle Start/Stop. Disabling trash auto-empties the bin.

**Tech Stack:** Go; Cobra (`dfsctl`); chi (REST); GORM + golang-migrate (postgres); badger; in-memory store; `pkg/metadata/storetest` cross-backend conformance; `test/e2e` framework (NFS/SMB mount harness).

**Spec:** `docs/superpowers/specs/2026-06-01-trash-recycle-bin-design.md`
**Branch:** `feat/190-trash-recycle-bin` (already created off `develop`).

**Conventions for every task:** Sign commits (`git -c gpg.format=ssh commit -S`; if "Couldn't get agent socket", first `export SSH_AUTH_SOCK="/Users/marmos91/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"`). Never mention AI in commits. Concise messages. Re-`git add` and verify `git diff --cached --stat` before committing. Run `gofmt -s -w . && go vet ./... && golangci-lint run --timeout=5m` before each commit that touches Go.

---

## Reserved-name constant (used throughout)

The bin directory name is the Synology convention `#recycle`. Define once and import everywhere:

- `pkg/metadata/trash.go` will export `const RecycleDirName = "#recycle"`.

---

## Phase 1 — Metadata layer: fields, migration, policy, recycle, conformance

### Task 1: Add deletion-metadata fields to `FileAttr` + postgres migration

**Files:**
- Modify: `pkg/metadata/file_types.go` (struct `FileAttr`, ends ~line 105)
- Create: `pkg/metadata/store/postgres/migrations/000027_file_trash_metadata.up.sql`
- Create: `pkg/metadata/store/postgres/migrations/000027_file_trash_metadata.down.sql`
- Modify: `pkg/metadata/store/postgres/files.go` (the files INSERT/UPDATE column map + the row scan)

- [ ] **Step 1: Add the three fields to `FileAttr`** (after `ObjectID`, before the closing brace)

```go
	// DeletedAt is set when this node was recycled (moved into #recycle).
	// nil means the node is live. Drives retention reaping and trash listing.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`

	// OriginalPath is the share-relative path the node occupied before being
	// recycled. Used as the default restore destination. Empty for live nodes.
	OriginalPath string `json:"original_path,omitempty"`

	// DeletedBy is the principal (AuthContext Identity.Username, or its UID as
	// a string when no username is known) that recycled the node. Display only.
	DeletedBy string `json:"deleted_by,omitempty"`
```

- [ ] **Step 2: Write the postgres up migration** (`000027_file_trash_metadata.up.sql`)

```sql
-- Per-file recycle-bin metadata (#190 trash / recycle bin).
--
-- When a share has trash enabled, an unlink moves the node into the share's
-- #recycle directory instead of deleting it. Three nullable columns record
-- the recycle event on the moved node's root so the reaper and `dfsctl trash
-- list` can enumerate bin entries without a side table, and restore knows
-- where the file came from:
--   deleted_at    -- recycle timestamp; NULL for live nodes; drives retention
--   original_path -- share-relative path before recycling; default restore dest
--   deleted_by    -- principal that recycled the node (display only)
--
-- The memory backend carries these via the in-process struct and badger via
-- JSON; only postgres needs explicit columns. Pre-existing rows default to
-- NULL/'' which the code treats as "live node".

ALTER TABLE files
    ADD COLUMN IF NOT EXISTS deleted_at    TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS original_path TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS deleted_by    TEXT NOT NULL DEFAULT '';
```

- [ ] **Step 3: Write the postgres down migration** (`000027_file_trash_metadata.down.sql`)

```sql
-- Revert: drop the per-file recycle-bin metadata columns (#190 trash).

ALTER TABLE files
    DROP COLUMN IF EXISTS deleted_at,
    DROP COLUMN IF EXISTS original_path,
    DROP COLUMN IF EXISTS deleted_by;
```

- [ ] **Step 4: Wire the columns into the postgres files INSERT/UPDATE + scan.** In `pkg/metadata/store/postgres/files.go`, find the column/value map used to write a file row (mirrors the GORM/SQL pattern already there for `mode`, `size`, `content_id`, etc.) and add `deleted_at`, `original_path`, `deleted_by` writing `attr.DeletedAt`, `attr.OriginalPath`, `attr.DeletedBy`. In the row-scan that builds a `FileAttr`, scan the three columns back (use a `sql.NullTime` for `deleted_at` → assign `&t` when valid, else nil). Match the existing nullable-time handling in that file.

- [ ] **Step 5: Build + vet**

Run: `go build ./... && go vet ./pkg/metadata/...`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add pkg/metadata/file_types.go pkg/metadata/store/postgres/migrations/000027_file_trash_metadata.up.sql pkg/metadata/store/postgres/migrations/000027_file_trash_metadata.down.sql pkg/metadata/store/postgres/files.go
git -c gpg.format=ssh commit -S -m "feat(metadata): add recycle-bin fields to FileAttr + postgres migration 000027 (#190)"
```

---

### Task 2: Define `TrashPolicy` + reserved name, inject into `MetadataService`

**Files:**
- Create: `pkg/metadata/trash.go`
- Modify: `pkg/metadata/service.go` (the `MetadataService` struct + constructor — locate the struct definition; it is the receiver of `RemoveFile`/`Move`)
- Test: `pkg/metadata/trash_test.go`

- [ ] **Step 1: Write the failing test** (`pkg/metadata/trash_test.go`)

```go
package metadata

import "testing"

func TestExcludedByPatterns(t *testing.T) {
	pol := TrashConfig{ExcludePatterns: []string{"*.tmp", "~$*"}}
	cases := map[string]bool{
		"report.tmp": true,
		"~$doc.docx": true,
		"keep.txt":   false,
		"#recycle":   false,
	}
	for name, want := range cases {
		if got := pol.Excluded(name); got != want {
			t.Errorf("Excluded(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestRecycleDirNameIsReserved(t *testing.T) {
	if RecycleDirName != "#recycle" {
		t.Fatalf("RecycleDirName = %q, want #recycle", RecycleDirName)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./pkg/metadata/ -run 'TestExcluded|TestRecycleDirName'`
Expected: FAIL — undefined `TrashConfig`, `RecycleDirName`.

- [ ] **Step 3: Implement `pkg/metadata/trash.go`**

```go
package metadata

import "path"

// RecycleDirName is the reserved per-share recycle-bin directory (Synology
// convention). Created lazily at the share root when trash is enabled.
const RecycleDirName = "#recycle"

// TrashConfig is a per-share recycle-bin policy snapshot, returned by a
// TrashPolicy under lock so callers never read a shared, mutating pointer.
type TrashConfig struct {
	Enabled         bool
	ExcludePatterns []string
}

// Excluded reports whether a base name matches any exclude glob and should
// therefore bypass the bin (immediate delete).
func (c TrashConfig) Excluded(name string) bool {
	for _, pat := range c.ExcludePatterns {
		if ok, err := path.Match(pat, name); err == nil && ok {
			return true
		}
	}
	return false
}

// TrashPolicy yields the recycle-bin config for the share owning a handle.
// Implemented by the runtime shares service with a locked accessor; nil on a
// MetadataService means trash is globally disabled (delete behaves as before).
type TrashPolicy interface {
	// TrashConfigForShare returns the policy for the named share. ok=false
	// when the share is unknown.
	TrashConfigForShare(shareName string) (cfg TrashConfig, ok bool)
}
```

- [ ] **Step 4: Inject the policy into `MetadataService`.** In `pkg/metadata/service.go`, add a field `trashPolicy TrashPolicy` to the `MetadataService` struct and a setter:

```go
// SetTrashPolicy installs the per-share recycle-bin policy. A nil policy
// (the default) disables trash: deletes destroy content as before.
func (s *MetadataService) SetTrashPolicy(p TrashPolicy) { s.trashPolicy = p }
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./pkg/metadata/ -run 'TestExcluded|TestRecycleDirName'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/metadata/trash.go pkg/metadata/trash_test.go pkg/metadata/service.go
git -c gpg.format=ssh commit -S -m "feat(metadata): TrashPolicy interface + reserved #recycle name (#190)"
```

---

### Task 3: Recycle on unlink inside `RemoveFile` / `RemoveDirectory`

**Files:**
- Create: `pkg/metadata/file_recycle.go` (helper `recycleNode`)
- Modify: `pkg/metadata/file_remove.go` (`RemoveFile`, branch near the top before the delete transaction)
- Modify: `pkg/metadata/directory.go` (`RemoveDirectory`, same branch)
- Test: extend conformance in Task 5 (the unit seam is exercised there with a real store)

- [ ] **Step 1: Implement the shared recycle helper** (`pkg/metadata/file_recycle.go`)

```go
package metadata

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// inRecycle reports whether a share-relative path is the recycle dir itself
// or lives underneath it. Deletes inside the bin are always permanent, and
// the bin can never be recycled into itself.
func inRecycle(relPath string) bool {
	relPath = strings.TrimPrefix(relPath, "/")
	return relPath == RecycleDirName || strings.HasPrefix(relPath, RecycleDirName+"/")
}

// recycleNode moves the node named `name` under `parentHandle` (whose
// share-relative path is `parentRel`) into the share's #recycle directory,
// preserving its original-path subtree, and stamps DeletedAt/OriginalPath/
// DeletedBy on the moved root. It returns the recycled node's pre-move File
// so callers can build a WCC/return value with PayloadID cleared (no block
// deletion). On any failure it returns an error WITHOUT having destroyed the
// node — the caller must surface it, never silently hard-delete.
//
// origRel is the node's own share-relative path (parentRel + "/" + name,
// normalized). The bin destination mirrors that path under #recycle, with a
// unix-timestamp suffix on the final component if the destination is taken.
func (s *MetadataService) recycleNode(
	ctx *AuthContext,
	shareName string,
	parentHandle FileHandle,
	name string,
	origRel string,
) (*File, error) {
	rootHandle, err := s.shareRootHandle(ctx, shareName) // see Step 2
	if err != nil {
		return nil, fmt.Errorf("recycle: resolve share root: %w", err)
	}

	binRoot, err := s.ensureChildDir(ctx, rootHandle, RecycleDirName) // see Step 3
	if err != nil {
		return nil, fmt.Errorf("recycle: ensure %s: %w", RecycleDirName, err)
	}

	// Recreate the original parent subtree under the bin: for orig path
	// a/b/c, ensure #recycle/a/b exists, then move into it as "c".
	relDir := path.Dir(origRel) // "a/b" or "." for top-level
	destParent := binRoot
	if relDir != "." && relDir != "/" && relDir != "" {
		for _, seg := range strings.Split(strings.Trim(relDir, "/"), "/") {
			destParent, err = s.ensureChildDir(ctx, destParent, seg)
			if err != nil {
				return nil, fmt.Errorf("recycle: ensure bin subtree %q: %w", seg, err)
			}
		}
	}

	now := time.Now().UTC()
	destName := name
	if _, lookupErr := s.lookupChild(ctx, destParent, destName); lookupErr == nil {
		// Collision in bin → Synology-style timestamp suffix.
		destName = fmt.Sprintf("%s (%s)", name, strconv.FormatInt(now.Unix(), 10))
	}

	// Grab the node's pre-move File for the return value.
	preFile, err := s.lookupChild(ctx, parentHandle, name)
	if err != nil {
		return nil, fmt.Errorf("recycle: lookup victim: %w", err)
	}

	if err := s.Move(ctx, parentHandle, name, destParent, destName); err != nil {
		return nil, fmt.Errorf("recycle: move into bin: %w", err)
	}

	// Stamp deletion metadata on the moved root node.
	if err := s.stampRecycleMeta(ctx, destParent, destName, &now, origRel, principalOf(ctx)); err != nil {
		return nil, fmt.Errorf("recycle: stamp metadata: %w", err)
	}

	ret := *preFile
	ret.PayloadID = "" // signal callers/adapters to skip block deletion
	return &ret, nil
}

// principalOf extracts a display principal from the auth context.
func principalOf(ctx *AuthContext) string {
	if ctx != nil && ctx.Identity != nil {
		if ctx.Identity.Username != "" {
			return ctx.Identity.Username
		}
		if ctx.Identity.UID != nil {
			return strconv.FormatUint(uint64(*ctx.Identity.UID), 10)
		}
	}
	return ""
}
```

- [ ] **Step 2: Add the helpers `recycleNode` depends on.** In `file_recycle.go` add small wrappers over existing store/service primitives (use the real method names already present on `MetadataService` / `Transaction` — grep `func (s *MetadataService)` and the `Transaction` interface; the names below are the intended seams, map them to the actual store calls discovered while implementing):

```go
// shareRootHandle returns the root FileHandle for a share. Implement via the
// existing share-root resolution the service already uses (grep for how
// RemoveDirectory/Move obtain a share root or how handles encode share id).
func (s *MetadataService) shareRootHandle(ctx *AuthContext, shareName string) (FileHandle, error)

// ensureChildDir returns the handle of child dir `name` under `parent`,
// creating it (mode 0700) if absent. Reuse the service's existing Mkdir /
// CreateDirectory path; treat ErrExist as success and re-lookup.
func (s *MetadataService) ensureChildDir(ctx *AuthContext, parent FileHandle, name string) (FileHandle, error)

// lookupChild returns the File for `name` under `parent`, or ErrNoEntity.
// Reuse the existing Lookup primitive.
func (s *MetadataService) lookupChild(ctx *AuthContext, parent FileHandle, name string) (*File, error)

// stampRecycleMeta sets DeletedAt/OriginalPath/DeletedBy on the named child
// using the existing SetAttr/PutFile write path (load FileAttr, set the three
// fields, persist).
func (s *MetadataService) stampRecycleMeta(ctx *AuthContext, parent FileHandle, name string, deletedAt *time.Time, origPath, deletedBy string) error
```

> Implementer note: these four are thin adapters over methods that already exist on the service/store. Find the real names (`grep -n "func (s \*MetadataService)" pkg/metadata/*.go` and the `Transaction`/`MetadataStore` interfaces in `pkg/metadata/store.go`) and implement each as 3–8 lines. Do not invent new store interface methods unless genuinely absent.

- [ ] **Step 3: Branch into recycle from `RemoveFile`.** In `pkg/metadata/file_remove.go`, at the very top of `RemoveFile` (after input validation, before the delete transaction), add:

```go
	if s.trashPolicy != nil {
		shareName := shareNameOf(parentHandle) // existing helper to read share from a handle
		if cfg, ok := s.trashPolicy.TrashConfigForShare(shareName); ok && cfg.Enabled {
			origRel := joinRel(parentRelPath(parentHandle), name) // share-relative path of the victim
			if !inRecycle(origRel) && !cfg.Excluded(name) {
				return s.recycleNode(ctx, shareName, parentHandle, name, origRel)
			}
		}
	}
```

> Implementer note: `shareNameOf`, `parentRelPath`, and `joinRel` map to however the service already derives a share name and path from a `FileHandle` (grep `ShareName` usage in `pkg/metadata`). If a path is not directly available from the handle, resolve it via the existing path lookup used elsewhere in the service. Reuse, don't reinvent.

- [ ] **Step 4: Branch into recycle from `RemoveDirectory`.** In `pkg/metadata/directory.go`, add the same guarded branch at the top of `RemoveDirectory`. Because `Move` relocates the whole subtree atomically, recycling a non-empty directory moves it as one entry with a single `DeletedAt` on its root — exactly the spec's subtree behavior. Return `nil` (RemoveDirectory's signature is `error`-only) after a successful `recycleNode`.

- [ ] **Step 5: Build + vet**

Run: `go build ./... && go vet ./pkg/metadata/...`
Expected: clean. (Behavioral verification happens in Task 5's conformance suite, which runs against real stores.)

- [ ] **Step 6: Commit**

```bash
git add pkg/metadata/file_recycle.go pkg/metadata/file_remove.go pkg/metadata/directory.go
git -c gpg.format=ssh commit -S -m "feat(metadata): recycle unlinked files/dirs into #recycle when trash enabled (#190)"
```

---

### Task 4: Recycle the victim on replace-overwrite in `Move`

**Files:**
- Modify: `pkg/metadata/file_modify.go` (`Move`)

- [ ] **Step 1: Add the victim-recycle branch.** In `Move`, after resolving that the destination `toName` exists under `toDir` and would be clobbered, and before performing the overwrite, add:

```go
	if s.trashPolicy != nil {
		shareName := shareNameOf(toDir)
		if cfg, ok := s.trashPolicy.TrashConfigForShare(shareName); ok && cfg.Enabled {
			victimRel := joinRel(parentRelPath(toDir), toName)
			if !inRecycle(victimRel) && !cfg.Excluded(toName) {
				if _, err := s.recycleNode(ctx, shareName, toDir, toName, victimRel); err != nil {
					return err // never silently clobber
				}
				// Destination is now free; fall through to the normal rename,
				// which will create toName fresh rather than overwrite.
			}
		}
	}
```

> Implementer note: ensure this runs only when the destination genuinely exists (guard with the existing "dest exists" check in `Move`). Recursion is bounded — `recycleNode` itself calls `Move` into `#recycle`, but `inRecycle` short-circuits any move whose destination/source is inside the bin, so there is no loop.

- [ ] **Step 2: Build + vet**

Run: `go build ./... && go vet ./pkg/metadata/...`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add pkg/metadata/file_modify.go
git -c gpg.format=ssh commit -S -m "feat(metadata): recycle the victim on replace-overwrite rename (#190)"
```

---

### Task 5: Cross-backend trash conformance suite

**Files:**
- Create: `pkg/metadata/storetest/trash_conformance.go`
- Modify: `pkg/metadata/storetest/suite.go` (add `t.Run("Trash", …)` inside `RunConformanceSuite`)

This suite runs automatically against memory, badger, and postgres wherever `RunConformanceSuite` is invoked.

- [ ] **Step 1: Register the group.** In `suite.go`'s `RunConformanceSuite`, after the existing groups add:

```go
	t.Run("Trash", func(t *testing.T) {
		runTrashConformanceTests(t, factory)
	})
```

- [ ] **Step 2: Write the conformance tests** (`trash_conformance.go`). These build a `MetadataService` over the factory's store with a stub `TrashPolicy` that enables trash for the test share, then assert behavior. Use the same `MetadataService` construction the other conformance files use (grep an existing `runFileOpsTests` to copy the service+auth setup verbatim).

```go
package storetest

import (
	"testing"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// trashOn enables trash (no excludes) for every share queried.
type trashOn struct{ excludes []string }

func (t trashOn) TrashConfigForShare(string) (metadata.TrashConfig, bool) {
	return metadata.TrashConfig{Enabled: true, ExcludePatterns: t.excludes}, true
}

func runTrashConformanceTests(t *testing.T, factory StoreFactory) {
	t.Helper()

	t.Run("UnlinkRecyclesIntoBin", func(t *testing.T) {
		svc, ctx, root := newTrashService(t, factory, trashOn{})
		writeFileAt(t, svc, ctx, root, "doc.txt", []byte("hello"))

		f, err := svc.RemoveFile(ctx, root, "doc.txt")
		if err != nil {
			t.Fatalf("RemoveFile: %v", err)
		}
		// PayloadID cleared → adapters skip block deletion.
		if f.PayloadID != "" {
			t.Errorf("recycled file PayloadID = %q, want empty", f.PayloadID)
		}
		// Original location is gone.
		if _, err := svc.lookupForTest(ctx, root, "doc.txt"); err == nil {
			t.Error("doc.txt should no longer exist at original path")
		}
		// Lives in the bin with deletion metadata.
		bin := lookupDir(t, svc, ctx, root, metadata.RecycleDirName)
		got := lookupAttr(t, svc, ctx, bin, "doc.txt")
		if got.DeletedAt == nil {
			t.Error("recycled node missing DeletedAt")
		}
		if got.OriginalPath == "" {
			t.Error("recycled node missing OriginalPath")
		}
	})

	t.Run("DeleteInsideBinIsPermanent", func(t *testing.T) {
		svc, ctx, root := newTrashService(t, factory, trashOn{})
		writeFileAt(t, svc, ctx, root, "doc.txt", []byte("x"))
		_, _ = svc.RemoveFile(ctx, root, "doc.txt") // now in bin
		bin := lookupDir(t, svc, ctx, root, metadata.RecycleDirName)

		f, err := svc.RemoveFile(ctx, bin, "doc.txt")
		if err != nil {
			t.Fatalf("RemoveFile inside bin: %v", err)
		}
		// Permanent delete → PayloadID present so caller frees blocks.
		if f.PayloadID == "" {
			t.Error("delete inside bin should return PayloadID for block deletion")
		}
		if _, err := svc.lookupForTest(ctx, bin, "doc.txt"); err == nil {
			t.Error("doc.txt should be gone from bin after permanent delete")
		}
	})

	t.Run("ExcludePatternBypassesBin", func(t *testing.T) {
		svc, ctx, root := newTrashService(t, factory, trashOn{excludes: []string{"*.tmp"}})
		writeFileAt(t, svc, ctx, root, "scratch.tmp", []byte("x"))

		f, err := svc.RemoveFile(ctx, root, "scratch.tmp")
		if err != nil {
			t.Fatalf("RemoveFile: %v", err)
		}
		if f.PayloadID == "" {
			t.Error("excluded file should be hard-deleted (PayloadID present)")
		}
		bin, err := svc.lookupForTest(ctx, root, metadata.RecycleDirName)
		if err == nil && bin != nil {
			if _, err := svc.lookupForTest(ctx, mustHandle(bin), "scratch.tmp"); err == nil {
				t.Error("excluded file must not appear in bin")
			}
		}
	})

	t.Run("CollisionGetsTimestampSuffix", func(t *testing.T) {
		svc, ctx, root := newTrashService(t, factory, trashOn{})
		writeFileAt(t, svc, ctx, root, "a.txt", []byte("1"))
		_, _ = svc.RemoveFile(ctx, root, "a.txt")
		writeFileAt(t, svc, ctx, root, "a.txt", []byte("2"))
		_, _ = svc.RemoveFile(ctx, root, "a.txt")

		bin := lookupDir(t, svc, ctx, root, metadata.RecycleDirName)
		names := listNames(t, svc, ctx, bin)
		count := 0
		for _, n := range names {
			if n == "a.txt" || (len(n) > 5 && n[:5] == "a.txt") {
				count++
			}
		}
		if count < 2 {
			t.Errorf("expected 2 recycled a.txt entries (one suffixed), got %d in %v", count, names)
		}
	})

	t.Run("SubtreeRecycledAsOneEntry", func(t *testing.T) {
		svc, ctx, root := newTrashService(t, factory, trashOn{})
		dir := mkdirAt(t, svc, ctx, root, "project")
		writeFileAt(t, svc, ctx, dir, "main.go", []byte("package main"))

		if err := svc.RemoveDirectory(ctx, root, "project"); err != nil {
			t.Fatalf("RemoveDirectory: %v", err)
		}
		bin := lookupDir(t, svc, ctx, root, metadata.RecycleDirName)
		got := lookupAttr(t, svc, ctx, bin, "project")
		if got.DeletedAt == nil {
			t.Error("recycled dir root should carry DeletedAt")
		}
		// Child preserved under the recycled subtree.
		recDir := lookupDir(t, svc, ctx, bin, "project")
		_ = lookupAttr(t, svc, ctx, recDir, "main.go")
	})

	_ = time.Now // keep import if helpers don't reference time
}
```

- [ ] **Step 3: Add the small test helpers** at the bottom of `trash_conformance.go`: `newTrashService` (build the `MetadataService` from the factory store, `SetTrashPolicy`, create a share + root, return svc/ctx/rootHandle), `writeFileAt`, `mkdirAt`, `lookupDir`, `lookupAttr`, `listNames`, `mustHandle`, and small test-only `lookupForTest` wrappers. Copy the service/auth/share bootstrap from an existing conformance file (e.g. the one containing `runFileOpsTests`) so it matches each backend's setup exactly.

- [ ] **Step 4: Run across memory + badger** (postgres requires a DSN; CI runs it)

Run: `go test ./pkg/metadata/storetest/... -run 'Trash' -v`
Expected: PASS for the memory and badger factories.

- [ ] **Step 5: Run the backends' own conformance entrypoints**

Run: `go test ./pkg/metadata/store/memory/... ./pkg/metadata/store/badger/... -run 'Conformance|Trash'`
Expected: PASS (postgres conformance is `//go:build integration` and runs in CI).

- [ ] **Step 6: Commit**

```bash
git add pkg/metadata/storetest/trash_conformance.go pkg/metadata/storetest/suite.go
git -c gpg.format=ssh commit -S -m "test(metadata): cross-backend trash conformance suite (#190)"
```

---

## Phase 2 — Trash service + reaper (runtime)

### Task 6: `trash.Service` skeleton + Restore + Empty + accounting

**Files:**
- Create: `pkg/controlplane/runtime/trash/service.go`
- Create: `pkg/controlplane/runtime/trash/service_test.go`

- [ ] **Step 1: Write the failing test** (`service_test.go`) for `Restore` path-resolution and `Empty` enumeration using an in-memory `MetadataService` (mirror the runtime test setup used by `snapshot_*_test.go`).

```go
package trash

import "testing"

func TestRestoreClearsDeletionMetadata(t *testing.T) {
	svc, msvc, ctx, root, shareName := newTestTrash(t)
	recycle(t, msvc, ctx, root, "doc.txt", []byte("hi")) // helper deletes via RemoveFile

	if err := svc.Restore(ctx, shareName, "doc.txt", ""); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	attr := attrAt(t, msvc, ctx, root, "doc.txt")
	if attr.DeletedAt != nil {
		t.Error("restored file should have nil DeletedAt")
	}
}

func TestEmptyRemovesAllBinEntries(t *testing.T) {
	svc, msvc, ctx, root, shareName := newTestTrash(t)
	recycle(t, msvc, ctx, root, "a.txt", []byte("a"))
	recycle(t, msvc, ctx, root, "b.txt", []byte("b"))

	n, err := svc.Empty(ctx, shareName, true)
	if err != nil {
		t.Fatalf("Empty: %v", err)
	}
	if n != 2 {
		t.Errorf("Empty removed %d, want 2", n)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/controlplane/runtime/trash/ -run 'TestRestore|TestEmpty'`
Expected: FAIL — package/methods undefined.

- [ ] **Step 3: Implement `service.go`.** The service holds a reference to the runtime's share/metadata accessors. Model it on `clients.Registry` for lifecycle and on how `snapshot.go` reaches the metadata service per share.

```go
package trash

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Entry is one recycled item surfaced to the API/CLI.
type Entry struct {
	BinPath      string     `json:"bin_path"`      // path under #recycle
	OriginalPath string     `json:"original_path"`
	DeletedBy    string     `json:"deleted_by"`
	DeletedAt    time.Time  `json:"deleted_at"`
	Size         uint64     `json:"size"`
	IsDir        bool       `json:"is_dir"`
}

// Deps is the narrow surface the trash service needs from the runtime,
// keeping it testable without the whole Runtime.
type Deps interface {
	// MetadataServiceForShare returns the metadata service + share root for a
	// share, or ok=false if unknown/disabled.
	MetadataServiceForShare(shareName string) (svc *metadata.MetadataService, root metadata.FileHandle, ok bool)
	// TrashConfigForShare returns retention/quota/restrict knobs.
	TrashConfigForShare(shareName string) (Config, bool)
	// EnabledTrashShares lists shares with trash currently enabled (reaper).
	EnabledTrashShares() []string
}

// Config is the full per-share trash policy (superset of metadata.TrashConfig).
type Config struct {
	Enabled         bool
	RetentionDays   int
	RestrictToAdmin bool
	MaxBytes        int64
	ExcludePatterns []string
}

type Service struct {
	deps     Deps
	interval time.Duration
	stopCh   chan struct{}
}

func New(deps Deps, reapInterval time.Duration) *Service {
	if reapInterval <= 0 {
		reapInterval = time.Hour
	}
	return &Service{deps: deps, interval: reapInterval, stopCh: make(chan struct{})}
}

// List enumerates bin entries for a share (DeletedAt != nil under #recycle).
func (s *Service) List(ctx *metadata.AuthContext, shareName string) ([]Entry, error) {
	svc, root, ok := s.deps.MetadataServiceForShare(shareName)
	if !ok {
		return nil, metadata.ErrNoEntity
	}
	return walkBin(ctx, svc, root) // see Step 4
}

// Restore moves a bin entry back to dest (or its OriginalPath if dest == "")
// and clears the deletion metadata. Fails with ErrExist if the target exists.
func (s *Service) Restore(ctx *metadata.AuthContext, shareName, binPath, dest string) error {
	svc, root, ok := s.deps.MetadataServiceForShare(shareName)
	if !ok {
		return metadata.ErrNoEntity
	}
	return restoreEntry(ctx, svc, root, binPath, dest) // see Step 4
}

// Empty permanently removes every bin entry. force is required when the
// share's RestrictToAdmin is set and the caller is not an admin (the API
// handler enforces admin; force reflects an explicit operator purge).
func (s *Service) Empty(ctx *metadata.AuthContext, shareName string, force bool) (int, error) {
	svc, root, ok := s.deps.MetadataServiceForShare(shareName)
	if !ok {
		return 0, metadata.ErrNoEntity
	}
	return emptyBin(ctx, svc, root) // see Step 4 — permanent RemoveFile of each entry
}
```

- [ ] **Step 4: Implement `walkBin`, `restoreEntry`, `emptyBin`** in the same file. `walkBin` lists `#recycle` recursively, collecting nodes with `DeletedAt != nil` (top-level recycled roots). `restoreEntry` resolves `dest` (default the stored `OriginalPath`), checks the destination is free (else `metadata.ErrExist`), recreates the parent chain if missing, `Move`s the entry out of the bin, then clears the three fields via the service's SetAttr/PutFile path. `emptyBin` walks `#recycle` and calls the **permanent** delete — since the node is inside the bin, `RemoveFile` short-circuits via `inRecycle` and returns a real `PayloadID`; the service must then free blocks the same way adapters do. Reuse `GetBlockStoreForHandle` + `blockStore.Delete` (grep its use in `pkg/controlplane/runtime`).

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./pkg/controlplane/runtime/trash/ -run 'TestRestore|TestEmpty'`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add pkg/controlplane/runtime/trash/service.go pkg/controlplane/runtime/trash/service_test.go
git -c gpg.format=ssh commit -S -m "feat(runtime): trash.Service list/restore/empty (#190)"
```

---

### Task 7: Reaper (retention + max-size) + lifecycle wiring

**Files:**
- Create: `pkg/controlplane/runtime/trash/reaper.go`
- Create: `pkg/controlplane/runtime/trash/reaper_test.go`
- Modify: `pkg/controlplane/runtime/runtime.go` (construct `trash.Service`, expose `Start/Stop`)
- Modify: `pkg/controlplane/runtime/lifecycle/service.go` wiring point (start in `serve`, stop in `shutdown`) — follow the `clients.Registry.StartSweeper`/`Stop` precedent

- [ ] **Step 1: Write the failing test** (`reaper_test.go`) with an injected clock so retention expiry is deterministic.

```go
package trash

import (
	"testing"
	"time"
)

func TestReapDeletesExpiredEntries(t *testing.T) {
	svc, msvc, ctx, root, shareName := newTestTrash(t)
	recycle(t, msvc, ctx, root, "old.txt", []byte("x"))

	// Config: 7-day retention; pretend 8 days passed.
	now := time.Now().Add(8 * 24 * time.Hour)
	removed, err := svc.reapShareAt(ctx, shareName, Config{Enabled: true, RetentionDays: 7}, now)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if removed != 1 {
		t.Errorf("reaped %d, want 1", removed)
	}
}

func TestMaxSizeEvictsOldestFirst(t *testing.T) {
	svc, msvc, ctx, root, shareName := newTestTrash(t)
	recycle(t, msvc, ctx, root, "old.bin", make([]byte, 1000))
	time.Sleep(2 * time.Millisecond)
	recycle(t, msvc, ctx, root, "new.bin", make([]byte, 1000))

	// Cap at 1500 bytes → must evict the oldest (old.bin).
	removed, err := svc.evictToCap(ctx, shareName, 1500)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if removed != 1 {
		t.Errorf("evicted %d, want 1", removed)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/controlplane/runtime/trash/ -run 'TestReap|TestMaxSize'`
Expected: FAIL — `reapShareAt`/`evictToCap` undefined.

- [ ] **Step 3: Implement `reaper.go`**

```go
package trash

import (
	"context"
	"sort"
	"time"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// Start launches the reaper goroutine (clients.Registry.StartSweeper pattern).
func (s *Service) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.stopCh:
				return
			case <-ticker.C:
				s.reapAll(ctx)
			}
		}
	}()
}

// Stop signals the reaper to exit (idempotent).
func (s *Service) Stop() {
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

func (s *Service) reapAll(ctx context.Context) {
	now := time.Now().UTC()
	for _, share := range s.deps.EnabledTrashShares() {
		cfg, ok := s.deps.TrashConfigForShare(share)
		if !ok || !cfg.Enabled {
			continue
		}
		actx := metadata.NewSystemAuthContext(ctx) // grep existing system/root auth ctx helper
		if cfg.RetentionDays > 0 {
			_, _ = s.reapShareAt(actx, share, cfg, now)
		}
		if cfg.MaxBytes > 0 {
			_, _ = s.evictToCap(actx, share, cfg.MaxBytes)
		}
	}
}

// reapShareAt permanently deletes entries older than RetentionDays as of `now`.
func (s *Service) reapShareAt(ctx *metadata.AuthContext, share string, cfg Config, now time.Time) (int, error) {
	entries, err := s.List(ctx, share)
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-time.Duration(cfg.RetentionDays) * 24 * time.Hour)
	removed := 0
	for _, e := range entries {
		if e.DeletedAt.Before(cutoff) {
			if err := s.purgeEntry(ctx, share, e); err != nil {
				return removed, err
			}
			removed++
		}
	}
	return removed, nil
}

// evictToCap removes oldest-first until total bin bytes <= cap.
func (s *Service) evictToCap(ctx *metadata.AuthContext, share string, cap int64) (int, error) {
	entries, err := s.List(ctx, share)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += int64(e.Size)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].DeletedAt.Before(entries[j].DeletedAt) })
	removed := 0
	for _, e := range entries {
		if total <= cap {
			break
		}
		if err := s.purgeEntry(ctx, share, e); err != nil {
			return removed, err
		}
		total -= int64(e.Size)
		removed++
	}
	return removed, nil
}
```

- [ ] **Step 4: Implement `purgeEntry`** (permanent delete of one bin entry + free its blocks) reusing the `emptyBin` element logic from Task 6.

- [ ] **Step 5: Wire into runtime + lifecycle.** In `runtime.go`, construct the `trash.Service` (with a `Deps` impl backed by the runtime — Task 9 supplies `TrashConfigForShare`/`EnabledTrashShares`). Add `Runtime.startTrashReaper(ctx)` / `stopTrashReaper()`. In `lifecycle/service.go`, start it alongside the other background services in `serve` and stop it in `shutdown` (same place `settings.Start`/`Stop` are called).

- [ ] **Step 6: Run tests + race**

Run: `go test -race ./pkg/controlplane/runtime/trash/ ./pkg/controlplane/runtime/ -run 'Trash|Reap|MaxSize'`
Expected: PASS, no race.

- [ ] **Step 7: Commit**

```bash
git add pkg/controlplane/runtime/trash/reaper.go pkg/controlplane/runtime/trash/reaper_test.go pkg/controlplane/runtime/runtime.go pkg/controlplane/runtime/lifecycle/service.go
git -c gpg.format=ssh commit -S -m "feat(runtime): trash reaper (retention + max-size) wired into lifecycle (#190)"
```

---

### Task 8: Disable → auto-empty

**Files:**
- Modify: `pkg/controlplane/runtime/trash/service.go` (add `OnDisable`)
- Test: `pkg/controlplane/runtime/trash/service_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestDisableAutoEmpties(t *testing.T) {
	svc, msvc, ctx, root, shareName := newTestTrash(t)
	recycle(t, msvc, ctx, root, "a.txt", []byte("a"))
	recycle(t, msvc, ctx, root, "b.txt", []byte("b"))

	if err := svc.OnDisable(ctx, shareName); err != nil {
		t.Fatalf("OnDisable: %v", err)
	}
	// #recycle removed entirely.
	if _, err := lookupChildForTest(msvc, ctx, root, "#recycle"); err == nil {
		t.Error("#recycle should be gone after disable")
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./pkg/controlplane/runtime/trash/ -run TestDisableAutoEmpties`
Expected: FAIL — `OnDisable` undefined.

- [ ] **Step 3: Implement `OnDisable`** — call `Empty(ctx, share, true)`, then remove the now-empty `#recycle` directory itself (permanent `RemoveDirectory` on the bin root, which `inRecycle` permits because it's the bin root being targeted by the service, not a recycle trigger).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./pkg/controlplane/runtime/trash/ -run TestDisableAutoEmpties`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/controlplane/runtime/trash/service.go pkg/controlplane/runtime/trash/service_test.go
git -c gpg.format=ssh commit -S -m "feat(runtime): auto-empty the bin when trash is disabled (#190)"
```

---

## Phase 3 — Share config (struct → DB → REST → apiclient → dfsctl) + the locked policy

### Task 9: Share config fields + locked `TrashPolicy`/`Deps` accessors

**Files:**
- Modify: `pkg/controlplane/runtime/shares/service.go` (`Share`, `ShareConfig`, `AddShare`, add locked accessors)
- Modify: `pkg/controlplane/store/shares.go` (column map) + the `models.Share` struct (grep it — add GORM columns)
- Test: `pkg/controlplane/runtime/shares/service_test.go` (race test for the accessor)

- [ ] **Step 1: Add fields to `Share` and `ShareConfig`** (mirror `AccessBasedEnumeration` style, comments included):

```go
	// TrashEnabled turns on the per-share recycle bin (#190). Default false.
	// Toggleable on a live share; the recycle decision is read per-delete via
	// the locked TrashConfigForShare accessor, so it takes effect immediately.
	TrashEnabled bool
	// TrashRetentionDays auto-empties bin entries older than N days (0 = keep
	// forever / manual empty only).
	TrashRetentionDays int
	// TrashRestrictToAdmin limits empty/force-delete to admins; users may still
	// restore their own items.
	TrashRestrictToAdmin bool
	// TrashMaxBytes caps total bin bytes (0 = unbounded); over-cap evicts oldest.
	TrashMaxBytes int64
	// TrashExcludePatterns are globs that bypass the bin (immediate delete).
	TrashExcludePatterns []string
```

Add the identical block to `ShareConfig`.

- [ ] **Step 2: Persist via the DB layer.** In `models.Share` add columns `TrashEnabled bool`, `TrashRetentionDays int`, `TrashRestrictToAdmin bool`, `TrashMaxBytes int64`, `TrashExcludePatterns string` (store the globs as a comma-joined string or JSON — match how any existing `[]string` share field is stored; grep `AllowedClients`/`BlockedOperations` for the established slice-in-DB pattern and copy it). In `pkg/controlplane/store/shares.go`'s update column map add the new keys (mirroring `"access_based_enumeration": share.AccessBasedEnumeration`).

- [ ] **Step 3: Populate the runtime `Share` in `AddShare`** (mirror line ~544):

```go
		TrashEnabled:         config.TrashEnabled,
		TrashRetentionDays:   config.TrashRetentionDays,
		TrashRestrictToAdmin: config.TrashRestrictToAdmin,
		TrashMaxBytes:        config.TrashMaxBytes,
		TrashExcludePatterns: config.TrashExcludePatterns,
```

- [ ] **Step 4: Add LOCKED accessors on the shares service** (this is the #936-race fix baked in — read under the service mutex, return a value, never a pointer):

```go
// TrashConfigForShare returns the recycle-bin policy for a share, read under
// the service lock. It satisfies metadata.TrashPolicy (via a thin adapter
// returning metadata.TrashConfig) and trash.Deps. Never hand out the *Share.
func (s *Service) TrashConfigForShare(name string) (trash.Config, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	sh, ok := s.shares[name] // use the actual map field name
	if !ok {
		return trash.Config{}, false
	}
	return trash.Config{
		Enabled:         sh.TrashEnabled,
		RetentionDays:   sh.TrashRetentionDays,
		RestrictToAdmin: sh.TrashRestrictToAdmin,
		MaxBytes:        sh.TrashMaxBytes,
		ExcludePatterns: append([]string(nil), sh.TrashExcludePatterns...),
	}, true
}

// EnabledTrashShares returns the names of shares with trash on (reaper loop).
func (s *Service) EnabledTrashShares() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for name, sh := range s.shares {
		if sh.TrashEnabled {
			out = append(out, name)
		}
	}
	return out
}
```

Add a tiny `metadata.TrashPolicy` adapter (in the runtime, where shares + metadata service are assembled) that calls `TrashConfigForShare` and maps to `metadata.TrashConfig{Enabled, ExcludePatterns}`, then `metadataService.SetTrashPolicy(adapter)` during runtime construction.

- [ ] **Step 5: Write a `-race` test** proving concurrent config read + a `SetShareTrashConfig`-style mutation don't race (mirror the test #936 was asked to add):

```go
func TestTrashConfigForShare_NoRace(t *testing.T) {
	svc := newTestSharesService(t) // existing helper
	addTrashShare(t, svc, "/data") // helper: AddShare with TrashEnabled=true
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			svc.SetShareTrashConfig("/data", trash.Config{Enabled: i%2 == 0, RetentionDays: i})
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_, _ = svc.TrashConfigForShare("/data")
	}
	<-done
}
```

- [ ] **Step 6: Implement `SetShareTrashConfig`** (live update under the write lock + persist to DB), mirroring how the existing update path mutates a share. Keep it write-locked so it pairs safely with the RLock accessor.

```go
func (s *Service) SetShareTrashConfig(name string, cfg trash.Config) error {
	s.mu.Lock()
	sh, ok := s.shares[name]
	if !ok {
		s.mu.Unlock()
		return ErrShareNotFound // use the package's existing sentinel
	}
	sh.TrashEnabled = cfg.Enabled
	sh.TrashRetentionDays = cfg.RetentionDays
	sh.TrashRestrictToAdmin = cfg.RestrictToAdmin
	sh.TrashMaxBytes = cfg.MaxBytes
	sh.TrashExcludePatterns = append([]string(nil), cfg.ExcludePatterns...)
	s.mu.Unlock()
	return s.persistShare(name) // reuse the existing DB-update call
}
```

- [ ] **Step 7: Run race test + build**

Run: `go test -race ./pkg/controlplane/runtime/shares/ -run Trash && go build ./...`
Expected: PASS, no race, clean build.

- [ ] **Step 8: Commit**

```bash
git add pkg/controlplane/runtime/shares/service.go pkg/controlplane/store/shares.go pkg/controlplane/runtime/shares/service_test.go pkg/controlplane/runtime/runtime.go
git -c gpg.format=ssh commit -S -m "feat(shares): per-share trash config + locked policy accessor (#190)"
```

---

### Task 10: REST share create/update + apiclient fields

**Files:**
- Modify: `internal/controlplane/api/handlers/shares.go` (`CreateShareRequest`/`UpdateShareRequest` structs + create + update logic)
- Modify: `pkg/apiclient/shares.go` (`CreateShareRequest`/`UpdateShareRequest`)
- Modify: `internal/controlplane/api/handlers/shares.go` update path to call `runtime.SetShareTrashConfig` for a live apply (so enabling on a running share takes effect without restart)
- Test: `internal/controlplane/api/handlers/shares_test.go`

- [ ] **Step 1: Add request fields** (pointer types, exactly like `AccessBasedEnumeration`) to BOTH the handler request structs and the apiclient request structs:

```go
	TrashEnabled         *bool    `json:"trash_enabled,omitempty"`
	TrashRetentionDays   *int     `json:"trash_retention_days,omitempty"`
	TrashRestrictToAdmin *bool    `json:"trash_restrict_to_admin,omitempty"`
	TrashMaxBytes        *int64   `json:"trash_max_bytes,omitempty"`
	TrashExcludePatterns []string `json:"trash_exclude_patterns,omitempty"`
```

- [ ] **Step 2: Apply in create** (mirror lines 301–309) — default to disabled/zero when nil; set the `ShareConfig` fields.

- [ ] **Step 3: Apply in update** (mirror lines 508–514). After mutating `share` + `store.UpdateShare`, ALSO call the runtime live-apply so a running share reflects the toggle immediately:

```go
	if req.TrashEnabled != nil || req.TrashRetentionDays != nil || req.TrashRestrictToAdmin != nil || req.TrashMaxBytes != nil || req.TrashExcludePatterns != nil {
		cfg := trash.Config{
			Enabled:         share.TrashEnabled,
			RetentionDays:   share.TrashRetentionDays,
			RestrictToAdmin: share.TrashRestrictToAdmin,
			MaxBytes:        share.TrashMaxBytes,
			ExcludePatterns: share.TrashExcludePatterns,
		}
		_ = h.runtime.SetShareTrashConfig(share.Name, cfg)
		if !cfg.Enabled {
			_ = h.runtime.Trash().OnDisable(systemAuthCtx(r.Context()), share.Name) // auto-empty
		}
	}
```

- [ ] **Step 4: Write a handler test** asserting a PATCH with `trash_enabled=true, trash_retention_days=7` persists and round-trips on GET. Mirror an existing `shares_test.go` update test; assert errors (no ignored-error nits — learn from #936).

- [ ] **Step 5: Run + build**

Run: `go test ./internal/controlplane/api/handlers/ -run Share && go build ./...`
Expected: PASS, clean.

- [ ] **Step 6: Commit**

```bash
git add internal/controlplane/api/handlers/shares.go pkg/apiclient/shares.go internal/controlplane/api/handlers/shares_test.go
git -c gpg.format=ssh commit -S -m "feat(api): expose per-share trash config on share create/update (#190)"
```

---

### Task 11: `dfsctl share edit` flags

**Files:**
- Modify: `cmd/dfsctl/commands/share/edit.go`
- Modify: `cmd/dfsctl/commands/share/create.go` (same flags on create)
- Test: `cmd/dfsctl/commands/share/edit_test.go` (if present; else add)

- [ ] **Step 1: Declare flags** (mirror `--access-based-enumeration` string-bool parsing for the bools; plain Int/Int64/StringSlice for the rest):

```go
	editCmd.Flags().StringVar(&editTrashEnabled, "enable-trash", "", "Enable/disable the recycle bin (true|false)")
	editCmd.Flags().IntVar(&editTrashRetentionDays, "trash-retention-days", -1, "Auto-empty items older than N days (0 = keep forever; -1 = no change)")
	editCmd.Flags().StringVar(&editTrashRestrict, "trash-restrict-empty-to-admin", "", "Restrict empty/purge to admins (true|false)")
	editCmd.Flags().Int64Var(&editTrashMaxBytes, "trash-max-size", -1, "Cap total bin bytes (0 = unbounded; -1 = no change)")
	editCmd.Flags().StringSliceVar(&editTrashExclude, "trash-exclude", nil, "Globs that bypass the bin (repeatable)")
```

- [ ] **Step 2: Map flags → request** in `runEdit` (string-bool parse for the two bools as in the ABE example; for the ints, only set when `>= 0` / changed; mark `hasUpdate = true` when any is set). For `--trash-exclude`, set `req.TrashExcludePatterns` when the flag was `Changed()`.

- [ ] **Step 3: Build + smoke**

Run: `go build -o /tmp/dfsctl cmd/dfsctl/main.go && /tmp/dfsctl share edit --help | grep -i trash`
Expected: the five trash flags listed.

- [ ] **Step 4: Commit**

```bash
git add cmd/dfsctl/commands/share/edit.go cmd/dfsctl/commands/share/create.go
git -c gpg.format=ssh commit -S -m "feat(dfsctl): share edit/create trash config flags (#190)"
```

---

## Phase 4 — `dfsctl trash` group + REST trash endpoints

### Task 12: REST trash endpoints + handler

**Files:**
- Create: `internal/controlplane/api/handlers/trash.go` (+ `_test.go`)
- Modify: `pkg/controlplane/api/router.go` (register `/{name}/trash` routes under the admin-gated `/shares` group, mirroring the snapshots block)

- [ ] **Step 1: Register routes** (after the snapshots block, ~line 209):

```go
	trashHandler := handlers.NewTrashHandler(rt)
	r.Route("/{name}/trash", func(r chi.Router) {
		r.Get("/", trashHandler.List)
		r.Post("/restore", trashHandler.Restore) // body: {bin_path, to}
		r.Post("/empty", trashHandler.Empty)     // body: {force}
		r.Get("/status", trashHandler.Status)
	})
```

- [ ] **Step 2: Implement the handler** (mirror the snapshot handler: `chi.URLParam(r,"name")`, `decodeJSONBody`, `writeJSON`/`writeProblem`). `List` → `rt.Trash().List`; `Restore` → `rt.Trash().Restore`; `Empty` → enforce `RestrictToAdmin` (admin already required by the parent group, so this is the user-vs-admin distinction — for v1, the whole `/shares` group is admin-gated, so `Empty` is admin-only by construction; document that end-user restore happens over the mount, not this endpoint); `Status` → size/count/oldest/next-reap/purge-state.

- [ ] **Step 3: Handler test** — `httptest` server, seed a recycled entry via an in-memory runtime, assert `GET /trash` returns it and `POST /empty` clears it. Assert all decode/marshal errors (no ignored errors).

- [ ] **Step 4: Run + build**

Run: `go test ./internal/controlplane/api/handlers/ -run Trash && go build ./...`
Expected: PASS, clean.

- [ ] **Step 5: Commit**

```bash
git add internal/controlplane/api/handlers/trash.go internal/controlplane/api/handlers/trash_test.go pkg/controlplane/api/router.go
git -c gpg.format=ssh commit -S -m "feat(api): per-share trash list/restore/empty/status endpoints (#190)"
```

---

### Task 13: apiclient trash methods

**Files:**
- Create: `pkg/apiclient/trash.go` (+ `_test.go`)

- [ ] **Step 1: Implement methods** mirroring `pkg/apiclient/shares.go` request/response style: `TrashList(share) ([]TrashEntry, error)`, `TrashRestore(share, binPath, to string) error`, `TrashEmpty(share string, force bool) error`, `TrashStatus(share) (*TrashStatus, error)`. Define `TrashEntry`/`TrashStatus` mirroring the handler JSON.

- [ ] **Step 2: Test** with an `httptest` stub server (mirror `gc_test.go`'s `newGCServer`), asserting path/method/body and decoding.

- [ ] **Step 3: Run + commit**

```bash
go test ./pkg/apiclient/ -run Trash
git add pkg/apiclient/trash.go pkg/apiclient/trash_test.go
git -c gpg.format=ssh commit -S -m "feat(apiclient): trash list/restore/empty/status (#190)"
```

---

### Task 14: `dfsctl trash` command group

**Files:**
- Create: `cmd/dfsctl/commands/trash/{trash,list,restore,empty,status}.go` (+ `*_test.go`)
- Modify: wherever top-level command groups are registered (grep `store.Cmd` / `share.Cmd` `AddCommand` in `cmd/dfsctl/`), add `trash.Cmd`

- [ ] **Step 1: Implement the group** following `cmd/dfsctl/commands/store/block/gc.go` verbatim in structure: `var Cmd = &cobra.Command{Use:"trash", Short:"Recycle-bin management"}`; subcommands `list <share>`, `restore <share> <bin-path> [--to P]`, `empty <share> [--force]`, `status <share>`, each `cobra.ExactArgs`, each parsing `cmdutil.GetOutputFormatParsed()` and printing via `output.PrintJSON`/`PrintYAML`/`output.SimpleTable`. `list` renders a table: Path, Original, Deleted By, Deleted At, Size, Expires In.

- [ ] **Step 2: Register** the group with the root dfsctl command.

- [ ] **Step 3: Tests** mirroring `gc_test.go` (`newGCServer`-style stub): assert each verb hits the right path/method/body and renders the expected fields; assert `ExactArgs` rejects missing share.

- [ ] **Step 4: Build + smoke + run tests**

Run: `go build -o /tmp/dfsctl cmd/dfsctl/main.go && /tmp/dfsctl trash --help && go test ./cmd/dfsctl/commands/trash/...`
Expected: group help shows list/restore/empty/status; tests PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/dfsctl/commands/trash/
git -c gpg.format=ssh commit -S -m "feat(dfsctl): trash list/restore/empty/status command group (#190)"
```

---

## Phase 5 — E2E (NFS + SMB, cross-OS) + docs

### Task 15: NFS e2e trash test

**Files:**
- Create: `test/e2e/trash_nfs_test.go`

- [ ] **Step 1: Write the e2e test** (mirror `file_operations_nfs_test.go` setup; create the share with trash enabled via the CLI runner — add a `helpers.WithShareTrashEnabled()` share option following the existing `helpers.WithShareDefaultPermission` pattern).

```go
//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/require"
)

func TestNFSTrashRecycleAndRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping NFS trash e2e in short mode")
	}
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(sp.ForceKill)
	runner := helpers.LoginAsAdmin(t, sp.APIURL())

	meta := helpers.UniqueTestName("meta")
	local := helpers.UniqueTestName("local")
	share := "/export"
	_, err := runner.CreateMetadataStore(meta, "memory")
	require.NoError(t, err)
	_, err = runner.CreateLocalBlockStore(local, "memory")
	require.NoError(t, err)
	_, err = runner.CreateShare(share, meta, local, helpers.WithShareTrashEnabled())
	require.NoError(t, err)

	port := helpers.FindFreePort(t)
	_, err = runner.EnableAdapter("nfs", helpers.WithAdapterPort(port))
	require.NoError(t, err)
	require.NoError(t, helpers.WaitForAdapterStatus(t, runner, "nfs", true, 5*time.Second))
	framework.WaitForServer(t, port, 10*time.Second)
	mount := framework.MountNFS(t, port)
	t.Cleanup(mount.Cleanup)

	// Write, then delete over the mount.
	content := []byte("recycle me")
	f := mount.FilePath("doc.txt")
	framework.WriteFile(t, f, content)
	require.NoError(t, os.Remove(f))

	// Gone from original location.
	_, statErr := os.Stat(f)
	require.True(t, os.IsNotExist(statErr), "file should be gone from original path")

	// Present in #recycle, byte-identical.
	recycled := mount.FilePath(filepath.Join("#recycle", "doc.txt"))
	require.Eventually(t, func() bool {
		_, err := os.Stat(recycled)
		return err == nil
	}, 5*time.Second, 100*time.Millisecond, "file should appear in #recycle")
	got := framework.ReadFile(t, recycled)
	require.Equal(t, content, got, "recycled bytes must match")

	// Restore by moving back out over the mount.
	require.NoError(t, os.Rename(recycled, f))
	require.Equal(t, content, framework.ReadFile(t, f), "restored bytes must match")

	// Delete inside #recycle is permanent.
	framework.WriteFile(t, f, content)
	require.NoError(t, os.Remove(f)) // back to bin
	require.NoError(t, os.Remove(recycled)) // permanent
	_, err2 := os.Stat(recycled)
	require.True(t, os.IsNotExist(err2))
}
```

- [ ] **Step 2: Add `helpers.WithShareTrashEnabled()`** in `test/e2e/helpers/` (set `TrashEnabled` in the create-share request the runner builds — mirror `WithShareDefaultPermission`).

- [ ] **Step 3: Compile-check** (e2e needs sudo+NFS to run; CI executes it)

Run: `go vet -tags e2e ./test/e2e/...`
Expected: clean.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/trash_nfs_test.go test/e2e/helpers/
git -c gpg.format=ssh commit -S -m "test(e2e): NFS recycle + restore + permanent-in-bin (#190)"
```

---

### Task 16: SMB e2e trash test + cross-OS coverage note

**Files:**
- Create: `test/e2e/trash_smb_test.go`

- [ ] **Step 1: Write the SMB e2e test** mirroring `file_operations_smb_test.go` (auth user + `framework.MountSMB`). Cover: create file, delete (SMB delete-on-close → bin), confirm in `#recycle`, restore via rename-out, and an SMB **rename-with-replace** that recycles the victim. The SMB delete-on-close path is what validates Windows-client semantics (Explorer delete = set delete-on-close); `#recycle` is a valid name on Windows (`#` is legal). macOS/Linux SMB clients exercise the same wire path via `framework.MountSMB`.

```go
//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/marmos91/dittofs/test/e2e/framework"
	"github.com/marmos91/dittofs/test/e2e/helpers"
	"github.com/stretchr/testify/require"
)

func TestSMBTrashRecycleAndRestore(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping SMB trash e2e in short mode")
	}
	sp := helpers.StartServerProcess(t, "")
	t.Cleanup(func() { if t.Failed() { sp.DumpLogs(t) }; sp.ForceKill() })
	cli := helpers.LoginAsAdmin(t, sp.APIURL())

	meta := helpers.UniqueTestName("meta")
	local := helpers.UniqueTestName("local")
	share := "/export"
	_, err := cli.CreateMetadataStore(meta, "memory")
	require.NoError(t, err)
	_, err = cli.CreateLocalBlockStore(local, "memory")
	require.NoError(t, err)
	_, err = cli.CreateShare(share, meta, local,
		helpers.WithShareDefaultPermission("read-write"), helpers.WithShareTrashEnabled())
	require.NoError(t, err)

	user := helpers.UniqueTestName("smbuser")
	pass := "testpass123"
	_, err = cli.CreateUser(user, pass)
	require.NoError(t, err)
	require.NoError(t, cli.GrantUserPermission(share, user, "read-write"))

	port := helpers.FindFreePort(t)
	_, err = cli.EnableAdapter("smb", helpers.WithAdapterPort(port))
	require.NoError(t, err)
	require.NoError(t, helpers.WaitForAdapterStatus(t, cli, "smb", true, 5*time.Second))
	framework.WaitForServer(t, port, 10*time.Second)
	mount := framework.MountSMB(t, port, framework.SMBCredentials{Username: user, Password: pass})
	t.Cleanup(mount.Cleanup)

	content := []byte("smb recycle me")
	f := mount.FilePath("doc.txt")
	framework.WriteFile(t, f, content)
	require.NoError(t, os.Remove(f)) // SMB delete-on-close

	recycled := mount.FilePath(filepath.Join("#recycle", "doc.txt"))
	require.Eventually(t, func() bool { _, err := os.Stat(recycled); return err == nil },
		5*time.Second, 100*time.Millisecond, "file should appear in #recycle over SMB")
	require.Equal(t, content, framework.ReadFile(t, recycled))

	require.NoError(t, os.Rename(recycled, f)) // restore
	require.Equal(t, content, framework.ReadFile(t, f))
}
```

- [ ] **Step 2: Compile-check**

Run: `go vet -tags e2e ./test/e2e/...`
Expected: clean.

- [ ] **Step 3: Commit**

```bash
git add test/e2e/trash_smb_test.go
git -c gpg.format=ssh commit -S -m "test(e2e): SMB recycle + restore (delete-on-close path) (#190)"
```

---

### Task 17: Docs

**Files:**
- Modify: `docs/CLI.md` (add a `dfsctl trash` section + the `share edit` trash flags)
- Modify: `docs/CONFIGURATION.md` (per-share trash settings)
- Modify: `docs/ARCHITECTURE.md` (one paragraph: recycle trap in MetadataService + deferred block deletion + reaper)
- Modify: `docs/FAQ.md` (entry: "Is there a recycle bin / can I recover deleted files?")

- [ ] **Step 1: Write the docs.** Describe `#recycle`, the `enable-trash` opt-in + four settings, restore over the mount vs `dfsctl trash`, disable-auto-empty, the single-shared-bin and in-place-overwrite-not-recycled caveats. No phase/plan IDs in docs (repo rule).

- [ ] **Step 2: Commit**

```bash
git add docs/CLI.md docs/CONFIGURATION.md docs/ARCHITECTURE.md docs/FAQ.md
git -c gpg.format=ssh commit -S -m "docs: recycle bin usage, config, architecture, FAQ (#190)"
```

---

## Phase 6 — Gates, PR

### Task 18: Simplifier → reviewer → lint → PR

- [ ] **Step 1:** Run `code-simplifier:code-simplifier` over the full diff (`git diff origin/develop...HEAD`). Apply safe simplifications; re-run unit tests.
- [ ] **Step 2:** Run `feature-dev:code-reviewer` over the diff. Fix HIGH/medium findings (watch specifically for: any lock-free read of mutating share fields; recycle failures that could silently hard-delete; reaper holding a lock across I/O). Re-run gates.
- [ ] **Step 3:** Full gate:

```bash
gofmt -s -w . && go vet ./... && golangci-lint run --timeout=5m
go test -race ./pkg/metadata/... ./pkg/controlplane/runtime/... ./pkg/apiclient/... ./cmd/dfsctl/...
go vet -tags e2e ./test/e2e/...
```
Expected: all clean/PASS.

- [ ] **Step 4:** Push + open PR to `develop`, assignee `marmos91`:

```bash
git push -u origin feat/190-trash-recycle-bin
gh pr create --base develop --assignee marmos91 \
  --title "feat: per-share recycle bin (#190)" \
  --body "Implements the Synology-style #recycle bin per the design spec. Closes #190 on merge (manual close — develop merges don't auto-close).

- Visible #recycle dir per share, opt-in via enable-trash (default off, live-toggleable)
- Recycle on unlink + replace-overwrite; in-place writes not trapped
- Settings: retention-days, restrict-empty-to-admin, max-size, exclude-patterns
- Deferred block deletion (recycled node keeps blocks until reaped/purged)
- Reaper (retention + max-size), disable auto-empties
- dfsctl trash list/restore/empty/status + share edit flags
- Cross-backend conformance (memory/badger/postgres), NFS + SMB e2e

🤖 Generated with [Claude Code](https://claude.com/claude-code)"
```

- [ ] **Step 5:** Wait ~15 min for Copilot + CI; address findings; when green + clean, report back (do not self-merge without confirmation; close #190 on merge).

---

## Self-Review (completed during plan authoring)

- **Spec coverage:** visible #recycle (T3/T15/T16), opt-in live toggle (T9/T10/T11), unlink+overwrite triggers (T3/T4), four settings (T9–T11), single shared bin + path/owner preservation (T3 `recycleNode`), disable auto-empty (T8/T10), guard layer/inRecycle/exclude (T2/T3/T5), reaper retention+max-size (T7), dfsctl all configs (T11/T14), REST+apiclient (T10/T12/T13), cross-backend conformance (T5), NFS+SMB e2e + cross-OS note (T15/T16), error codes (T6/T12), docs (T17), simplifier+reviewer gate (T18). No gaps.
- **Placeholders:** Implementer notes flag the few seams that must bind to existing-but-not-quoted method names (share-root resolution, path-from-handle, block-free call); each names the grep to find the real symbol and bounds the work. No "TBD/handle edge cases" hand-waving.
- **Type consistency:** `TrashConfig{Enabled,ExcludePatterns}` (metadata) vs `trash.Config{Enabled,RetentionDays,RestrictToAdmin,MaxBytes,ExcludePatterns}` (runtime superset) used consistently; `RecycleDirName`, `inRecycle`, `recycleNode`, `TrashConfigForShare`, `SetShareTrashConfig`, `OnDisable`, `reapShareAt`, `evictToCap`, `purgeEntry` referenced with stable signatures across tasks.
