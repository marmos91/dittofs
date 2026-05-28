---
phase: 14-migration-tool-a5
plan: 06
type: execute
wave: 6
depends_on: [14-05-integrity-cutover]
files_modified:
  - cmd/dfsctl/commands/blockstore/migrate_status.go
  - cmd/dfsctl/commands/blockstore/migrate_status_test.go
  - cmd/dfsctl/commands/blockstore/blockstore.go
  - internal/controlplane/api/handlers/migrate_status.go
  - internal/controlplane/api/handlers/migrate_status_test.go
  - pkg/controlplane/api/router.go
  - pkg/controlplane/runtime/runtime.go
  - pkg/controlplane/runtime/share.go
  - pkg/controlplane/runtime/shares/service.go
  - pkg/apiclient/blockstore.go
  - pkg/apiclient/blockstore_test.go
autonomous: true
requirements: [MIG-01, MIG-02]
tags: [status, rest_api, openapi, dfsctl]
must_haves:
  truths:
    - "`dfsctl blockstore migrate status --share NAME` reads {share-data-dir}/.migration-state.jsonl + queries metadata for BlockLayout, prints a human table by default, JSON via `-o json` (D-A16)"
    - "REST endpoint `GET /api/v1/blockstore/migrate/status?share=NAME` returns the same structure as the CLI's JSON output (D-A16) with admin-only JWT auth (matches every other admin endpoint)"
    - "Status fields: share name, BlockLayout (legacy|cas-only), files_total, files_done, files_skipped, last_commit_at, journal_present (bool), snapshot_present (bool), bytes_uploaded, bytes_deduped"
    - "When share=NAME has no journal (never migrated), status returns BlockLayout=legacy or cas-only as appropriate, files_done=0, journal_present=false (NOT an error — this is the steady state for an unmigrated share or a fully-cut-over share whose journal was retained)"
    - "REST handler imports `pkg/blockstore/migrate` (the package created in Plan 14-03) for the journal Replay+aggregate logic — NOT `cmd/dfsctl/commands/blockstore/`. Go forbids pkg/ and internal/ from importing cmd/. (BLOCKER 3 fix.)"
    - "REST handler resolves the metadata store via existing `Runtime.GetMetadataStoreForShare(shareName)` — the actual exported method name. (BLOCKER 2 fix.)"
    - "REST handler resolves the journal directory via a new explicit Runtime accessor `Runtime.LocalStoreDir(shareName) (string, error)` added in this plan. The accessor delegates to the existing shares service which already tracks per-share local-store paths. The pre-revision plan's `LocalStoreDirFor` did not exist. (BLOCKER 2 fix.)"
    - "File total is computed by walking `migrate.WalkShareFiles` (the helper added in Plan 14-03 Task 2). The pre-revision plan's `Runtime.CountFiles()` does not exist. (BLOCKER 2 fix.)"
  artifacts:
    - path: cmd/dfsctl/commands/blockstore/migrate_status.go
      provides: "`migrate status` cobra subcommand"
      contains: "migrateStatusCmd"
    - path: internal/controlplane/api/handlers/migrate_status.go
      provides: "MigrateStatusHandler for /api/v1/blockstore/migrate/status; imports pkg/blockstore/migrate for journal read"
      contains: "MigrateStatusHandler"
    - path: pkg/controlplane/api/router.go
      provides: "Route registration: r.Get(\"/blockstore/migrate/status\", ...)"
      contains: "blockstore/migrate/status"
    - path: pkg/controlplane/runtime/runtime.go
      provides: "New `LocalStoreDir(shareName string) (string, error)` accessor — delegates to the shares service which already tracks per-share local data dirs"
      contains: "LocalStoreDir"
    - path: pkg/apiclient/blockstore.go
      provides: "Client.MigrateStatus(share) → MigrateStatusResponse"
      contains: "MigrateStatus"
  key_links:
    - from: cmd/dfsctl/commands/blockstore/migrate_status.go
      to: pkg/apiclient/blockstore.go
      via: "Client.MigrateStatus call"
      pattern: "MigrateStatus"
    - from: pkg/apiclient/blockstore.go
      to: internal/controlplane/api/handlers/migrate_status.go
      via: "GET /api/v1/blockstore/migrate/status?share=NAME"
      pattern: "/api/v1/blockstore/migrate/status"
    - from: internal/controlplane/api/handlers/migrate_status.go
      to: pkg/blockstore/migrate (Plan 14-03 output)
      via: "OpenJournalReadOnly + Aggregate (read-only) on the runtime's local-store path"
      pattern: "migrate\\.OpenJournalReadOnly"
    - from: internal/controlplane/api/handlers/migrate_status.go
      to: pkg/controlplane/runtime/runtime.go
      via: "GetMetadataStoreForShare + LocalStoreDir (new) — both real exported Runtime methods"
      pattern: "GetMetadataStoreForShare|LocalStoreDir"
