# Phase 02 — Deferred Items

Out-of-scope issues discovered during execution of Phase 02 plans. Not fixed here; logged for future hygiene passes.

## 02-04 (Postgres backup driver)

### `TestAPIServer_Lifecycle` fails when port 18080 is already bound

- **File:** `pkg/controlplane/api/server_test.go` (pre-existing)
- **Symptom:** `listen tcp :18080: bind: address already in use`
- **Root cause:** Test hard-codes port 18080; on the developer box Docker Desktop's `com.docker.backend` was already holding the port. Not introduced by the 02-04 changes — the test file was untouched.
- **Suggested fix:** Use `net.Listen("tcp", ":0")` and read back the assigned port, or parametrize the port via env var.
- **Scope:** Out of scope for Phase 02 (per-engine drivers). Fix belongs with the API server plan that originally introduced the test.

### Postgres Backup buffers each table fully in memory before tar emission

- **File:** `pkg/metadata/store/postgres/backup.go` (`Backup`, around the `bytes.Buffer` per table)
- **Symptom:** peak RAM during Backup is O(largest table) because `CopyTo` drains each `COPY ... TO STDOUT` into a `bytes.Buffer` so the tar header can record an exact `Size`. Large deployments with multi-GB tables could OOM.
- **Root cause:** `archive/tar` requires `Size` up front; no existing streaming alternative in `pkg/metadata/store/postgres/`.
- **Suggested fix:** stream to a temp file via `os.CreateTemp`, stat it for `Size`, then tar-stream the file; or switch to a framing format that supports unknown sizes (length-prefixed frames like the Badger driver).
- **Scope:** Out of scope for Phase 02 success criteria (round-trip + D-04 vacuum-lock budget met). Revisit for v0.13.0 production hardening or when the first large-table benchmark surfaces it.
