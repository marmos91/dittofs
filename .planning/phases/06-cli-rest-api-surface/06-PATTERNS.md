# Phase 6: CLI & REST API Surface - Pattern Map

**Mapped:** 2026-04-17
**Files analyzed:** 45 new/modified files
**Analogs found:** 45 / 45 (all have direct in-tree analogs — Phase 6 is purely additive over Phases 1–5)

Phase 6 ships thin adapters over existing runtime primitives. Every new file has a
close analog in `cmd/dfsctl/commands/`, `internal/controlplane/api/handlers/`,
`pkg/apiclient/`, or `pkg/controlplane/store/`. This doc pins the analog per
file and quotes the idioms the plans must copy.

## File Classification

### New CLI commands (per-store backup subtree)

| New File | Role | Data Flow | Closest Analog | Match |
|---|---|---|---|---|
| `cmd/dfsctl/commands/store/metadata/backup/backup.go` | Cobra parent command | dispatch | `cmd/dfsctl/commands/store/metadata/metadata.go` | exact |
| `cmd/dfsctl/commands/store/metadata/backup/run.go` | Cobra run verb (on-demand trigger + `--wait` poll) | async request-response | `cmd/dfsctl/commands/store/metadata/add.go` (create shape) + `health.go` (format dispatch) | role-match |
| `cmd/dfsctl/commands/store/metadata/backup/list.go` | Cobra list verb | read-list | `cmd/dfsctl/commands/store/metadata/list.go` | exact |
| `cmd/dfsctl/commands/store/metadata/backup/show.go` | Cobra show verb (by id) | read-one | `cmd/dfsctl/commands/share/show.go` | exact |
| `cmd/dfsctl/commands/store/metadata/backup/pin.go` | Cobra state-toggle verb | write | `cmd/dfsctl/commands/store/metadata/edit.go` (partial-patch PUT/PATCH) | role-match |
| `cmd/dfsctl/commands/store/metadata/backup/unpin.go` | Cobra state-toggle verb | write | same as `pin.go` | exact |
| `cmd/dfsctl/commands/store/metadata/backup/job/job.go` | Cobra parent command | dispatch | `cmd/dfsctl/commands/store/block/block.go` (nested parent) | exact |
| `cmd/dfsctl/commands/store/metadata/backup/job/list.go` | Cobra list verb (filtered) | read-list | `cmd/dfsctl/commands/store/metadata/list.go` | exact |
| `cmd/dfsctl/commands/store/metadata/backup/job/show.go` | Cobra show verb (grouped sections + progress bar) | read-one | `cmd/dfsctl/commands/share/show.go` (sectioned ShareDetail) | role-match |
| `cmd/dfsctl/commands/store/metadata/backup/job/cancel.go` | Cobra POST-action verb | write-action | `cmd/dfsctl/commands/store/metadata/remove.go` (RunDeleteWithConfirmation scaffold) + `health.go` | role-match |
| `cmd/dfsctl/commands/store/metadata/restore/restore.go` | Cobra run verb with confirm + `--wait` + `--dry-run` | async request-response | `cmd/dfsctl/commands/store/metadata/add.go` + `remove.go` (confirmation) + `run.go` (shared async-poll helper) | role-match |
| `cmd/dfsctl/commands/store/metadata/repo/repo.go` | Cobra parent command | dispatch | `cmd/dfsctl/commands/store/metadata/metadata.go` | exact |
| `cmd/dfsctl/commands/store/metadata/repo/add.go` | Cobra add verb (per-kind interactive prompts) | write | `cmd/dfsctl/commands/store/metadata/add.go` + `cmd/dfsctl/commands/store/block/remote/add.go` | exact |
| `cmd/dfsctl/commands/store/metadata/repo/list.go` | Cobra list verb | read-list | `cmd/dfsctl/commands/store/metadata/list.go` | exact |
| `cmd/dfsctl/commands/store/metadata/repo/show.go` | Cobra show verb | read-one | `cmd/dfsctl/commands/share/show.go` | exact |
| `cmd/dfsctl/commands/store/metadata/repo/edit.go` | Cobra edit verb (partial patch) | write | `cmd/dfsctl/commands/store/metadata/edit.go` | exact |
| `cmd/dfsctl/commands/store/metadata/repo/remove.go` | Cobra remove verb (confirmation + `--purge-archives`) | write | `cmd/dfsctl/commands/store/metadata/remove.go` | exact |

### Modified / restructured CLI (share tree + metadata parent)

| Modified File | Role | Data Flow | Analog / Template | Notes |
|---|---|---|---|---|
| `cmd/dfsctl/commands/store/metadata/metadata.go` | Cobra parent (register backup / restore / repo) | dispatch | same file (self-extend via `Cmd.AddCommand`) | add 3 new subtrees |
| `cmd/dfsctl/commands/share/share.go` | Cobra parent (restructure: root-level list+create, `<name> <verb>` for others) | dispatch | `cmd/dfsctl/commands/store/block/block.go` (nested `Cmd.AddCommand`) | breaking restructure per D-35 |
| `cmd/dfsctl/commands/share/disable.go` | new Cobra verb (block until drained) | write-action | `cmd/dfsctl/commands/share/delete.go` (action + name arg) | D-27, D-36, D-37 |
| `cmd/dfsctl/commands/share/enable.go` | new Cobra verb | write-action | `cmd/dfsctl/commands/share/disable.go` (pair) | D-27, D-38 |
| `cmd/dfsctl/commands/share/show.go` | Cobra show (add ENABLED field) | read-one (modified) | self (append row in `ShareDetail.Rows`) | D-28 |
| `cmd/dfsctl/commands/share/list.go` | Cobra list (add ENABLED column) | read-list (modified) | self (extend `shareRow` + `ShareList` headers/rows) | D-28 |
| `cmd/dfsctl/commands/share/delete.go` / `edit.go` / `mount.go` / `unmount.go` | Cobra verbs re-rooted to `<name> <verb>` | write/read (unchanged) | self (only `Use` field and parent wiring changes) | D-35 breaking flip |

### New REST handlers

| New File | Role | Data Flow | Closest Analog | Match |
|---|---|---|---|---|
| `internal/controlplane/api/handlers/backups.go` | REST resource handler (records + pin/unpin + trigger + restore) | CRUD | `internal/controlplane/api/handlers/metadata_stores.go` | exact |
| `internal/controlplane/api/handlers/backup_jobs.go` | REST resource handler (list/show/cancel) | CRUD (read-heavy) | `internal/controlplane/api/handlers/metadata_stores.go` | exact |
| `internal/controlplane/api/handlers/backup_repos.go` | REST resource handler (CRUD with partial PATCH) | CRUD | `internal/controlplane/api/handlers/metadata_stores.go` + `shares.go` (composite handler store) | exact |

### Modified REST handlers / router

