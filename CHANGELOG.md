# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.13.0] — YYYY-MM-DD

### Breaking Changes

- **Backup/restore/repo CLI surface.** New verbs under
  `dfsctl store metadata <store> backup` — `run`, `list`, `show`, `pin`,
  `unpin`, `restore`, `repo` (add/list/show/edit/remove), and
  `job` (list/show/cancel). Restore and repo management live under
  `backup` so every backup-related operation is in one subtree.

- **New share verbs `disable` / `enable`.** Drain clients + refuse new
  connections. `disable` is synchronous — the command returns only after
  connected clients have been disconnected (or the server's lifecycle
  shutdown timeout fires). `disable` on every share backing a metadata
  store is the required precondition for `backup restore`.

### Added

- **CLI: first-class metadata-store backup/restore.** `dfsctl store metadata <store> backup`
  exposes `run`, `list`, `show`, `pin`, `unpin`, `restore`, `repo`
  (add/edit/list/show/remove), and `job` (list/show/cancel). Repos can be
  local filesystem or S3 with optional AES-256-GCM encryption and cron
  schedules.
- **CLI: `share list` and `share show` surface an `ENABLED` field / column.**
  `share list` adds an `ENABLED` column rendering `yes`/`-`. `share show`
  adds an `Enabled: yes/no` row. Both are surfaced in `-o json` / `-o yaml`
  output via the `enabled` field on the Share record.
- **REST: `POST /api/v1/shares/{name}/disable` + `POST /api/v1/shares/{name}/enable`.**
  Admin-only. Return the updated Share record on success. The disable route
  blocks until the drain completes.
- **REST: backup/restore/job/repo endpoints under
  `/api/v1/store/metadata/{name}/`.** Admin-only. Backup triggers return
  202 Accepted with `{Record, Job}`; restore returns 202 with `BackupJob`.
  Restore refuses with 409 + RFC7807 `RestorePreconditionError` if any
  share backing the store is still enabled.