---

<objective>
Ship the operator-visible status surface: `dfsctl blockstore migrate status --share NAME` (table + JSON) AND `GET /api/v1/blockstore/migrate/status?share=NAME` REST endpoint with admin auth. Both share the same response shape so dittofs-pro UI consumes the REST contract and operators consume the CLI. (D-A16, MIG-01 / MIG-02 completion.)

Purpose: D-A16 explicitly elects "REST endpoint shipped in Phase 14" — overriding the original "defer to dittofs-pro" recommendation. Operators need a quick post-flight check after running the migration; ops + automation pipelines need the REST contract for monitoring.

**BLOCKER 2 + 3 fixes from review iteration 1:**

1. The pre-revision plan called `rt.LocalStoreDirFor()`, `rt.MetadataStoreFor()`, `rt.CountFiles()` — none of which exist on `pkg/controlplane/runtime.Runtime`. The actual existing method is `Runtime.GetMetadataStoreForShare`. This revision uses the real name + adds an explicit `Runtime.LocalStoreDir(shareName) (string, error)` accessor (with corresponding action and acceptance criterion). FilesTotal is computed via `migrate.WalkShareFiles` (added in Plan 14-03 Task 2), not a non-existent `CountFiles`. (BLOCKER 2.)
2. The pre-revision plan placed the journal type in `cmd/dfsctl/commands/blockstore/`, then asked the REST handler in `internal/` to import it — illegal in Go. The journal now lives in `pkg/blockstore/migrate/journal.go` from day one (Plan 14-03 Task 2). This handler imports from there. (BLOCKER 3.)

Output: Two surfaces (CLI + REST) returning the same MigrateStatusResponse JSON.
</objective>

<execution_context>
@$HOME/.claude/get-shit-done/workflows/execute-plan.md
@$HOME/.claude/get-shit-done/templates/summary.md
</execution_context>

<context>
@.planning/PROJECT.md
@.planning/phases/14-migration-tool-a5/14-CONTEXT.md
@.planning/phases/14-migration-tool-a5/14-03-SUMMARY.md
@.planning/phases/14-migration-tool-a5/14-05-SUMMARY.md
@.planning/codebase/CONVENTIONS.md
@.planning/codebase/STRUCTURE.md

<interfaces>
<!-- Existing patterns to mirror exactly. -->

From cmd/dfsctl/commands/grace/status.go (canonical CLI status command with table+JSON+YAML output):
```go
var statusCmd = &cobra.Command{ Use: "status", ...; RunE: runGraceStatus }
type graceStatusRenderer struct { resp *apiclient.GraceStatusResponse }
func (g graceStatusRenderer) Headers() []string { ... }
func (g graceStatusRenderer) Rows() [][]string { ... }
```

From pkg/apiclient/client.go (the API client base):
```go
type Client struct { ... }
func (c *Client) get(path string, out any) error { ... }
```

From internal/controlplane/api/handlers/grace.go (existing admin handler):
```go
type GraceHandler struct { rt *runtime.Runtime }
func (h *GraceHandler) Status(w http.ResponseWriter, r *http.Request) {
    json.NewEncoder(w).Encode(resp)
}
```

From pkg/controlplane/runtime/runtime.go (verified existing methods):

