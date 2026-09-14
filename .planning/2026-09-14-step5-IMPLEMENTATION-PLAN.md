# Step 5 Block Store Simplification — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development
> (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use
> checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the local/remote block store distinction so a share has one journal (automatic)
and one block store (named, remote, durable).

**Architecture:** The local tier becomes a journal provisioned under one server-level root, so no
per-share local config exists. With one kind of store left, the `kind` discriminator is deleted
from the model, the REST routes and the CLI. Migrations refuse ambiguity rather than guessing,
because the journal holds the only copy of any byte not yet offloaded.

**Tech Stack:** Go 1.x, GORM (SQLite + Postgres), chi router, Cobra CLI, testify-free stdlib tests.

**Spec:** `.planning/2026-09-14-step5-block-store-simplification-SPEC.md` — read it with this plan.
Decisions D11–D23 live in `.planning/2026-09-01-block-dataflow-MASTER-PLAN.md` §8.

## Global Constraints

- Commit messages: concise, imperative. **Never** mention Claude, AI tools, or add
  `Co-Authored-By` lines.
- Sign every commit: `git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S`.
- Code comments describe **behaviour only** — no issue/PR numbers, no plan or decision IDs. The
  sole exception is an existing `ponytail:` marker, which must be preserved when editing nearby.
- `docs/guide/cli.md` is **generated**. Never hand-edit; run `go run ./cmd/gendocs`.
- Rebase, never merge. Work in a worktree under `~/dittofs-worktrees/`.
- Never run `git reset` in a worktree you did not create.
- Verify with exit codes captured on their own line. `cmd | head; echo $?` reports head's status,
  not the command's — this has caused two wrong reports already.
- Base branch: `chore/drop-local-blockstore-config`.

---

## Execution Phases

Tasks within a phase touch disjoint files and may run in parallel. Phases are ordered by
compilation dependency — a later phase will not build until the earlier one lands.

| Phase | Tasks | Parallel? |
|---|---|---|
| A — model + migration foundation | 1, 2, 3 | **No** — all three touch `models/` and `store/gorm.go`. One worker, in order. |
| B — surfaces | 4, 5, 6, 7 | **Yes** — four workers, disjoint trees |
| C — dead code + harness | 8, 9 | **Yes** — two workers |
| D — docs | 10 | **No** — must be last; needs the final CLI shape for `gendocs` |

---

## File Structure

**Phase A**
- `pkg/controlplane/models/share.go` — add `CommitAck`, drop `LocalBlockStoreID`, rename
  `RemoteBlockStoreID` → `BlockStoreID`
- `pkg/controlplane/models/stores.go` — drop `BlockStoreKind`, `Kind`, fix the unique index
- `pkg/controlplane/store/gorm.go` — three pre-AutoMigrate steps
- `pkg/controlplane/store/block_store_collision.go` *(new)* — the D23 collision check
- `pkg/controlplane/store/block.go` — drop the `kind` parameter from four methods

**Phase B**
- `pkg/controlplane/runtime/shares/blockstore_config.go` — journal wiring, eviction ceiling
- `internal/controlplane/api/handlers/block_stores.go`, `shares.go`, `pkg/controlplane/api/router.go`,
  `pkg/apiclient/stores.go`, `shares.go` — REST collapse
- `cmd/dfsctl/commands/store/block/**`, `cmd/dfsctl/commands/share/**` — CLI collapse
- `internal/cli/health/types.go`, `display.go` — single-fetch status

**Phase C**
- The ~10 `HasRemote` branch sites; `test/**`, `.github/workflows/smb-client-compat.yml`

**Phase D**
- `docs/guide/*.md`, `docs/internals/*.md`, `README.md`, regenerated `docs/guide/cli.md`

---

## Phase A — model and migration foundation

### Task 1: Per-share commit acknowledgement

**Files:**
- Modify: `pkg/controlplane/models/share.go`
- Modify: `pkg/controlplane/store/gorm.go` (post-AutoMigrate backfill)
- Test: `pkg/controlplane/store/commit_ack_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: `models.CommitAck` (string type), constants `models.CommitAckJournal = "journal"` and
  `models.CommitAckBlockStore = "block-store"`, field `models.Share.CommitAck CommitAck`.
  Task 4 reads this field; Task 6 renders it.

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
)

// A share that predates the column must come up acknowledging on the journal,
// which is the behaviour it already had. Leaving it empty would make the
// durability promise unreadable.
func TestMigration_CommitAckBackfillsToJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")

	s := openAt(t, path)
	if err := s.DB().Exec(
		`INSERT INTO shares (id, name, metadata_store_id, local_block_store_id, commit_ack)
		 VALUES (?, ?, ?, ?, ?)`,
		"s1", "/legacy", "meta", "local", "",
	).Error; err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2 := openAt(t, path)
	defer func() { _ = s2.Close() }()

	var got string
	if err := s2.DB().Raw("SELECT commit_ack FROM shares WHERE name = ?", "/legacy").Scan(&got).Error; err != nil {
		t.Fatalf("read: %v", err)
	}
	if got != string(models.CommitAckJournal) {
		t.Errorf("commit_ack = %q, want %q", got, models.CommitAckJournal)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./pkg/controlplane/store/ -run CommitAckBackfills`
Expected: FAIL — `models.CommitAckJournal` undefined.

- [ ] **Step 3: Add the type and field**

In `pkg/controlplane/models/share.go`, above the `Share` struct:

```go
// CommitAck selects what an NFS COMMIT or SMB Flush waits for before it
// acknowledges a write.
type CommitAck string

const (
	// CommitAckJournal acknowledges once the write is durable in the share's
	// journal. It survives process and host crash, not device loss.
	CommitAckJournal CommitAck = "journal"

	// CommitAckBlockStore acknowledges only once the data has reached the
	// block store. It survives device loss, at the cost of every commit
	// waiting for an upload.
	CommitAckBlockStore CommitAck = "block-store"
)
```

Add to the `Share` struct beside `JournalSize`:

```go
	CommitAck CommitAck `gorm:"size:16;default:journal" json:"commit_ack"`
```

- [ ] **Step 4: Add the backfill**

In `pkg/controlplane/store/gorm.go`, in the post-AutoMigrate block beside the `enabled` backfill:

```go
	// Post-migration: backfill commit_ack for rows that predate the column.
	// A share acknowledging on nothing is not a weaker promise, it is an
	// unreadable one, and SQLite may leave NULL on ALTER TABLE ADD COLUMN even
	// with a DEFAULT.
	if err := db.Exec(
		"UPDATE shares SET commit_ack = ? WHERE commit_ack IS NULL OR commit_ack = ''",
		string(models.CommitAckJournal),
	).Error; err != nil {
		return nil, fmt.Errorf("failed to backfill commit_ack: %w", err)
	}
```

- [ ] **Step 5: Run the test**

Run: `go test ./pkg/controlplane/store/ -run CommitAckBackfills -v`
Expected: PASS.

- [ ] **Step 6: Confirm the backfill is load-bearing**

Temporarily change the `UPDATE` to `WHERE 1 = 0`, re-run the test, confirm FAIL, then restore.
A migration nobody has seen fail is unverified.

- [ ] **Step 7: Commit**

```bash
git add pkg/controlplane/models/share.go pkg/controlplane/store/gorm.go pkg/controlplane/store/commit_ack_test.go
git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(shares): let a share choose what a commit waits for

NFS COMMIT and SMB Flush acknowledge on journal durability by default. A
share may instead wait until the data reaches the block store, trading
throughput for survival of device loss."
```

---

### Task 2: Refuse a block store name collision, then drop the kind column

**Files:**
- Create: `pkg/controlplane/store/block_store_collision.go`
- Create: `pkg/controlplane/store/block_store_collision_test.go`
- Modify: `pkg/controlplane/models/stores.go`
- Modify: `pkg/controlplane/store/gorm.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `store.ErrBlockStoreNameCollision` (sentinel). `models.BlockStoreKind`,
  `models.BlockStoreKindLocal`, `models.BlockStoreKindRemote` and `BlockStoreConfig.Kind` cease
  to exist — Tasks 4, 5, 6 depend on that.

- [ ] **Step 1: Write the failing test**

```go
package store

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// The old unique index was (name, kind), so a local and a remote store could
// share a name. Dropping kind makes name globally unique and those rows
// collide. Renaming one under the operator breaks their scripts later instead
// of now, and deleting one orphans any share that referenced it.
func TestMigration_RefusesBlockStoreNameCollision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cp.db")

	s := openAt(t, path)
	for _, kind := range []string{"local", "remote"} {
		if err := s.DB().Exec(
			`INSERT INTO block_store_configs (id, name, kind, type, config)
			 VALUES (?, ?, ?, ?, ?)`,
			"id-"+kind, "blocks", kind, "memory", "{}",
		).Error; err != nil {
			t.Fatalf("insert %s: %v", kind, err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err := New(&Config{Type: DatabaseTypeSQLite, SQLite: SQLiteConfig{Path: path}})
	if err == nil {
		t.Fatal("got nil, want a refusal for the colliding rows")
	}
	if !errors.Is(err, ErrBlockStoreNameCollision) {
		t.Fatalf("errors.Is(err, ErrBlockStoreNameCollision) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "blocks") {
		t.Errorf("error must name the colliding store; got %v", err)
	}
}
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `go test ./pkg/controlplane/store/ -run RefusesBlockStoreNameCollision`
Expected: FAIL — `ErrBlockStoreNameCollision` undefined.

- [ ] **Step 3: Write the check**

`pkg/controlplane/store/block_store_collision.go`:

```go
package store

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"gorm.io/gorm"
)

// ErrBlockStoreNameCollision reports two block store rows that shared a name
// under the old (name, kind) uniqueness and cannot both survive it being
// dropped.
var ErrBlockStoreNameCollision = errors.New("two block stores share a name")

// checkBlockStoreNameCollisions refuses an upgrade whose block store names stop
// being unique once the kind column is dropped.
//
// Renaming one row would change a name the operator's scripts use, failing them
// at some later point instead of now; deleting one would silently orphan any
// share that referenced it. Refusing costs a restart and loses nothing.
//
// A database with no kind column has already been migrated and is skipped.
func checkBlockStoreNameCollisions(db *gorm.DB) error {
	if !db.Migrator().HasTable("block_store_configs") {
		return nil
	}
	// No need to probe for the kind column: once it is gone the surviving
	// unique index on name makes duplicates impossible, so the query below
	// returns nothing and the check is a no-op. Probing would mean a
	// dialect-specific catalog query for no gain.

	var names []string
	if err := db.Raw(
		"SELECT name FROM block_store_configs GROUP BY name HAVING COUNT(*) > 1",
	).Scan(&names).Error; err != nil {
		return fmt.Errorf("failed to check block store names: %w", err)
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %q is used by more than one block store\n", n)
	}
	b.WriteString("\nBlock stores no longer have a kind, so their names must be unique.\n")
	b.WriteString("Rename one of each pair with `dfsctl store block edit`, then restart.")
	return fmt.Errorf("%w%s", ErrBlockStoreNameCollision, b.String())
}
```

- [ ] **Step 4: Call it before AutoMigrate**

In `pkg/controlplane/store/gorm.go`, in the pre-AutoMigrate block, **before** any index work:

```go
	// Pre-migration: the (name, kind) uniqueness becomes (name), so two rows
	// that legitimately shared a name must be resolved by the operator first.
	if err := checkBlockStoreNameCollisions(db); err != nil {
		return nil, err
	}
```

- [ ] **Step 5: Run the test**

Run: `go test ./pkg/controlplane/store/ -run RefusesBlockStoreNameCollision -v`
Expected: PASS.

- [ ] **Step 6: Drop the discriminator from the model**

In `pkg/controlplane/models/stores.go`: delete the `BlockStoreKind` type and both constants,
delete the `Kind` field, and change the name index from
`uniqueIndex:idx_block_store_name_kind,priority:1` to `uniqueIndex:idx_block_store_name`. Delete
the standalone index on `Kind`.

- [ ] **Step 7: Drop the column and the stale index**

In `gorm.go`, in the post-AutoMigrate block:

```go
	// Post-migration: drop the kind column and the composite index it anchored.
	// The collision check above guarantees names are already unique.
	_ = db.Exec("DROP INDEX IF EXISTS idx_block_store_name_kind")
	if migrator.HasColumn(&models.BlockStoreConfig{}, "kind") {
		_ = migrator.DropColumn(&models.BlockStoreConfig{}, "kind")
	}
```

- [ ] **Step 8: Fix the legacy bootstrap insert**

`gorm.go:413` hard-codes `INSERT INTO block_store_configs (id, name, kind, type, config) VALUES
(?, 'default-local', 'local', 'fs', '{}', ?)`. It fails once the column is gone. Remove the `kind`
column and value from that statement and rename the row to `default-block-store`.

- [ ] **Step 9: Drop the kind parameter from the store layer**

In `pkg/controlplane/store/block.go`, remove the `kind models.BlockStoreKind` parameter from
`GetBlockStore`, `ListBlockStores`, `DeleteBlockStore` and `GetSharesByBlockStore`, and drop the
`kind = ?` clauses from their queries. Update `interface.go` to match.

- [ ] **Step 10: Verify**

```bash
go build ./... ; echo "BUILD=$?"
go test ./pkg/controlplane/store/ 2>&1 | tail -5
```
Expected: BUILD=0. Compilation failures outside `pkg/controlplane/store` are expected at this
point and are Phase B's work — record them, do not fix them here.

- [ ] **Step 11: Commit**

```bash
git add pkg/controlplane/models/stores.go pkg/controlplane/store/
git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "refactor(store): a block store no longer has a kind

Names become globally unique, so an install holding a local and a remote
store under one name is refused with both rows named rather than having
one renamed or dropped underneath it."
```

---

### Task 3: Drop the local block store reference from a share

**Files:**
- Modify: `pkg/controlplane/models/share.go`
- Modify: `pkg/controlplane/store/shares.go`, `gorm.go`
- Modify: `pkg/controlplane/runtime/shares/blockstore_config.go`, `lifecycle.go`, `service.go`
- Modify: `pkg/controlplane/runtime/init.go`
- Test: `pkg/controlplane/runtime/shares/journal_root_wiring_test.go` (create)

**Interfaces:**
- Consumes: `shares.OpenShareJournal(shareName string, defaults *LocalStoreDefaults)
  (*journal.Store, error)` and `shares.CheckJournalRoot(configured string, found []ShareJournalPath)
  error` — both already exist on the base branch.
- Produces: `models.Share.BlockStoreID` (was `RemoteBlockStoreID`, JSON `block_store_id`),
  `ShareConfig.BlockStoreID`. `LocalBlockStoreID` ceases to exist.

- [ ] **Step 1: Write the failing test**

```go
package shares

import (
	"errors"
	"testing"
)

// Startup must refuse a share whose recorded data location disagrees with the
// configured root rather than opening an empty journal beside it.
func TestStartupRefusesAMisplacedShare(t *testing.T) {
	err := CheckJournalRoot("/srv/blocks", []ShareJournalPath{
		{Share: "/alpha", Path: "/mnt/elsewhere"},
	})
	if !errors.Is(err, ErrJournalRootMismatch) {
		t.Fatalf("got %v, want a wrapped ErrJournalRootMismatch", err)
	}
}
```

- [ ] **Step 2: Run it**

Run: `go test ./pkg/controlplane/runtime/shares/ -run StartupRefusesAMisplacedShare -v`
Expected: PASS (the helper already exists). This test pins the contract Step 3 wires up.

- [ ] **Step 3: Switch share construction to the journal root**

In `createBlockStoreForShare` (`blockstore_config.go`), delete the `resolveBlockStoreConfig` call
for the local store and the `localCfg.Kind` assertion, and replace the constructor call:

```go
	localStore, err := OpenShareJournal(config.Name, effectiveDefaults)
	if err != nil {
		return fmt.Errorf("failed to open share journal: %w", err)
	}
```

Delete `CreateLocalStoreFromConfig`, `deriveLocalStoreDir`, `openJournalStore`'s now-unused
callers, and the `case "fs"` / `case "memory"` switch. Keep `checkLegacyLayout` — it is still
called from `OpenShareJournal` via `openJournalStore`.

- [ ] **Step 4: Wire the refusal into startup**

In `pkg/controlplane/runtime/init.go`, add the sentinel to the fatal matcher at the
`ErrLegacyLocalFormat` site:

```go
			if errors.Is(err, block.ErrFutureFormat) || errors.Is(err, journal.ErrFutureFormat) ||
				errors.Is(err, sharesvc.ErrLegacyLocalFormat) ||
				errors.Is(err, sharesvc.ErrJournalRootMismatch) {
```

Note the package is imported as `sharesvc` there to avoid shadowing a local `shares` variable.

- [ ] **Step 5: Rename the remaining reference**

`Share.RemoteBlockStoreID` → `BlockStoreID`, JSON tag `remote_block_store_id` → `block_store_id`,
and make it non-null (`gorm:"not null;size:36"`). Delete `LocalBlockStoreID` and the
`LocalBlockStore` association. Update `store/shares.go`'s update field-map and every `Preload`.

- [ ] **Step 6: Add the column migration**

In `gorm.go`, pre-AutoMigrate, beside the `journal_size` rename:

```go
	// Pre-migration: rename remote_block_store_id to block_store_id. Must
	// precede AutoMigrate for the same reason as journal_size — otherwise the
	// new column arrives empty and every share loses its store reference.
	if db.Migrator().HasColumn(&models.Share{}, "remote_block_store_id") {
		if db.Migrator().HasColumn(&models.Share{}, "block_store_id") {
			return nil, fmt.Errorf("shares table has both remote_block_store_id and block_store_id; " +
				"copy the intended values into block_store_id, drop the old column, and restart")
		}
		if err := db.Migrator().RenameColumn(&models.Share{}, "remote_block_store_id", "block_store_id"); err != nil {
			return nil, fmt.Errorf("failed to rename remote_block_store_id column: %w", err)
		}
	}
	if migrator := db.Migrator(); migrator.HasColumn(&models.Share{}, "local_block_store_id") {
		_ = migrator.DropColumn(&models.Share{}, "local_block_store_id")
	}
```

- [ ] **Step 7: Verify**

```bash
go build ./... ; echo "BUILD=$?"
go test ./pkg/controlplane/... 2>&1 | tail -10
```
Expected: BUILD=0 within `pkg/controlplane`. `internal/controlplane` and `cmd/dfsctl` failures are
Phase B.

- [ ] **Step 8: Commit**

```bash
git add pkg/controlplane/
git -c user.signingkey=$HOME/.ssh/id_rsa.pub commit -S -m "feat(shares): provision the journal instead of configuring it

A share no longer names a local block store: its journal is opened under
the server-level root. Startup refuses a share whose recorded location
disagrees with that root rather than opening an empty journal beside the
data."
```

---

## Phase B — surfaces (four workers, parallel)

### Task 4: REST route collapse

**Files:** `pkg/controlplane/api/router.go`, `internal/controlplane/api/handlers/block_stores.go`,
`internal/controlplane/api/handlers/shares.go`, `pkg/apiclient/stores.go`, `pkg/apiclient/shares.go`
**Owns:** everything under `internal/controlplane/api/` and `pkg/apiclient/`.

- [ ] **Step 1:** Change `r.Route("/block/{kind}", …)` to `r.Route("/block", …)` in `router.go:352`.
  Update the package doc route list at `:44`.
- [ ] **Step 2:** **Do not remove** the `/shares/{name}/blockstore/…` mounting workaround at
  `:305-308`. Its comment blames the `{kind}` collision, but `/store/block/{name}` collides with
  the same wildcard class. Update the comment to name the real cause; keep the layout.
- [ ] **Step 3:** Delete `extractKind` and the seven `BadRequest(w, "Invalid block store kind…")`
  preambles. Delete `validateBlockStoreType`'s local arm so it accepts `s3` and `memory` only.
- [ ] **Step 4:** Delete `errWrongBlockStoreKind`, drop the `kind` parameter from
  `resolveBlockStoreRef`, and collapse `writeBlockStoreRefError`'s `tier` parameter — its strings
  currently read "Local block store has the wrong kind".
- [ ] **Step 5:** Drop `local_block_store*` JSON fields; rename `remote_block_store_id` →
  `block_store_id` in `CreateShareRequest`, `UpdateShareRequest`, `ShareResponse` and the three
  `apiclient` mirrors. Drop the `kind` parameter from the six `apiclient` methods.
- [ ] **Step 6:** `go build ./... ; echo "BUILD=$?"` then
  `go test ./internal/controlplane/... ./pkg/apiclient/... 2>&1 | tail -10`. Both must be clean.
- [ ] **Step 7:** Commit as `refactor(api): one block store endpoint, no kind segment`.

### Task 5: dfsctl collapse

**Files:** `cmd/dfsctl/commands/store/block/**`, `cmd/dfsctl/commands/share/**`,
`cmd/dfsctl/commands/store/store.go`
**Owns:** everything under `cmd/dfsctl/`.

- [ ] **Step 1:** Move `add.go`, `edit.go`, `list.go`, `remove.go` from
  `store/block/remote/` up to `store/block/`, change their package to `block`, and delete the
  `remote/` directory including `remote.go`. Register the four on `Cmd` in `block.go`.
- [ ] **Step 2:** **Verify the eight kind-agnostic verbs still register and still work:** `stats`,
  `evict`, `health`, `gc`, `gc-status`, `audit-refcounts`, `reconcile`, `reclaim`. A collapse that
  reshapes these is the most likely review miss. Assert by running
  `go run ./cmd/dfsctl store block --help` and checking all twelve subcommands appear.
- [ ] **Step 3:** Delete `--kind` from `health.go` entirely — flag, `MarkFlagRequired`, the
  validation branch, and the four "For local filesystem stores…" help lines. Pass `"remote"`'s
  replacement (no kind) to `client.BlockStoreHealth`.
- [ ] **Step 4:** In `share/create.go`: replace `--local` and `--remote` with a single
  `--block-store`, `MarkFlagRequired` it, collapse the two interactive prompts to one, and rewrite
  the Long help — it currently says "A share requires a metadata store and a local block store."
- [ ] **Step 5:** In `share/edit.go`: same flag change; update the `hasFlags` check and the
  `"no fields specified. Use --local, --remote, …"` error string.
- [ ] **Step 6:** In `share/list.go`: collapse the `LOCAL STORE` / `REMOTE STORE` columns to one
  `BLOCK STORE`, and replace the two `ListBlockStores` round trips with one.
- [ ] **Step 7:** In `share/show.go`: collapse the two block store rows to one. Add a
  `Commit Ack` row rendering `models.CommitAck` — the durability promise must be visible in the
  product, not only in the config reference.
- [ ] **Step 8:** Fix `store/store.go`'s parent help, which lists both kinds.
- [ ] **Step 9:** `go build ./... ; echo "BUILD=$?"` and `go test ./cmd/... 2>&1 | tail -10`.
- [ ] **Step 10:** Commit as `refactor(dfsctl): one block store command tree`.

### Task 6: Single-fetch status

**Files:** `internal/cli/health/types.go`, `internal/cli/health/display.go`
**Owns:** `internal/cli/`.

This is the only site that fails **silently**, so it must land with the route change, not after.

- [ ] **Step 1: Write the failing test** — `internal/cli/health/types_test.go`:

```go
package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A failed fetch must not be indistinguishable from a cluster with no block
// stores. The old code appended both failures to a warning list and gated its
// output on a non-empty result, so total loss of visibility exited zero.
func TestBlockStoreFetchFailureIsReported(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	ents := FetchEntities(srv.Client(), srv.URL, "")
	joined := strings.Join(ents.Errors(), " ")
	if joined == "" {
		t.Fatal("a failed block store fetch must be reported")
	}
	if !strings.Contains(joined, "block stores") {
		t.Errorf("error must name what failed; got %q", joined)
	}
}
```

The entry point is `FetchEntities(client *http.Client, baseURL, token string) Entities`
(`internal/cli/health/types.go:75`). If `Entities` exposes its errors under a different accessor
than `Errors()`, use that one — do not add a new entry point.

- [ ] **Step 2:** Replace the two `doGet` calls with a single `doGet(baseURL+"/store/block", &all)`.
- [ ] **Step 3:** Drop `Kind` from `BlockStoreListItem` and the
  `if b.Kind != "" { label = b.Kind + "/" + b.Name }` rendering in `display.go:58`.
- [ ] **Step 4:** Make a fetch failure distinguishable from an empty list — the caller must be able
  to tell "could not ask" from "nothing configured".
- [ ] **Step 5:** `go test ./internal/cli/... 2>&1 | tail -5`.
- [ ] **Step 6:** Commit as `fix(status): one block store fetch, and a failed one is visible`.

### Task 7: Journal size governs eviction

**Files:** `pkg/controlplane/runtime/shares/blockstore_config.go`, `pkg/config/blockstore.go`
**Owns:** `pkg/config/` and `mergeLocalStoreDefaults`.

- [ ] **Step 1:** Delete `DefaultRemoteCacheSize` from `BlockstoreJournalConfig`, its
  `defaultRemoteCacheSize` const, its `ApplyDefaults` clause, and the
  `LocalStoreDefaults.DefaultRemoteCacheSize` field.
- [ ] **Step 2:** Rewrite `mergeLocalStoreDefaults` so an unset `JournalSize` means **unbounded**
  and a set one is the eviction ceiling. Delete the "10 GiB when a remote is configured, deduced
  otherwise" conditional.
- [ ] **Step 3: Write the test** — assert that an unset size yields no ceiling and a set one is
  passed through verbatim:

```go
func TestMergeDefaults_UnsetJournalSizeIsUnbounded(t *testing.T) {
	got := mergeLocalStoreDefaults(&LocalStoreDefaults{}, &ShareConfig{}, true)
	if got.MaxSize != 0 {
		t.Errorf("MaxSize = %d, want 0 (unbounded) when no journal size is configured", got.MaxSize)
	}
}

func TestMergeDefaults_SetJournalSizeIsTheCeiling(t *testing.T) {
	got := mergeLocalStoreDefaults(&LocalStoreDefaults{}, &ShareConfig{JournalSize: 5 << 30}, true)
	if got.MaxSize != 5<<30 {
		t.Errorf("MaxSize = %d, want the configured 5 GiB", got.MaxSize)
	}
}
```

- [ ] **Step 4:** Confirm the write-backpressure path still runs when the cap is reached with
  nothing offloaded. It is **not** dead code — eviction cannot free a block that is the only copy.
  Add a comment at the backpressure site saying so.
- [ ] **Step 5:** `go test ./pkg/config/ ./pkg/controlplane/runtime/shares/ 2>&1 | tail -5`.
- [ ] **Step 6:** Commit as `refactor(shares): journal size is the eviction ceiling`.

---

## Phase C — dead code and harness (two workers, parallel)

### Task 8: Remove the unreachable local-only branches

**Files:** `pkg/controlplane/runtime/shares/blockstore_ops.go:310`,
`pkg/controlplane/runtime/manifestcheck.go:42`, `pkg/block/engine/stats.go:159,165`,
`pkg/metrics/collectors.go:182`, `pkg/metrics/snapshot.go:55`,
`pkg/controlplane/runtime/offline.go:100`, `pkg/controlplane/runtime/snapshot.go:1676,1689`,
`cmd/dfsctl/commands/store/check.go:53`, `cmd/dfsctl/commands/store/block/warm.go:31`,
`pkg/block/engine/syncer.go:689`

- [ ] **Step 1:** Every share now has a block store, so `HasRemote` is constant. Remove each
  branch and its error string rather than leaving an unreachable arm.
- [ ] **Step 2: Handle the evict safety guard deliberately.**
  `blockstore_ops.go:310` — *"cannot evict local blocks for share %q: no remote store configured
  (data would be lost)"* — exists to prevent data loss and loses its reachable condition. Removing
  it is a decision, not a cleanup. Replace it with an assertion that a block store is present, so
  the invariant that made it safe to delete is stated in code rather than assumed.
- [ ] **Step 3:** `grep -rn "HasRemote\|HasRemoteStore" --include="*.go" pkg internal cmd` and
  confirm every remaining use is a genuine tier statement, not a configuration test.
- [ ] **Step 4:** `go build ./... ; echo "BUILD=$?"` and `go test ./... 2>&1 | tail -20`.
- [ ] **Step 5:** Commit as `refactor: drop the branches for a share with no block store`.

### Task 9: Test harness, scripts and workflow

**Files:** `test/e2e/helpers/stores.go`, `matrix.go`, `blocks.go`, `cas.go`,
`test/posix/setup-posix.sh`, `test/crash/invalidate-cold-loss.sh`, `test/crash/device-loss.sh`,
`test/e2e/testdata/nlm/nlm_axis_interop.sh`, `.github/workflows/smb-client-compat.yml`,
`cmd/gendocs/helpref_test.go`

- [ ] **Step 1:** In `test/e2e/helpers/stores.go`, delete the five `*LocalBlockStore` helpers and
  retarget the remainder at `store block` with no kind. **This file carries 54 block-store
  references through the typed API, not shell-outs — a grep for the CLI spelling will not find
  them.**
- [ ] **Step 2:** `.github/workflows/smb-client-compat.yml:83,213,422` — delete the
  `store block local add` lines; `:86,214,423` — change `share create --local mem-payload` to
  `--block-store mem-payload` and make sure a block store is created first, since one is now
  mandatory.
- [ ] **Step 3:** Update the four shell scripts to `store block add` / `store block remote add`
  → `store block add`.
- [ ] **Step 4:** `cmd/gendocs/helpref_test.go:437` holds a **negative** assertion rejecting
  `dfsctl store block add --kind local`, commented "it is `store block local add`". That rule is
  now backwards. Rewrite the fixture so it tests the same "flag value as a separate token"
  property without asserting the old tree.
- [ ] **Step 5:** `go build ./test/... ; echo "E2E_BUILD=$?"` and `go test ./cmd/gendocs/`.
- [ ] **Step 6:** Commit as `test: one block store across the harness and workflows`.

---

## Phase D — documentation

### Task 10: Follow the change through the guides

**Files:** `docs/guide/{configuration,choosing-stores,durability,faq,getting-started,troubleshooting,encryption,sizing}.md`,
`docs/internals/{architecture,implementing-stores,testing}.md`, `README.md`, `docs/guide/cli.md`

- [ ] **Step 1:** `README.md:182-183` hand-documents both `store block local add` and
  `store block remote add`. Collapse to one.
- [ ] **Step 2:** Document the **commit acknowledgement** flag in `docs/guide/durability.md`: what
  each setting waits for, what each survives, and the throughput cost. This is a durability
  promise an operator cannot infer from behaviour.
- [ ] **Step 3:** Document that an unset `journal_size` means unbounded growth, and that a set one
  evicts **only blocks already offloaded to the block store**. Include the consequence: a cap with
  nothing offloaded is honoured by write backpressure, not eviction.
- [ ] **Step 4:** Add an upgrade section covering all four refusals — divergent journal roots,
  both size columns present, both store-id columns present, and a block store name collision —
  with the exact operator action for each.
- [ ] **Step 5:** Rewrite `docs/internals/implementing-stores.md`, which documents implementing a
  custom **local** store type — a concept that no longer exists.
- [ ] **Step 6:** Regenerate: `go run ./cmd/gendocs ; echo "GENDOCS=$?"`.
- [ ] **Step 7:** `go test ./cmd/gendocs/` — `TestDocsCommandReferences` fails if a guide names a
  command that no longer exists. That is the enforcement mechanism; make it pass.
- [ ] **Step 8:** Commit as `docs: one journal, one block store`.

---

## Final verification

```bash
go build ./...            ; echo "BUILD=$?"
go vet ./...              ; echo "VET=$?"
gofmt -l pkg cmd internal
go test ./... 2>&1 | grep -v "^ok\|no test files" | head -20
go build ./test/...       ; echo "E2E_BUILD=$?"
go run ./cmd/gendocs      ; echo "GENDOCS=$?"
git diff --stat origin/develop..HEAD
git log origin/develop..HEAD --format=%B | grep -icE "claude|co-authored|anthropic"   # must be 0
```

Then confirm each of the four refusals fires, by hand or by test: divergent roots, duplicate size
columns, duplicate store-id columns, colliding store names. **A guard that has never refused
anything is unverified.**