| Modified File | Role | Change | Template |
|---|---|---|---|
| `internal/controlplane/api/handlers/shares.go` | add `Disable` / `Enable` methods | append methods + expose `Enabled` field in `ShareResponse` | self-pattern (existing handler methods) |
| `pkg/controlplane/api/router.go` | wire `/api/v1/store/metadata/{name}/{backups,backup-jobs,restore,repos}` + `/api/v1/shares/{name}/{disable,enable}` | add chi sub-routes under admin middleware | self (existing `/store/metadata` + `/shares` sub-routes) |
| `internal/controlplane/api/handlers/problem.go` | typed extra fields `enabled_shares`, `running_job_id` on conflict problems | extend `Problem` with variants | self (existing `Problem` struct + helper funcs) |

### New apiclient methods (typed client)

| New File | Role | Data Flow | Closest Analog | Match |
|---|---|---|---|---|
| `pkg/apiclient/backups.go` (or split into `backups.go` + `backup_jobs.go` + `backup_repos.go`) | typed REST client + request/response models | request-response | `pkg/apiclient/stores.go` + `pkg/apiclient/shares.go` | exact |

### Modified models / store

| Modified File | Change | Template |
|---|---|---|
| `pkg/controlplane/store/interface.go` (`BackupStore`) | add `ListBackupRecords(ctx, repoID, filter)`, `UpdateBackupJobProgress(ctx, jobID, pct)` | self (existing method shapes) |
| `pkg/controlplane/store/backup.go` | GORM impls of new methods | self (existing `ListBackupJobs`, `SetBackupRecordPinned`, `UpdateBackupJob` impls) |
| `pkg/controlplane/models/share.go` | expose `Enabled` field in JSON response (Phase 6 surfacing — column already exists from Phase 5) | existing `Share` struct tags |
| `pkg/backup/executor/executor.go` | 5 `UpdateBackupJobProgress` call sites at stage boundaries (0/10/50/95/100) | self (existing `UpdateBackupJob` progress=100 finalization at line 257) |
| `pkg/backup/restore/restore.go` | 6 `UpdateBackupJobProgress` call sites (0/10/30/60/95/100) | self (existing `UpdateBackupJob` progress=100 at line 188) |

---

## Pattern Assignments

### `cmd/dfsctl/commands/store/metadata/backup/backup.go` (Cobra parent, dispatch)

**Analog:** `cmd/dfsctl/commands/store/metadata/metadata.go`

**Structure (copy verbatim, substitute verbs):**

```go
// Package backup implements per-store backup management commands.
package backup

import (
	"github.com/marmos91/dittofs/cmd/dfsctl/commands/store/metadata/backup/job"
	"github.com/spf13/cobra"
)

var Cmd = &cobra.Command{
	Use:   "backup",
	Short: "Manage backups for a metadata store",
	Long: `Manage backups for a metadata store on the DittoFS server.

Examples:
  # Trigger on-demand backup
  dfsctl store metadata <name> backup --repo <repo>

  # List backup records
  dfsctl store metadata <name> backup list --repo <repo>

  # Pin / unpin a record
  dfsctl store metadata <name> backup pin <record-id>`,
	RunE: runCmd, // Cobra discretion (D-Claude): parent with no subcommand = trigger run
}

func init() {
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(pinCmd)
	Cmd.AddCommand(unpinCmd)
	Cmd.AddCommand(job.Cmd)
}
```

**Same idiom:** `metadata.go` lines 9-34 — package-level `var Cmd`, `init()` wires children.

---

### `cmd/dfsctl/commands/store/metadata/backup/run.go` (Cobra trigger + async poll)

**Analog:** `cmd/dfsctl/commands/store/metadata/add.go` (POST shape) +
`cmd/dfsctl/commands/store/metadata/health.go` (format switch).

**Imports pattern** (copy from `add.go` lines 1-12):
```go
package backup

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)
```

**Cobra wiring + auth pattern** (from `add.go` lines 22-90):
```go
var runCmd = &cobra.Command{
	Use:   "backup",
	Short: "Trigger an on-demand backup",
	Args:  cobra.ExactArgs(1), // <store-name>
	RunE:  runRun,
}

func init() {
	runCmd.Flags().String("repo", "", "Backup repo name (required when >1 repo attached)")
	runCmd.Flags().Bool("wait", true, "Block until the backup job terminates (D-01)")
	runCmd.Flags().Bool("async", false, "Return immediately with job record (D-01)")
	runCmd.Flags().Duration("timeout", 0, "Wait timeout (0 = indefinite per D-04)")
}

func runRun(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	// ... call client.TriggerBackup(storeName, &req) which returns BackupJob
}
```

**Output format dispatch** (copy from `health.go` lines 36-72):
```go
format, err := cmdutil.GetOutputFormatParsed()
if err != nil {
	return err
}
switch format {
case output.FormatJSON:
	return output.PrintJSON(os.Stdout, resp)
case output.FormatYAML:
	return output.PrintYAML(os.Stdout, resp)
default:
	return printSomeTable(resp)
}
```

**Async poll loop (NEW — no exact analog; use `time.NewTicker` at 1s per D-05):**

```go
// Poll every 1s until terminal state. Planner: factor this into a shared helper
// in cmd/dfsctl/commands/store/metadata/backup/poll.go so restore.go reuses it.
func waitForJob(client *apiclient.Client, storeName, jobID string, timeout time.Duration) (*apiclient.BackupJob, error) {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	var deadline <-chan time.Time
	if timeout > 0 {
		deadline = time.After(timeout)
	}
	// D-08 Ctrl-C / D-03 three-way prompt plumbing: sigCh := make(chan os.Signal, 1); signal.Notify(sigCh, os.Interrupt)
	for {
		select {
		case <-ticker.C:
			job, err := client.GetBackupJob(storeName, jobID)
			if err != nil {
				return nil, err
			}
			if isTerminal(job.Status) {
				return job, nil
			}
			// render spinner (D-02): table mode only, stderr
		case <-deadline:
			return nil, errTimeout // D-11: exit 2 + detach message
		case <-sigCh:
			// D-03: print three-way prompt, handle d|c|C
		}
	}
}
```

**Reference for progress column (D-50):** `pkg/backup/executor/executor.go:257` shows where the executor currently sets `Progress: 100` on success — Phase 6 adds 4 intermediate updates (0/10/50/95) through a new `BackupStore.UpdateBackupJobProgress(ctx, jobID, pct)` method.

---

### `cmd/dfsctl/commands/store/metadata/backup/list.go` (Cobra list)

**Analog:** `cmd/dfsctl/commands/store/metadata/list.go` — exact match.