```go
// REAL methods that exist. The pre-revision plan used names that did NOT exist;
// these are what the runtime actually exposes:

func (r *Runtime) GetMetadataStoreForShare(shareName string) (metadata.MetadataStore, error)  // exists
func (r *Runtime) GetShare(name string) (*Share, error)                                        // exists
func (r *Runtime) ShareExists(name string) bool                                                // exists
func (r *Runtime) GetBlockStoreForHandle(ctx context.Context, handle metadata.FileHandle) (*engine.BlockStore, error)  // exists

// NEW method this plan adds (BLOCKER 2 fix): an explicit accessor
// for the share's local data directory. Delegates to the existing
// shares service which already tracks per-share local-store paths
// (see Runtime.sharesSvc usage in pkg/controlplane/runtime/share.go).
func (r *Runtime) LocalStoreDir(shareName string) (string, error)
```

From pkg/blockstore/migrate (Plan 14-03 output — the journal lives in pkg/, importable):

```go
// In pkg/blockstore/migrate/journal.go:

func OpenJournalReadOnly(dir string) (*Journal, error)
func (j *Journal) Aggregate() (entries []JournalEntry, journalPresent, snapshotPresent bool, lastCommitAt time.Time)
func (j *Journal) Close() error

// In pkg/blockstore/migrate/walk.go:
func WalkShareFiles(ctx context.Context, mds metadata.MetadataStore, shareName string, fn WalkCallback) error
```

From pkg/controlplane/api/router.go lines 224–230 (existing admin-protected `/blockstore` route group — the new endpoint goes here):
```go
r.Route("/blockstore", func(r chi.Router) {
    r.Use(apiMiddleware.RequireAdmin())
    r.Get("/stats", blockStoreHandler.Stats)
    // NEW: r.Get("/migrate/status", migrateStatusHandler.Status)
})
```
</interfaces>
</context>

<tasks>

