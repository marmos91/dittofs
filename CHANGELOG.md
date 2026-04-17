# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.13.0] — YYYY-MM-DD

### Breaking Changes

- **`dfsctl share` restructure (D-35).** All per-share verbs now follow the
  `dfsctl share <name> <verb>` layout. `share list` and `share create` remain
  root-level. Scripts invoking the old `<verb> <name>` order continue to work
  mechanically (Cobra parses the name as `args[0]` regardless of position),
  but the new layout is now the documented canonical form. `share disable`
  and `share enable` (new in v0.13.0) only accept the new shape.

  Before:

  ```
  dfsctl share delete /archive
  dfsctl share edit /archive --read-only true
  dfsctl share show /archive
  dfsctl share mount /archive /mnt/dittofs
  dfsctl share unmount /mnt/dittofs
  ```

  After:

  ```
  dfsctl share /archive delete
  dfsctl share /archive edit --read-only true
  dfsctl share /archive show
  dfsctl share /archive mount /mnt/dittofs
  dfsctl share unmount /mnt/dittofs   # unchanged — keyed by mount-point
  ```

  `unmount` continues to take a mount-point path because a single share can
  be mounted to multiple local paths.

### Added

- **CLI: `dfsctl share <name> disable` / `dfsctl share <name> enable`.**
  Drain clients + refuse new connections. Disable is synchronous — the
  command returns only after connected clients have been disconnected (or
  the server's lifecycle shutdown timeout fires). Required precondition for
  a metadata-store restore.
- **CLI: `share list` and `share show` surface an `ENABLED` field / column.**
  `share list` adds an `ENABLED` column rendering `yes`/`-`. `share show`
  adds an `Enabled: yes/no` row. Both are surfaced in `-o json` / `-o yaml`
  output via the `enabled` field on the Share record.
- **REST: `POST /api/v1/shares/{name}/disable` + `POST /api/v1/shares/{name}/enable`.**
  Admin-only. Return the updated Share record on success. The disable route
  blocks until the drain completes.