**Copy this structure verbatim** (lines 1-56 of metadata list.go):
```go
package backup

import (
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var listCmd = &cobra.Command{
	Use:   "list",
	Short: "List backup records",
	Args:  cobra.ExactArgs(1), // <store-name>
	RunE:  runList,
}

// BackupRecordList is a list of backup records for table rendering (D-26).
type BackupRecordList []apiclient.BackupRecord

// Headers implements TableRenderer.
func (bl BackupRecordList) Headers() []string {
	return []string{"ID", "CREATED", "SIZE", "STATUS", "REPO", "PINNED"}
}

// Rows implements TableRenderer.
func (bl BackupRecordList) Rows() [][]string { /* ... */ }

func runList(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	records, err := client.ListBackupRecords(args[0], repoFlag)
	if err != nil {
		return fmt.Errorf("failed to list backup records: %w", err)
	}
	return cmdutil.PrintOutput(os.Stdout, records, len(records) == 0,
		fmt.Sprintf("No backups yet. Run: dfsctl store metadata %s backup [--repo <name>]", args[0]),
		BackupRecordList(records))
}
```

**Short-ULID truncation helper (D-26)** — no existing helper; planner vendors inline:
```go
func shortULID(id string) string {
	if len(id) <= 8 { return id }
	return id[:8] + "…"
}
```

**Relative-time helper (D-26 `3h ago`)** — no existing helper; `internal/cli/timeutil/format.go` only has uptime + RFC3339 formatters. Planner vendors a minimal `timeAgo(t time.Time) string` (only used in table mode).

---

### `cmd/dfsctl/commands/store/metadata/backup/show.go` / `job/show.go` (Cobra show, grouped sections)

**Analog:** `cmd/dfsctl/commands/share/show.go`

**Sectioned TableRenderer** (copy shape from `share/show.go` lines 28-82):
```go
type BackupJobDetail struct {
	job *apiclient.BackupJob
}

func (d BackupJobDetail) Headers() []string { return []string{"FIELD", "VALUE"} }
func (d BackupJobDetail) Rows() [][]string {
	j := d.job
	rows := [][]string{
		{"ID", j.ID},
		{"Kind", string(j.Kind)},
		{"Repo", j.RepoID},
		{"Started", j.StartedAt.Format("2006-01-02 15:04:05")},
	}
	if j.FinishedAt != nil {
		rows = append(rows, []string{"Finished", j.FinishedAt.Format("2006-01-02 15:04:05")})
		rows = append(rows, []string{"Duration", j.FinishedAt.Sub(*j.StartedAt).String()})
	} else {
		rows = append(rows, []string{"Finished", "-"})
	}
	rows = append(rows, []string{"Status", string(j.Status)})
	if j.Status == "running" {
		rows = append(rows, []string{"Progress", renderProgressBar(j.Progress)}) // D-47
	}
	if j.Error != "" {
		rows = append(rows, []string{"Error", j.Error})
	}
	return rows
}

func runShow(cmd *cobra.Command, args []string) error {
	// ... fetch + format + PrintResource like share/show.go:84-108
	format, fmtErr := cmdutil.GetOutputFormatParsed()
	if fmtErr != nil { return fmtErr }
	if format != output.FormatTable {
		return cmdutil.PrintResource(os.Stdout, job, nil)
	}
	return output.PrintTable(os.Stdout, BackupJobDetail{job: job})
}
```

**Progress bar helper (D-47)** — vendor inline; no dep:
```go
func renderProgressBar(pct int) string {
	const w = 20
	filled := pct * w / 100
	return fmt.Sprintf("%d%%  [%s%s]", pct, strings.Repeat("▓", filled), strings.Repeat("░", w-filled))
}
```

---

### `cmd/dfsctl/commands/store/metadata/backup/pin.go` / `unpin.go` (Cobra PATCH verbs)

**Analog:** `cmd/dfsctl/commands/store/metadata/edit.go` (PATCH shape).

**Minimal verb body:**
```go
var pinCmd = &cobra.Command{
	Use:   "pin <record-id>",
	Short: "Mark a backup record as pinned (retention-exempt)",
	Args:  cobra.ExactArgs(2), // <store-name> <record-id>
	RunE:  runPin,
}

func runPin(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil { return err }
	rec, err := client.SetBackupRecordPinned(args[0], args[1], true)
	if err != nil {
		return fmt.Errorf("failed to pin backup record: %w", err)
	}
	return cmdutil.PrintResourceWithSuccess(os.Stdout, rec, fmt.Sprintf("Record '%s' pinned", args[1]))
}
```

`PrintResourceWithSuccess` signature is in `cmd/dfsctl/cmdutil/util.go:157`.

---

### `cmd/dfsctl/commands/store/metadata/backup/job/*` (nested parent + subverbs)

**Parent command analog:** `cmd/dfsctl/commands/store/block/block.go`
— demonstrates the nested-parent-under-nested-parent pattern that Phase 6
reuses for `backup job`.

**Key excerpt (block.go lines 11-40):**
```go
var Cmd = &cobra.Command{
	Use:   "block",
	Short: "Block store management",
}

func init() {
	Cmd.AddCommand(local.Cmd)   // sub-sub parent
	Cmd.AddCommand(remote.Cmd)
	Cmd.AddCommand(statsCmd)
	Cmd.AddCommand(evictCmd)
	Cmd.AddCommand(healthCmd)
}
```

Phase 6 `backup/job/job.go` follows identical structure with `listCmd`, `showCmd`, `cancelCmd`.

---

### `cmd/dfsctl/commands/store/metadata/backup/job/cancel.go` (POST action)

**Analog blend:** `cmd/dfsctl/commands/store/metadata/remove.go` for the argv/auth scaffolding; `cmd/dfsctl/commands/store/metadata/health.go` for the POST → format-dispatch shape.

**Copy pattern (D-44 — no implicit wait for terminal state):**
```go
var cancelCmd = &cobra.Command{
	Use:   "cancel <job-id>",
	Short: "Request cancellation of a running backup/restore job",
	Args:  cobra.ExactArgs(2), // <store-name> <job-id>
	RunE:  runCancel,
}

func runCancel(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil { return err }
	job, err := client.CancelBackupJob(args[0], args[1])
	if err != nil {
		return fmt.Errorf("failed to cancel job: %w", err)
	}
	// D-45: cancel on terminal = 200 OK idempotent. CLI prints next-step hint.
	cmdutil.PrintSuccessWithInfo(
		fmt.Sprintf("Cancel requested for job %s.", args[1]),
		fmt.Sprintf("Poll: dfsctl store metadata %s backup job show %s", args[0], args[1]),
	)
	if cmdutil.GetOutputFormat() == "json" || cmdutil.GetOutputFormat() == "yaml" {
		return cmdutil.PrintResource(os.Stdout, job, nil)
	}
	return nil
}
```

`cmdutil.PrintSuccessWithInfo` signature is in `cmd/dfsctl/cmdutil/util.go:142-152`.

