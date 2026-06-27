# Quotas

DittoFS enforces **per-identity** quotas on a share — by user, by group, or a
default-user fallback — limiting both **bytes** and **inodes (file count)**. Each limit
has a **soft** threshold, a **hard** ceiling, and a **grace** window between them.

> Quotas are managed live on a running server via `dfsctl` (or the REST API). There is no
> config-file or Kubernetes CRD form. The full reference is in
> [Configuration § Shares](configuration.md#8-shares-exports).

## Set a quota

```bash
# A user (by UID): 10 GiB / 100k files hard, soft at 8 GiB / 90k files,
# 7-day grace (604800s) before the soft byte limit becomes hard.
dfsctl quota set /export --scope user --id 1000 \
    --limit-bytes 10GiB --soft-bytes 8GiB \
    --limit-files 100000 --soft-files 90000 --grace-seconds 604800

# A group (by GID): 50 GiB, no file-count limit.
dfsctl quota set /export --scope group --id 2000 --limit-bytes 50GiB

# Everyone without an explicit quota (fallback template):
dfsctl quota set /export --scope default-user --limit-bytes 5GiB
```

Sizes accept binary units (`GiB`, `MiB`, …). Omit a flag to leave that dimension
unlimited.

## Inspect and remove

```bash
dfsctl quota list /export
dfsctl quota remove /export --scope user --id 1000
```

## How enforcement works

- **Soft → grace → hard.** When an identity crosses its soft threshold, a grace timer
  starts; once the grace window elapses the soft limit is enforced as hard. Drop back
  under soft and the timer resets. Default-user grace is tracked per-user and survives a
  server restart.
- **Most-specific wins.** A user is limited by its own quota if set, else its group's,
  else the default-user fallback.
- **Usage is by owner.** Bytes and inode counts are keyed by file owner UID/GID and
  rebuilt from file rows on startup. A `chown` moves a file's usage between identities.
- **Best-effort.** Under heavy concurrent writes an identity may briefly exceed a limit
  before usage catches up — normal for a userspace NFS/SMB server.

Quota limits live in the control-plane database and are also reachable via the REST API
at `/api/v1/shares/{name}/quotas`.