<task type="auto" tdd="true">
  <name>Task 1: dfsctl blockstore migrate status CLI subcommand</name>
  <files>
    cmd/dfsctl/commands/blockstore/migrate_status.go,
    cmd/dfsctl/commands/blockstore/migrate_status_test.go,
    cmd/dfsctl/commands/blockstore/blockstore.go,
    pkg/apiclient/blockstore.go,
    pkg/apiclient/blockstore_test.go
  </files>
  <read_first>
    - cmd/dfsctl/commands/grace/status.go (full file — copy structure verbatim)
    - cmd/dfsctl/commands/blockstore/blockstore.go (Plan 03 output — register the new subcommand in init())
    - pkg/apiclient/client.go (HTTP client base — extend it with MigrateStatus method)
    - pkg/apiclient/grace.go (existing client method shape — mirror exactly)
  </read_first>
  <behavior>
    - Test 1 — `dfsctl blockstore migrate status --share NAME` (table mode): outputs a key-value table with FIELD column and VALUE column listing share, block_layout, files_total, files_done, files_skipped, journal_present, last_commit_at, bytes_uploaded, bytes_deduped.
    - Test 2 — `dfsctl blockstore migrate status --share NAME -o json`: emits JSON matching the MigrateStatusResponse struct.
    - Test 3 — `--share` is required; missing flag exits non-zero with a clear error.
    - Test 4 — Client.MigrateStatus(share) issues `GET /api/v1/blockstore/migrate/status?share=NAME`, decodes the response into MigrateStatusResponse, returns it.
    - Test 5 — Server returns 404 for an unknown share → CLI prints a clear "share %q not found" message and exits non-zero.
  </behavior>
  <action>
    1. Add `pkg/apiclient/blockstore.go`:
       ```go
       package apiclient

       import (
           "fmt"
           "net/url"
       )

       type MigrateStatusResponse struct {
           Share            string `json:"share"`
           BlockLayout      string `json:"block_layout"`         // "legacy" | "cas-only"
           FilesTotal       int    `json:"files_total"`
           FilesDone        int    `json:"files_done"`
           FilesSkipped     int    `json:"files_skipped"`
           BytesUploaded    uint64 `json:"bytes_uploaded"`
           BytesDeduped     uint64 `json:"bytes_deduped"`
           JournalPresent   bool   `json:"journal_present"`
           SnapshotPresent  bool   `json:"snapshot_present"`
           LastCommitAt     string `json:"last_commit_at,omitempty"` // RFC3339
       }

       // MigrateStatus queries the per-share migration progress.
       // Endpoint: GET /api/v1/blockstore/migrate/status?share=NAME
       func (c *Client) MigrateStatus(share string) (*MigrateStatusResponse, error) {
           if share == "" { return nil, fmt.Errorf("share is required") }
           q := url.Values{}; q.Set("share", share)
           path := "/api/v1/blockstore/migrate/status?" + q.Encode()
           var resp MigrateStatusResponse
           if err := c.get(path, &resp); err != nil { return nil, err }
           return &resp, nil
       }
       ```

       Add `pkg/apiclient/blockstore_test.go` with httptest-server tests for the success and 404 cases.

    2. Add `cmd/dfsctl/commands/blockstore/migrate_status.go`:
       ```go
       package blockstore

       import (
           "fmt"
           "os"

           "github.com/spf13/cobra"

           "github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
           "github.com/marmos91/dittofs/internal/cli/output"
           "github.com/marmos91/dittofs/pkg/apiclient"
       )

       var migrateStatusCmd = &cobra.Command{
           Use:   "status",
           Short: "Show migration progress for a share",
           Long: `Show migration progress for a share.

       Combines the per-share .migration-state.jsonl journal (if present) with
       the share's BlockLayout flag from the metadata store, returning a unified
       view of progress and current state.

       Examples:
         dfsctl blockstore migrate status --share myshare
         dfsctl blockstore migrate status --share myshare -o json
         dfsctl blockstore migrate status --share myshare -o yaml`,
           RunE: runMigrateStatus,
       }

       func init() {
           migrateStatusCmd.Flags().String("share", "", "Share name (required)")
           _ = migrateStatusCmd.MarkFlagRequired("share")
       }

       type migrateStatusRenderer struct { resp *apiclient.MigrateStatusResponse }
       func (r migrateStatusRenderer) Headers() []string { return []string{"FIELD", "VALUE"} }
       func (r migrateStatusRenderer) Rows() [][]string {
           return [][]string{
               {"Share", r.resp.Share},
               {"BlockLayout", r.resp.BlockLayout},
               {"FilesTotal", fmt.Sprintf("%d", r.resp.FilesTotal)},
               {"FilesDone", fmt.Sprintf("%d", r.resp.FilesDone)},
               {"FilesSkipped", fmt.Sprintf("%d", r.resp.FilesSkipped)},
               {"BytesUploaded", fmt.Sprintf("%d", r.resp.BytesUploaded)},
               {"BytesDeduped", fmt.Sprintf("%d", r.resp.BytesDeduped)},
               {"JournalPresent", fmt.Sprintf("%t", r.resp.JournalPresent)},
               {"SnapshotPresent", fmt.Sprintf("%t", r.resp.SnapshotPresent)},
               {"LastCommitAt", r.resp.LastCommitAt},
           }
       }

       func runMigrateStatus(cmd *cobra.Command, args []string) error {
           share, _ := cmd.Flags().GetString("share")
           client, err := cmdutil.GetAuthenticatedClient()
           if err != nil { return err }
           resp, err := client.MigrateStatus(share)
           if err != nil { return err }
           format, err := cmdutil.GetOutputFormatParsed()
           if err != nil { return err }
           switch format {
           case output.FormatJSON: return output.PrintJSON(os.Stdout, resp)
           case output.FormatYAML: return output.PrintYAML(os.Stdout, resp)
           default:               return output.PrintTable(os.Stdout, migrateStatusRenderer{resp: resp})
           }
       }
       ```

    3. Register in `cmd/dfsctl/commands/blockstore/blockstore.go`:
       ```go
       func init() {
           Cmd.AddCommand(migrateCmd)
           migrateCmd.AddCommand(migrateStatusCmd) // makes it `migrate status`
       }
       ```

    4. Add `migrate_status_test.go` with the 5 behaviors. Use httptest.Server to simulate the REST endpoint.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./... &amp;&amp; go test ./cmd/dfsctl/commands/blockstore/ -run 'TestMigrateStatus' -count=1 &amp;&amp; go test ./pkg/apiclient/ -run 'TestMigrateStatus' -count=1 &amp;&amp; go run ./cmd/dfsctl blockstore migrate --help 2&gt;&amp;1 | grep -q status</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'MigrateStatus' pkg/apiclient/blockstore.go` >= 1.
    - `grep -c 'MigrateStatusResponse' pkg/apiclient/blockstore.go` >= 1.
    - `grep -c 'migrateStatusCmd' cmd/dfsctl/commands/blockstore/migrate_status.go` >= 1.
    - `grep -c 'migrateCmd.AddCommand(migrateStatusCmd)' cmd/dfsctl/commands/blockstore/blockstore.go` == 1.
    - `go run ./cmd/dfsctl blockstore migrate --help` lists `status` in Available Commands.
    - All 5 unit tests pass.
  </acceptance_criteria>
  <done>
    CLI subcommand wired and registered as `dfsctl blockstore migrate status --share NAME`; supports table / JSON / YAML; talks to apiclient which talks to the REST endpoint (Task 2).
  </done>
</task>

<task type="auto" tdd="true">
  <name>Task 2: REST endpoint /api/v1/blockstore/migrate/status + Runtime.LocalStoreDir accessor</name>
  <files>
    internal/controlplane/api/handlers/migrate_status.go,
    internal/controlplane/api/handlers/migrate_status_test.go,
    pkg/controlplane/api/router.go,
    pkg/controlplane/runtime/runtime.go,
    pkg/controlplane/runtime/share.go
  </files>
  <read_first>
    - internal/controlplane/api/handlers/grace.go (existing handler — JSON response, error mapping; mirror)
    - pkg/controlplane/api/router.go (route registration; the new endpoint is admin-protected — goes inside the JWTAuth+RequireAdmin group near the existing `/blockstore` route at lines 224–230)
    - pkg/controlplane/runtime/runtime.go (verify the existing methods: `GetMetadataStoreForShare` exists at line 172; `LocalStoreDir` does NOT exist — Task adds it. `MetadataStoreFor`, `LocalStoreDirFor`, and `CountFiles` from the pre-revision plan do NOT exist anywhere)
    - pkg/controlplane/runtime/share.go (find the share record that already tracks the local-store data dir; `LocalStoreDir` delegates to that)
    - pkg/controlplane/runtime/shares (the shares subservice — likely owns the per-share local data path)
    - pkg/blockstore/migrate/journal.go (Plan 14-03 output — the handler reuses OpenJournalReadOnly + Aggregate to build the journal-side fields)
    - pkg/blockstore/migrate/walk.go (Plan 14-03 output — used to compute FilesTotal)
  </read_first>
  <behavior>
    Runtime accessor:
    - Test R1 — `Runtime.LocalStoreDir("foo")` returns the share's local-store data dir as configured at AddShare time; matches what the migration tool's offline runtime would compute for the same share.
    - Test R2 — `Runtime.LocalStoreDir("unknown")` returns `runtime.ErrShareNotFound` (the existing sentinel).

    Handler:
    - Test 1: GET /api/v1/blockstore/migrate/status?share=foo → 200 with JSON { share:"foo", block_layout:"legacy", files_total:N, ..., journal_present:false } when no journal exists.
    - Test 2: with a populated journal at the share's local-store dir → returns files_done == journal entry count, BytesUploaded/Deduped totaled across entries, last_commit_at = max(timestamp), journal_present:true.
    - Test 3: ?share parameter is required; missing returns 400 with `{"error":"share is required"}`.
    - Test 4: unknown share (no metadata record) → 404 with `{"error":"share not found"}`.
    - Test 5: unauthenticated request → 401 (existing JWT middleware handles this; just confirm route is inside the admin group).
    - Test 6: non-admin authenticated request → 403 (existing RequireAdmin middleware).
  </behavior>
  <action>
    **Step 1 — Add `Runtime.LocalStoreDir` accessor (BLOCKER 2 fix).**

    Edit `pkg/controlplane/runtime/runtime.go`. Add a method that delegates to the existing shares service. The shares service already tracks per-share local-store paths because the daemon needs it at AddShare time and at GC time; the new method exposes that read-only.

    ```go
    // LocalStoreDir returns the on-disk data directory for the named share's
    // local block store. Used by the Phase 14 migration status REST handler
    // to locate the per-share .migration-state.jsonl journal.
    //
    // Returns ErrShareNotFound when the share is not registered.
    func (r *Runtime) LocalStoreDir(shareName string) (string, error) {
        return r.sharesSvc.LocalStoreDir(shareName)
    }
    ```

    Add the corresponding method on the shares service in `pkg/controlplane/runtime/share.go` (or wherever the shares subservice lives). The implementation reads from the same per-share record (`*Share` struct) the existing `GetShare` exposes. Find the data-dir field by reading the Share struct definition; if it isn't already there, use the local-store-config path from the share's BlockStore config. Match whatever pkg/blockstore/local/fs uses to compute its base path.

    **Step 2 — Create the handler.** Create `internal/controlplane/api/handlers/migrate_status.go`:

    ```go
    package handlers

    import (
        "context"
        "encoding/json"
        "errors"
        "net/http"
        "time"

        "github.com/marmos91/dittofs/internal/logger"
        "github.com/marmos91/dittofs/pkg/blockstore/migrate" // Plan 14-03 output — pkg, not cmd!
        "github.com/marmos91/dittofs/pkg/controlplane/runtime"
        "github.com/marmos91/dittofs/pkg/metadata"
    )

    type MigrateStatusHandler struct {
        rt *runtime.Runtime
    }

    func NewMigrateStatusHandler(rt *runtime.Runtime) *MigrateStatusHandler {
        return &MigrateStatusHandler{rt: rt}
    }

    type migrateStatusResponse struct {
        Share           string `json:"share"`
        BlockLayout     string `json:"block_layout"`
        FilesTotal      int    `json:"files_total"`
        FilesDone       int    `json:"files_done"`
        FilesSkipped    int    `json:"files_skipped"`
        BytesUploaded   uint64 `json:"bytes_uploaded"`
        BytesDeduped    uint64 `json:"bytes_deduped"`
        JournalPresent  bool   `json:"journal_present"`
        SnapshotPresent bool   `json:"snapshot_present"`
        LastCommitAt    string `json:"last_commit_at,omitempty"`
    }

    func (h *MigrateStatusHandler) Status(w http.ResponseWriter, r *http.Request) {
        share := r.URL.Query().Get("share")
        if share == "" {
            writeJSONError(w, http.StatusBadRequest, "share is required")
            return
        }

        // 1. Read BlockLayout via the REAL existing Runtime method.
        //    BLOCKER 2 fix: pre-revision plan called `MetadataStoreFor` — does not exist.
        //    The actual method is `GetMetadataStoreForShare`.
        ms, err := h.rt.GetMetadataStoreForShare(share)
        if err != nil {
            if errors.Is(err, runtime.ErrShareNotFound) {
                writeJSONError(w, http.StatusNotFound, "share not found")
                return
            }
            writeJSONError(w, http.StatusInternalServerError, err.Error())
            return
        }
        opts, err := ms.GetShareOptions(r.Context(), share)
        if err != nil {
            writeJSONError(w, http.StatusInternalServerError, err.Error())
            return
        }

        resp := migrateStatusResponse{
            Share:       share,
            BlockLayout: string(opts.BlockLayout),
        }

        // 2. Resolve the journal directory via the new explicit
        //    Runtime.LocalStoreDir accessor (Step 1 above; BLOCKER 2 fix —
        //    pre-revision plan called `LocalStoreDirFor`, does not exist).
        journalDir, err := h.rt.LocalStoreDir(share)
        if err != nil {
            // Share has no local store (memory-only test fixture); proceed
            // without journal data.
            logger.Debug("migrate-status: no local store dir", "share", share, "err", err)
        } else if journalDir != "" {
            // 3. Read journal+snapshot in read-only mode.
            j, err := migrate.OpenJournalReadOnly(journalDir)
            if err != nil {
                logger.Warn("migrate-status: journal read failed", "share", share, "err", err)
            } else {
                defer j.Close()
                entries, jPresent, sPresent, lastCommit := j.Aggregate()
                resp.JournalPresent = jPresent
                resp.SnapshotPresent = sPresent
                resp.FilesDone = len(entries)
                if !lastCommit.IsZero() {
                    resp.LastCommitAt = lastCommit.UTC().Format(time.RFC3339)
                }
                for _, e := range entries {
                    resp.BytesUploaded += e.BytesUploaded
                    resp.BytesDeduped += e.BytesDeduped
                }
            }
        }

        // 4. FilesTotal: walk the share via migrate.WalkShareFiles.
        //    BLOCKER 2 fix: pre-revision plan called `Runtime.CountFiles` —
        //    does not exist. Walk the metadata store directly using the
        //    Plan 14-03 helper. Default = include unless ?with_total=false.
        //    Wrap in a 30s context to avoid blocking the API server on
        //    huge shares.
        if r.URL.Query().Get("with_total") != "false" {
            ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
            defer cancel()
            total := 0
            walkErr := migrate.WalkShareFiles(ctx, ms, share,
                func(_ metadata.FileHandle, _ *metadata.File) error {
                    total++
                    return nil
                })
            if walkErr != nil {
                logger.Warn("migrate-status: file walk failed/incomplete",
                    "share", share, "err", walkErr, "partial_total", total)
                resp.FilesTotal = -1 // sentinel for "incomplete"
            } else {
                resp.FilesTotal = total
            }
        }

        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(resp)
    }
    ```

    **Step 3 — Register route.** Edit `pkg/controlplane/api/router.go`. Locate the existing `r.Route("/blockstore", ...)` admin block and add:
    ```go
    r.Route("/blockstore", func(r chi.Router) {
        r.Use(apiMiddleware.RequireAdmin())
        r.Get("/stats", blockStoreHandler.Stats)
        r.Post("/evict", blockStoreHandler.Evict)
        // Phase 14: per-share migration progress (D-A16).
        migrateStatusHandler := handlers.NewMigrateStatusHandler(rt)
        r.Get("/migrate/status", migrateStatusHandler.Status)
    })
    ```

    **Step 4 — Tests.** Add `internal/controlplane/api/handlers/migrate_status_test.go` covering the 6 handler behaviors (R1, R2 covered in `pkg/controlplane/runtime/runtime_test.go` extension). Use the existing memory metadata store fixture from the controlplane tests; mirror `internal/controlplane/api/handlers/grace_test.go` if present.
  </action>
  <verify>
    <automated>cd /Users/marmos91/Projects/dittofs-409 &amp;&amp; go build ./... &amp;&amp; go test ./internal/controlplane/api/handlers/ -run 'TestMigrateStatus' -count=1 &amp;&amp; go test ./pkg/controlplane/runtime/ -run 'TestLocalStoreDir' -count=1 &amp;&amp; go vet ./internal/controlplane/api/... ./pkg/controlplane/runtime/ &amp;&amp; grep -q '/blockstore/migrate/status' pkg/controlplane/api/router.go &amp;&amp; grep -q 'NewMigrateStatusHandler' pkg/controlplane/api/router.go &amp;&amp; grep -q 'GetMetadataStoreForShare' internal/controlplane/api/handlers/migrate_status.go &amp;&amp; grep -q 'LocalStoreDir' internal/controlplane/api/handlers/migrate_status.go &amp;&amp; grep -q 'pkg/blockstore/migrate' internal/controlplane/api/handlers/migrate_status.go &amp;&amp; ! grep -q 'cmd/dfsctl' internal/controlplane/api/handlers/migrate_status.go</automated>
  </verify>
  <acceptance_criteria>
    - `grep -c 'func.*Runtime.*LocalStoreDir' pkg/controlplane/runtime/runtime.go` >= 1 (new accessor lands).
    - `grep -c 'MigrateStatusHandler' internal/controlplane/api/handlers/migrate_status.go` >= 1.
    - `grep -c '/blockstore/migrate/status' pkg/controlplane/api/router.go` >= 1.
    - The endpoint is registered INSIDE the JWTAuth + RequireAdmin group (verify by reading router.go lines around the new registration).
    - `grep -c 'GetMetadataStoreForShare' internal/controlplane/api/handlers/migrate_status.go` >= 1 (uses the REAL existing method, not the pre-revision phantom `MetadataStoreFor`).
    - `grep -c 'h.rt.LocalStoreDir' internal/controlplane/api/handlers/migrate_status.go` >= 1 (uses the new accessor, not the phantom `LocalStoreDirFor`).
    - `grep -c 'migrate\.WalkShareFiles' internal/controlplane/api/handlers/migrate_status.go` >= 1 (replaces the phantom `Runtime.CountFiles`).
    - `grep -c 'pkg/blockstore/migrate' internal/controlplane/api/handlers/migrate_status.go` >= 1 (BLOCKER 3 — handler imports the journal from pkg/, NOT cmd/).
    - `grep -c 'cmd/dfsctl' internal/controlplane/api/handlers/migrate_status.go` == 0 (BLOCKER 3 invariant — handler must not reach into cmd/).
    - All 6 handler tests + 2 Runtime accessor tests pass.
    - `go build ./...` succeeds.
    - `dfsctl blockstore migrate status --share NAME` end-to-end against a memory-backed test server returns matching CLI output.
  </acceptance_criteria>
  <done>
    REST endpoint serves the same JSON shape as the CLI, admin-only auth, 400 on missing share, 404 on unknown share. Uses real existing Runtime methods (`GetMetadataStoreForShare`) plus a new explicit accessor (`LocalStoreDir`). Imports the journal from `pkg/blockstore/migrate` (BLOCKER 3 satisfied — no cmd/ import from internal/).
  </done>
</task>

</tasks>

<threat_model>
## Trust Boundaries

| Boundary | Description |
|----------|-------------|
| Operator (CLI / REST consumer) → handler | Admin-only auth via JWT middleware. |
| Handler → journal file | Read-only open via OpenJournalReadOnly. Concurrent migration run + status query is safe by POSIX file semantics + the read-only open never truncates or rotates. |
| Handler → metadata store | Read-only via GetShareOptions + walk. |

## STRIDE Threat Register

| Threat ID | Category | Component | Disposition | Mitigation Plan |
|-----------|----------|-----------|-------------|-----------------|
| T-14-06-01 | Information disclosure | Status endpoint leaks file counts to non-admin user | mitigate | Endpoint is inside JWTAuth + RequireAdmin route group. Tests 5 + 6 assert 401/403. |
| T-14-06-02 | Tampering | Crafted ?share parameter triggering path traversal in journal-dir lookup | mitigate | Journal dir is computed via `rt.LocalStoreDir(share)` which uses the share's *configured* local-store path from the controlplane DB. The user-supplied ?share is only a lookup key — it cannot influence the path concatenation. |
| T-14-06-03 | DoS | Status query with ?with_total=true on a 100M-file share blocks the API server | mitigate | `?with_total=false` short-circuits the file count; default is true. The walk is wrapped in a 30s context; on timeout, FilesTotal is set to -1 (incomplete sentinel) and the response still ships. Document in OpenAPI / runbook. |
| T-14-06-04 | Tampering | Concurrent migration run truncating the journal under a read | mitigate | OpenJournalReadOnly opens both files O_RDONLY and never truncates or rotates. Replay tolerates the trailing partial line (the journal has the same property by D-A4). |
</threat_model>

<verification>
- CLI: `dfsctl blockstore migrate status --share NAME` works in table / json / yaml mode.
- REST: `GET /api/v1/blockstore/migrate/status?share=NAME` returns 200 with the shared response shape.
- 400 / 404 / 401 / 403 paths covered.
- New `Runtime.LocalStoreDir(name)` accessor lands and is used by the handler.
- Handler imports `pkg/blockstore/migrate` (BLOCKER 3) and uses real Runtime methods (BLOCKER 2).
- `go build ./...`, `go vet ./...`, all listed unit tests green.
</verification>

<success_criteria>
- CLI status subcommand registered as `dfsctl blockstore migrate status`.
- REST endpoint at `/api/v1/blockstore/migrate/status?share=NAME` admin-only.
- Both surface the same MigrateStatusResponse JSON shape.
- Journal type imported from `pkg/blockstore/migrate` — no cmd/ import from internal/.
- Runtime exposes `LocalStoreDir` (added in this plan) + uses existing `GetMetadataStoreForShare` (existing).
</success_criteria>

<output>
Create `.planning/phases/14-migration-tool-a5/14-06-SUMMARY.md` documenting the response shape, the route registration, the new `Runtime.LocalStoreDir` accessor, and the handler's strict pkg/-only import policy (BLOCKER 3 record).
</output>