---

### `cmd/dfsctl/commands/store/metadata/restore/restore.go` (Cobra restore + confirm + wait)

**Blended analog:**
- `cmd/dfsctl/commands/share/delete.go` for confirmation-with-force scaffolding
- `cmd/dfsctl/commands/store/metadata/backup/run.go` (new; same session) for async poll
- `cmd/dfsctl/commands/store/metadata/add.go` for flag-plus-prompt shape

**Confirmation pattern (D-30 / D-31)** — from `delete.go` lines 12-48 with
richer prompt body per D-30:
```go
var restoreCmd = &cobra.Command{
	Use:   "restore <store-name>",
	Short: "Restore a metadata store from a backup record",
	Args:  cobra.ExactArgs(1),
	RunE:  runRestore,
}

func init() {
	restoreCmd.Flags().String("from", "", "Record ULID to restore from (26 chars; default = latest succeeded)")
	restoreCmd.Flags().Bool("yes", false, "Skip confirmation prompt (alias -y)")
	restoreCmd.Flags().BoolP("force", "f", false, "Alias for --yes")
	restoreCmd.Flags().Bool("dry-run", false, "Pre-flight only (D-31)")
	restoreCmd.Flags().Duration("timeout", 0, "Wait timeout")
	restoreCmd.Flags().Bool("wait", true, "Block until terminal (D-01)")
	restoreCmd.Flags().Bool("async", false, "Return immediately")
}

func runRestore(cmd *cobra.Command, args []string) error {
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil { return err }

	if !dryRun {
		// D-29 pre-flight: check shares-enabled gate. Server returns 409 on restore call anyway,
		// but CLI can also peek via client.ListEnabledSharesForStore(args[0]) before prompt.
	}

	if !yesFlag {
		confirmed, err := prompt.ConfirmWithForce(renderRestorePromptBody(args[0], fromFlag), false)
		if err != nil {
			return cmdutil.HandleAbort(err)
		}
		if !confirmed {
			fmt.Println("Aborted.")
			return nil
		}
	}
	// POST /api/v1/store/metadata/{name}/restore, then waitForJob (shared helper)
}
```

**`prompt.ConfirmWithForce` reference:** `internal/cli/prompt/confirm.go:76-82`.
**`cmdutil.HandleAbort` reference:** `cmd/dfsctl/cmdutil/util.go:260-266`.

**Prompt body (D-30 verbatim string):** render directly; no library needed.

**Exit codes (D-06):** `os.Exit` from the Cobra `RunE` return. 0 = nil, 1 = failed/interrupted/canceled, 2 = timeout deadline (D-11). Phase 6 planner defines a small typed error in `cmdutil` (e.g., `exitCode int`) wrapped by a `rootCmd.PersistentPostRunE` — OR just uses `os.Exit(2)` directly in restore.go.

---

### `cmd/dfsctl/commands/store/metadata/repo/add.go` (interactive per-kind add)

**Analog:** `cmd/dfsctl/commands/store/metadata/add.go` (prompt switch) + `cmd/dfsctl/commands/store/block/remote/add.go` (S3 kind)

**Imports pattern (copy from `block/remote/add.go` lines 1-12):**
```go
import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/prompt"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)
```

**Flag set pattern — mix local + S3 per D-14 (copy from `block/remote/add.go` lines 14-72):**
```go
var (
	addName            string
	addKind            string // "local" | "s3" (D-14)
	addConfig          string
	addSchedule        string // cron — D-18 server-validated
	addKeepCount       int
	addKeepAgeDays     int
	addEncryption      string // "on"|"off"
	addEncryptionKeyRef string // "env:KEY" / "file:/path" (D-16)
	// Local kind
	addPath string
	// S3 kind
	addBucket, addRegion, addEndpoint, addPrefix, addAccessKey, addSecretKey string
)

func init() {
	addCmd.Flags().StringVar(&addName, "name", "", "Repo name (required)")
	addCmd.Flags().StringVar(&addKind, "kind", "", "Destination kind: local, s3 (required)")
	addCmd.Flags().StringVar(&addSchedule, "schedule", "", "Cron schedule (optional; validated server-side)")
	addCmd.Flags().IntVar(&addKeepCount, "keep-count", 0, "Retention count (D-17)")
	addCmd.Flags().IntVar(&addKeepAgeDays, "keep-age-days", 0, "Retention age in days (D-17)")
	addCmd.Flags().StringVar(&addEncryption, "encryption", "", "Encryption: on|off (D-16)")
	addCmd.Flags().StringVar(&addEncryptionKeyRef, "encryption-key-ref", "", "env:VAR or file:/path (D-16)")
	// local
	addCmd.Flags().StringVar(&addPath, "path", "", "Local filesystem path (for kind=local)")
	// s3
	addCmd.Flags().StringVar(&addBucket, "bucket", "", "S3 bucket (for kind=s3)")
	// ... (copy remaining S3 flags from block/remote/add.go)
	_ = addCmd.MarkFlagRequired("name")
	_ = addCmd.MarkFlagRequired("kind")
}
```

**Interactive prompt switch (copy from `metadata/add.go:102-161`):**
```go
func buildRepoConfig(kind, jsonConfig, path, bucket, region, endpoint, prefix, accessKey, secretKey string) (map[string]any, error) {
	if jsonConfig != "" {
		var cfg map[string]any
		if err := json.Unmarshal([]byte(jsonConfig), &cfg); err != nil {
			return nil, fmt.Errorf("invalid JSON config: %w", err)
		}
		return cfg, nil
	}
	switch kind {
	case "local":
		p := path
		if p == "" {
			var err error
			p, err = prompt.InputRequired("Local backup path")
			if err != nil { return nil, err }
		}
		return map[string]any{"path": p}, nil
	case "s3":
		// Copy S3 prompt flow verbatim from block/remote/add.go:112-170.
		// D-15 research flag: confirm whether access_key_id/secret_access_key
		// belong in backup_repos.config or fall back to ambient AWS chain.
	default:
		return nil, fmt.Errorf("unknown kind: %s (supported: local, s3)", kind)
	}
}
```

**D-15 research flag:** Planner must check `pkg/blockstore/remote/s3` S3
config shape and decide whether `backup_repos.config` stores credentials or
relies on ambient AWS chain. The `block/remote/add.go:143-156` pattern of
prompting for explicit `access_key_id` / `secret_access_key` is the
conservative baseline — mirror it unless the researcher documents otherwise.

---

### `cmd/dfsctl/commands/share/disable.go` / `enable.go` (breaking restructure + new verbs)

**Analog:** `cmd/dfsctl/commands/share/delete.go` (action-with-name arg).

**Copy the whole shape, swap the verb:**
```go
package share

import (
	"fmt"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/spf13/cobra"
)

var disableCmd = &cobra.Command{
	Use:   "disable", // D-35: registered under parent that accepts <name> as positional
	Short: "Disable a share (drain clients, set Enabled=false)",
	Args:  cobra.ExactArgs(1), // <name> — D-35 restructure: name is argv[0]
	RunE:  runDisable,
}

func runDisable(cmd *cobra.Command, args []string) error {
	name := args[0]
	client, err := cmdutil.GetAuthenticatedClient()
	if err != nil {
		return err
	}
	share, err := client.DisableShare(name)
	if err != nil {
		return fmt.Errorf("failed to disable share: %w", err)
	}
	return cmdutil.PrintResourceWithSuccess(os.Stdout, share, fmt.Sprintf("Share %s disabled.", name))
}
```

**`share.go` restructure (copy from block.go:34-40 structure, adapt to share verbs):**
```go
func init() {
	Cmd.AddCommand(listCmd)            // root-level (D-35)
	Cmd.AddCommand(createCmd)          // root-level (D-35)
	Cmd.AddCommand(permission.Cmd)     // root-level (nested sub-parent)
	Cmd.AddCommand(listMountsCmd)      // root-level (mounts)
	// Restructured verbs all take <name> as argv[0]:
	Cmd.AddCommand(showCmd)            // dfsctl share <name> show
	Cmd.AddCommand(editCmd)            // dfsctl share <name> edit
	Cmd.AddCommand(deleteCmd)          // dfsctl share <name> delete
	Cmd.AddCommand(mountCmd)           // dfsctl share <name> mount
	Cmd.AddCommand(unmountCmd)         // dfsctl share <name> unmount
	Cmd.AddCommand(disableCmd)         // dfsctl share <name> disable
	Cmd.AddCommand(enableCmd)          // dfsctl share <name> enable
}
```

**Note:** Cobra doesn't natively support positional-argument-then-verb. Planner must either (a) use a custom `Args` validator per leaf verb that accepts `<name>` as `args[0]`, or (b) restructure via `Use: "<name> show"` + `Aliases`. Conservative choice: each leaf verb has `Args: cobra.ExactArgs(1)` where arg[0] is `<name>`; the parent command accepts `<name>` transparently through Cobra's default sub-routing — **matches `delete.go` pattern already in-tree.**

---

### `internal/controlplane/api/handlers/backups.go` / `backup_jobs.go` / `backup_repos.go`

**Analog:** `internal/controlplane/api/handlers/metadata_stores.go` (full-CRUD resource handler) + `shares.go` (composite `ShareHandlerStore` pattern).

**Imports pattern** (copy from `metadata_stores.go` lines 1-17):
```go
package handlers

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/storebackups"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
)
```

**Composite handler store (copy shape from `shares.go` lines 44-62):**
```go
// BackupHandlerStore is the composite interface BackupHandler needs.
type BackupHandlerStore interface {
	store.BackupStore
	store.MetadataStoreConfigStore // resolve {name} → target_id
}

type BackupHandler struct {
	store   BackupHandlerStore
	runtime *runtime.Runtime
	svc     *storebackups.Service // RunBackup / RunRestore / Validate... entrypoints
}

func NewBackupHandler(s BackupHandlerStore, rt *runtime.Runtime, svc *storebackups.Service) *BackupHandler {
	return &BackupHandler{store: s, runtime: rt, svc: svc}
}
```

**Request/response types (copy shape from `metadata_stores.go` lines 30-53):**
```go
// TriggerBackupRequest is POST /api/v1/store/metadata/{name}/backups.
type TriggerBackupRequest struct {
	Repo string `json:"repo,omitempty"` // required when store has >1 repo (D-24)
}

// BackupRecordResponse mirrors models.BackupRecord (fields align with DB columns).
type BackupRecordResponse struct {
	ID           string    `json:"id"`
	RepoID       string    `json:"repo_id"`
	CreatedAt    time.Time `json:"created_at"`
	SizeBytes    int64     `json:"size_bytes"`
	Status       string    `json:"status"`
	Pinned       bool      `json:"pinned"`
	ManifestPath string    `json:"manifest_path"`
	SHA256       string    `json:"sha256"`
	StoreID      string    `json:"store_id"`
	Error        string    `json:"error,omitempty"`
}
```

**Core handler body (copy shape from `metadata_stores.go` lines 58-123):**
```go
func (h *BackupHandler) TriggerBackup(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		BadRequest(w, "Store name is required")
		return
	}

	var req TriggerBackupRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}

	// Resolve store → target_id
	storeCfg, err := h.store.GetMetadataStore(r.Context(), name)
	if err != nil {
		if errors.Is(err, models.ErrStoreNotFound) {
			NotFound(w, "Metadata store not found")
			return
		}
		InternalServerError(w, "Failed to resolve metadata store")
		return
	}

	// Resolve repo (D-24: required if >1)
	repos, err := h.store.ListReposByTarget(r.Context(), "metadata", storeCfg.ID)
	if err != nil {
		InternalServerError(w, "Failed to list repos")
		return
	}
	repoID, problem := resolveRepoID(req.Repo, repos) // D-24 multi-repo enforcement helper
	if problem != nil {
		writeProblem(w, problem)
		return
	}

	rec, err := h.svc.RunBackup(r.Context(), repoID)
	if err != nil {
		// D-13: map ErrBackupAlreadyRunning → 409 with running_job_id
		if errors.Is(err, storebackups.ErrBackupAlreadyRunning) {
			WriteBackupAlreadyRunningProblem(w, /* running_job_id */)
			return
		}
		if errors.Is(err, storebackups.ErrRepoNotFound) {
			NotFound(w, "Backup repo not found")
			return
		}
		InternalServerError(w, "Failed to run backup: "+err.Error())
		return
	}
	WriteJSONCreated(w, backupRecordToResponse(rec))
}
```

**Error handling (copy from `metadata_stores.go` lines 255-284 — sentinel → HTTP status mapping):**

Refer to `pkg/controlplane/runtime/storebackups/errors.go` for the full Phase-5/6 sentinel taxonomy. Mapping table:

| Sentinel | HTTP | Handler emits |
|---|---|---|
| `ErrBackupAlreadyRunning` | 409 | typed problem with `running_job_id` (D-13) |
| `ErrRepoNotFound` / `ErrBackupRepoNotFound` | 404 | `NotFound(w, ...)` |
| `ErrRestorePreconditionFailed` | 409 | typed problem with `enabled_shares[]` (D-29) |
| `ErrStoreIDMismatch` / `ErrStoreKindMismatch` | 400 | `BadRequest(w, ...)` |
| `ErrNoRestoreCandidate` | 409 | `Conflict(w, ...)` |
| `ErrRecordNotRestorable` / `ErrRecordRepoMismatch` | 409/400 | `Conflict`/`BadRequest` |
| `ErrScheduleInvalid` | 400 | `BadRequest(w, ...)` |

---

### `internal/controlplane/api/handlers/problem.go` (extend with typed fields)

**Analog:** self — existing `Problem` struct at `problem.go:11-27`.

**Current shape (lines 9-44) — copy and extend with sibling types:**
```go
type Problem struct {
	Type     string `json:"type,omitempty"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}
```

**Phase 6 adds typed variants (D-46):**
```go
// BackupAlreadyRunningProblem carries the conflicting running job ID (D-13).
type BackupAlreadyRunningProblem struct {
	Problem
	RunningJobID string `json:"running_job_id"`
}

// RestorePreconditionFailedProblem carries the list of shares blocking restore (D-29).
type RestorePreconditionFailedProblem struct {
	Problem
	EnabledShares []string `json:"enabled_shares"`
}

// WriteBackupAlreadyRunningProblem writes a typed 409 problem.
func WriteBackupAlreadyRunningProblem(w http.ResponseWriter, runningJobID string) {
	p := &BackupAlreadyRunningProblem{
		Problem: Problem{
			Type:   "about:blank",
			Title:  "Conflict",
			Status: http.StatusConflict,
			Detail: "backup already running",
		},
		RunningJobID: runningJobID,
	}
	w.Header().Set("Content-Type", ContentTypeProblemJSON)
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(p)
}
```

---

### `internal/controlplane/api/handlers/shares.go` — add Disable / Enable

**Analog:** existing handler methods in the same file (e.g., `Delete` at lines 549-577).

**New methods — copy existing `Delete` shape and call runtime `DisableShare`/`EnableShare`:**
```go
// Disable handles POST /api/v1/shares/{name}/disable. Admin only.
func (h *ShareHandler) Disable(w http.ResponseWriter, r *http.Request) {
	name := normalizeShareName(chi.URLParam(r, "name"))
	if name == "/" {
		BadRequest(w, "Share name is required")
		return
	}
	if h.runtime == nil {
		InternalServerError(w, "runtime not initialized")
		return
	}
	if err := h.runtime.ShareService().DisableShare(r.Context(), h.store, name); err != nil {
		if errors.Is(err, shares.ErrShareNotFound) {
			NotFound(w, "Share not found")
			return
		}
		InternalServerError(w, "Failed to disable share: "+err.Error())
		return
	}
	// Fetch updated record and return
	updated, err := h.store.GetShare(r.Context(), name)
	if err != nil {
		InternalServerError(w, "Failed to reload share")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), HealthCheckTimeout)
	defer cancel()
	WriteJSONOK(w, h.shareToResponseWithUsage(ctx, updated))
}
```

**Exposing `Enabled` in `ShareResponse` (D-28):** Add `Enabled bool json:"enabled"` to `ShareResponse` (shares.go:98-124) and wire in `shareToResponse` (shares.go:815-849).

---

### `pkg/controlplane/api/router.go` — wire new routes

**Analog:** existing `/store/metadata` sub-route at lines 231-240.

**New sub-routes (insert under existing `/store/metadata` at the same indentation):**
```go
r.Route("/metadata", func(r chi.Router) {
	metadataStoreHandler := handlers.NewMetadataStoreHandler(cpStore, rt)
	r.Post("/", metadataStoreHandler.Create)
	r.Get("/", metadataStoreHandler.List)
	r.Get("/{name}", metadataStoreHandler.Get)
	r.Put("/{name}", metadataStoreHandler.Update)
	r.Delete("/{name}", metadataStoreHandler.Delete)
	r.Get("/{name}/health", metadataStoreHandler.HealthCheck)
	r.Get("/{name}/status", metadataStoreHandler.Status)

	// Phase 6 additions
	backupHandler := handlers.NewBackupHandler(cpStore, rt, rt.StoreBackups())
	r.Route("/{name}/backups", func(r chi.Router) {
		r.Post("/", backupHandler.TriggerBackup)        // D-13
		r.Get("/", backupHandler.ListRecords)           // D-26
		r.Get("/{id}", backupHandler.ShowRecord)        // D-48
		r.Patch("/{id}", backupHandler.PatchRecord)     // D-23 pin/unpin
	})
	r.Route("/{name}/backup-jobs", func(r chi.Router) {
		r.Get("/", backupHandler.ListJobs)              // D-42
		r.Get("/{id}", backupHandler.GetJob)            // polling
		r.Post("/{id}/cancel", backupHandler.CancelJob) // D-43
	})
	r.Post("/{name}/restore", backupHandler.Restore)    // D-29
	r.Route("/{name}/repos", func(r chi.Router) {
		r.Post("/", backupHandler.CreateRepo)
		r.Get("/", backupHandler.ListRepos)
		r.Get("/{repo}", backupHandler.GetRepo)
		r.Patch("/{repo}", backupHandler.PatchRepo)    // D-19 partial
		r.Delete("/{repo}", backupHandler.DeleteRepo)  // D-21 --purge-archives carried in query param
	})
})
```

**Share disable/enable** — same pattern, added inside the existing `r.Route("/shares", ...)` block (router.go:183-204):
```go
r.Post("/{name}/disable", shareHandler.Disable) // D-27
r.Post("/{name}/enable", shareHandler.Enable)   // D-27
```

All new routes inherit `RequireAdmin()` middleware from the enclosing `r.Use(apiMiddleware.RequireAdmin())` at router.go:184/216 (D-32).

---

### `pkg/apiclient/backups.go` (new typed methods)

**Analog:** `pkg/apiclient/stores.go` + `pkg/apiclient/shares.go`.

**Imports (copy from stores.go lines 1-7):**
```go
package apiclient

import (
	"encoding/json"
	"fmt"
	"time"
)
```

**Model types (copy shape from stores.go lines 9-26):**
```go
type BackupRepo struct {
	ID                string          `json:"id"`
	TargetID          string          `json:"target_id"`
	TargetKind        string          `json:"target_kind"`
	Name              string          `json:"name"`
	Kind              string          `json:"kind"`
	Schedule          *string         `json:"schedule,omitempty"`
	KeepCount         *int            `json:"keep_count,omitempty"`
	KeepAgeDays       *int            `json:"keep_age_days,omitempty"`
	EncryptionEnabled bool            `json:"encryption_enabled"`
	EncryptionKeyRef  string          `json:"encryption_key_ref,omitempty"`
	Config            json.RawMessage `json:"config,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
	UpdatedAt         time.Time       `json:"updated_at"`
}

type BackupRecord struct {
	ID           string    `json:"id"`
	RepoID       string    `json:"repo_id"`
	CreatedAt    time.Time `json:"created_at"`
	SizeBytes    int64     `json:"size_bytes"`
	Status       string    `json:"status"`
	Pinned       bool      `json:"pinned"`
	ManifestPath string    `json:"manifest_path"`
	SHA256       string    `json:"sha256"`
	StoreID      string    `json:"store_id"`
	Error        string    `json:"error,omitempty"`
}

type BackupJob struct {
	ID             string     `json:"id"`
	Kind           string     `json:"kind"`
	RepoID         string     `json:"repo_id"`
	BackupRecordID *string    `json:"backup_record_id,omitempty"`
	Status         string     `json:"status"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	Error          string     `json:"error,omitempty"`
	Progress       int        `json:"progress"`
}
```

**Generic helper usage (copy from stores.go — shows `listResources[T]` / `getResource[T]` pattern):**
```go
func (c *Client) ListBackupRecords(storeName, repo string) ([]BackupRecord, error) {
	path := fmt.Sprintf("/api/v1/store/metadata/%s/backups", storeName)
	if repo != "" {
		path += "?repo=" + url.QueryEscape(repo)
	}
	return listResources[BackupRecord](c, path)
}

func (c *Client) GetBackupJob(storeName, jobID string) (*BackupJob, error) {
	return getResource[BackupJob](c, fmt.Sprintf("/api/v1/store/metadata/%s/backup-jobs/%s", storeName, jobID))
}

func (c *Client) TriggerBackup(storeName string, req *TriggerBackupRequest) (*BackupRecord, error) {
	var rec BackupRecord
	if err := c.post(fmt.Sprintf("/api/v1/store/metadata/%s/backups", storeName), req, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (c *Client) SetBackupRecordPinned(storeName, recordID string, pinned bool) (*BackupRecord, error) {
	body := map[string]bool{"pinned": pinned}
	var rec BackupRecord
	if err := c.patch(fmt.Sprintf("/api/v1/store/metadata/%s/backups/%s", storeName, recordID), body, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

func (c *Client) CancelBackupJob(storeName, jobID string) (*BackupJob, error) {
	var job BackupJob
	if err := c.post(fmt.Sprintf("/api/v1/store/metadata/%s/backup-jobs/%s/cancel", storeName, jobID), nil, &job); err != nil {
		return nil, err
	}
	return &job, nil
}
```

**`c.patch` helper reference:** `pkg/apiclient/client.go:125-127` — already supports PATCH.

**Share disable/enable (sibling pattern in `pkg/apiclient/shares.go`):**
```go
func (c *Client) DisableShare(name string) (*Share, error) {
	var share Share
	if err := c.post(fmt.Sprintf("/api/v1/shares/%s/disable",
		url.PathEscape(normalizeShareNameForAPI(name))), nil, &share); err != nil {
		return nil, err
	}
	return &share, nil
}

func (c *Client) EnableShare(name string) (*Share, error) {
	var share Share
	if err := c.post(fmt.Sprintf("/api/v1/shares/%s/enable",
		url.PathEscape(normalizeShareNameForAPI(name))), nil, &share); err != nil {
		return nil, err
	}
	return &share, nil
}
```

---

### `pkg/controlplane/store/interface.go` / `backup.go` — new store methods

**Analog:** existing `UpdateBackupJob` (backup.go:265-284) + `SetBackupRecordPinned` (backup.go:221-233).

**New interface methods (add to `BackupStore` in interface.go:351-458):**
```go
// ListBackupRecords returns records for a repo, optionally filtered by status.
// Matches the listing contract of ListBackupJobs; default sort: newest-first.
ListBackupRecords(ctx context.Context, repoID string, statusFilter models.BackupStatus) ([]*models.BackupRecord, error)

// UpdateBackupJobProgress updates a job's Progress column. Best-effort: update
// failures are logged at WARN by callers but do NOT fail the parent op (D-50).
UpdateBackupJobProgress(ctx context.Context, jobID string, pct int) error
```

**Implementation pattern (copy shape from backup.go:221-233 `SetBackupRecordPinned`):**
```go
func (s *GORMStore) UpdateBackupJobProgress(ctx context.Context, jobID string, pct int) error {
	result := s.db.WithContext(ctx).
		Model(&models.BackupJob{}).
		Where("id = ?", jobID).
		Update("progress", pct)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return models.ErrBackupJobNotFound
	}
	return nil
}

func (s *GORMStore) ListBackupRecords(ctx context.Context, repoID string, statusFilter models.BackupStatus) ([]*models.BackupRecord, error) {
	q := s.db.WithContext(ctx).Where("repo_id = ?", repoID)
	if statusFilter != "" {
		q = q.Where("status = ?", statusFilter)
	}
	var results []*models.BackupRecord
	if err := q.Order("created_at DESC").Find(&results).Error; err != nil {
		return nil, err
	}
	return results, nil
}
```

---

### `pkg/backup/executor/executor.go` — progress call sites (D-50)

**Analog:** existing `UpdateBackupJob(..., Progress: 100)` at line 257.

**Stage marker pattern:**
```go
// Phase-6 D-50: 5 progress milestones. Best-effort; log-and-continue on error.
updateProgress := func(pct int) {
	if err := e.store.UpdateBackupJobProgress(ctx, jobID, pct); err != nil {
		logger.Warn("Failed to update backup job progress", "job_id", jobID, "pct", pct, "error", err)
	}
}

// After CreateBackupJob (line ~103):
updateProgress(0)

// After destination ready (before Step 3 — manifest skeleton, line ~117):
updateProgress(10)

// Immediately before dst.PutBackup (line ~156):
updateProgress(50)

// After record persist, before finalize-job update (line ~249):
updateProgress(95)
// Final 100 is already set by the existing finalize UpdateBackupJob (line 257).
```

**Note:** `JobStore` interface (executor.go:22-26) must be extended to include `UpdateBackupJobProgress`. Planner updates the interface, the test fakes, and adds a no-op stub for any mock implementation.

---

### `pkg/backup/restore/restore.go` — progress call sites (D-50)

Exact same pattern as executor. Six markers, matching Phase 5 D-05 stages:

| Stage | After Line | Pct |
|---|---|---|
| `CreateBackupJob` done | ~161 | 0 |
| Manifest fetched + validated | ~245 | 10 |
| `OpenFreshEngineAtTemp` done | ~251 | 30 |
| Payload streamed + SHA verified | ~299 | 60 |
| Swap committed | ~307 | 95 |
| Finalize (existing) | 188-192 | 100 |

---

## Shared Patterns

### Authentication (CLI)

**Source:** `cmd/dfsctl/cmdutil/util.go:31-86` — `GetAuthenticatedClient()`.

**Apply to:** Every new Cobra command's `RunE`.

```go
client, err := cmdutil.GetAuthenticatedClient()
if err != nil {
	return err
}
```

### Authorization (REST)

**Source:** `pkg/controlplane/api/router.go:184, 216, 261` + `internal/controlplane/api/middleware/auth.go:78-80` (`RequireAdmin`).

**Apply to:** Every new REST route — inherit from enclosing admin route group. D-32 confirms admin-only for all Phase 6 writes.

```go
r.Use(apiMiddleware.RequireAdmin()) // already applied on parent route groups
```

### Error handling (REST)

**Source:** `internal/controlplane/api/handlers/problem.go` — `BadRequest`, `NotFound`, `Conflict`, `InternalServerError`, `WriteProblem`.

**Apply to:** Every handler. Phase 6 extends with typed problem variants (see `problem.go` section above).

**Sentinel taxonomy:** `pkg/controlplane/runtime/storebackups/errors.go` —
every Phase-4/5 sentinel that Phase 6 handlers need to map to HTTP statuses
is re-exported there.

### Output format dispatch (CLI)

**Source:** `cmd/dfsctl/cmdutil/util.go:110-189` — `PrintOutput`, `PrintResource`, `PrintResourceWithSuccess`.

**Apply to:** Every list/show/create/update Cobra verb.

| Intent | Helper | File line |
|---|---|---|
| List (with empty hint) | `PrintOutput` | util.go:110-128 |
| Show one | `PrintResource` | util.go:176-190 |
| Create/update with success | `PrintResourceWithSuccess` | util.go:157-172 |
| Success + info hint (used by `cancel`, `disable`) | `PrintSuccessWithInfo` | util.go:142-152 |

### Confirmation prompts (CLI)

**Source:** `cmd/dfsctl/cmdutil/util.go:193-213` — `RunDeleteWithConfirmation`
and `internal/cli/prompt/confirm.go:76-82` — `ConfirmWithForce`.

**Apply to:** `repo remove`, `restore` (D-30), any destructive new verb.

```go
return cmdutil.RunDeleteWithConfirmation("Backup repo", name, forceFlag, func() error {
	if err := client.DeleteBackupRepo(storeName, name, purgeArchives); err != nil {
		return fmt.Errorf("failed to delete repo: %w", err)
	}
	return nil
})
```

**Custom prompt body (D-30 restore):** compose via `prompt.ConfirmWithForce(customBody, forceFlag)` — `ConfirmWithForce` accepts any label string.

### Interactive prompts for per-kind config

**Source:** `internal/cli/prompt/{input.go,password.go,select.go}` + idioms in
`cmd/dfsctl/commands/store/metadata/add.go:92-161` and
`cmd/dfsctl/commands/store/block/remote/add.go:99-175`.

**Apply to:** `repo add` (D-14) interactive flow.

Reuse directly:
- `prompt.InputRequired(label)` — required string
- `prompt.Input(label, default)` — optional with default
- `prompt.InputOptional(label)` — may return ""
- `prompt.Password(label)` / `prompt.PasswordWithValidation(label, minLen)` — hidden entry
- `prompt.InputPort(label, default)` — validated port
- `prompt.Select(label, opts)` — kind picker

**Abort handling:** wrap with `cmdutil.HandleAbort(err)` (util.go:260-266) at the call site so Ctrl+C becomes a clean exit.

### Table rendering

**Source:** `internal/cli/output/table.go` — `TableRenderer` interface
(`Headers() []string`, `Rows() [][]string`), `PrintTable`, `SimpleTable`
(key/value pairs).

**Apply to:** Every list command (via `TableRenderer`) and every grouped-section show command (via `ShareDetail`-like helpers that implement `TableRenderer`). See `cmd/dfsctl/commands/share/show.go:29-82` for the canonical grouped-section example.

### JSON body decoding (REST)

**Source:** `internal/controlplane/api/handlers/helpers.go:13-19` — `decodeJSONBody`.

**Apply to:** Every handler that accepts a request body.

```go
var req TriggerBackupRequest
if !decodeJSONBody(w, r, &req) {
	return
}
```

### Logging

**Source:** `internal/logger` — already imported across all handlers; see `metadata_stores.go:88`, `shares.go:520`.

**Apply to:** All new handlers and executor call sites. Conventions:
- `logger.Info(msg, kvs...)` for normal state transitions
- `logger.Warn(msg, kvs...)` for best-effort failures (e.g., progress update failures per D-50, runtime add-share failures per shares.go:327-329)
- `logger.Error(msg, kvs...)` for unexpected errors that don't fail the request (e.g., shares.go:272)

### ULID / UUID generation

- Records and jobs use **ULIDs** via `github.com/oklog/ulid/v2` — `ulid.Make().String()` (see backup.go:182-186, 256-259).
- Repos use **UUIDs** via `github.com/google/uuid` — `uuid.New().String()` (see backup.go:63-65).
- Phase 6 new code must match these conventions (D-40: exact 26-char ULID for `--from`).

---

## No Analog Found

None. Every file in Phase 6 has a close in-tree analog because Phase 6 is
pure surface area over runtime primitives that already exist in Phases 1–5.

The only semi-novel pieces are:

1. **Async poll loop with Ctrl-C three-way prompt (D-02, D-03).** No existing
   CLI command in the repo polls + intercepts SIGINT. Planner composes from
   stdlib: `time.NewTicker(1s)` + `signal.NotifyContext(os.Interrupt)`. No
   spinner library is used elsewhere in the repo; planner either vendors a
   minimal `\r`-based animation or picks a small dep (briandowns/spinner is
   the obvious pick but adds weight — planner decides).

2. **Typed problem-details extra fields (D-46).** Phase 6 adds
   `running_job_id` and `enabled_shares` extensions to the existing
   `Problem` struct shape; no similar typed extension exists in-tree today.
   Pattern is trivial: embed `Problem` + sibling fields (see
   `handlers/problem.go` section above).

3. **Cobra `<name> <verb>` layout for share commands.** Cobra doesn't
   natively support "positional-then-verb" at the parent level. The
   pragmatic path (already implicit in every `<verb> <name>` pattern in
   the repo like `share delete <name>`) is to keep `<verb>` at the
   parent level and have each leaf verb accept `<name>` as
   `args[0]` (`Args: cobra.ExactArgs(1)`). The user-visible command
   string `dfsctl share <name> <verb>` is pure help-text / documentation
   framing — Cobra still parses it the same way as today. **Planner
   should confirm this framing with a dry-run before the breaking flip.**

---

## Metadata

**Analog search scope:**
- `cmd/dfsctl/commands/` (entire tree — store, share, adapter, user, etc.)
- `internal/controlplane/api/handlers/` (all REST handlers)
- `pkg/apiclient/` (all typed-client files)
- `pkg/controlplane/store/` (interface + GORM impl)
- `pkg/controlplane/runtime/storebackups/` + `pkg/controlplane/runtime/shares/` (runtime entrypoints)
- `pkg/backup/executor/` + `pkg/backup/restore/` (progress call-site hosts)

**Files scanned:** ~55 Go files across the above trees.

**Pattern extraction date:** 2026-04-17.
