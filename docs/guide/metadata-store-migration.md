# Metadata Store Migration Guide

DittoFS now keeps file metadata in **BadgerDB** only. The `postgres`, `sqlite` and
`memory` metadata store types have been removed:

| Removed type | Replacement |
|--------------|-------------|
| `postgres`, `sqlite` | `badger` (on disk) |
| `memory` | `badger` kept in RAM: `dfsctl store metadata add --name <n> --in-memory` |

There is no tool that converts a SQL metadata store to Badger. A share on a removed
type moves to the new version by **copying its files** through a second DittoFS
instance, as described below.

## What is not affected

- **The control-plane database.** Users, groups, shares and permissions still live
  in the `database:` section of the server configuration, SQLite or PostgreSQL, as
  before. Only *metadata stores* changed.
- **Block stores.** S3 and the other block stores are unchanged.

## Are you affected?

On the version you run now, before upgrading:

```bash
dfsctl store metadata list
```

If every metadata store has type `badger`, you are not affected: upgrade as usual.

## What happens if you upgrade anyway

The new server refuses to start while any metadata store has a removed type:

```
failed to load metadata stores: failed to create metadata store "<name>": unsupported metadata store type "postgres": badger is the only metadata store type
```

Nothing is modified. Reinstall the version you upgraded from and follow the steps
below. A stopped server cannot remove the old store for you: `dfsctl` needs a
running server.

## `memory` stores: remove them before upgrading

A `memory` store holds nothing that survives a restart, so there is nothing to copy.
On the old version, remove each share that uses one, then the store:

```bash
dfsctl share remove /<share> --force
dfsctl store metadata remove <store> --force
```

Then upgrade, and recreate them on Badger in RAM:

```bash
dfsctl store metadata add --name <store> --in-memory
dfsctl share create --name /<share> --metadata <store> --block-store <block store>
```

## `postgres` and `sqlite` stores: copy the files to a new instance

Keep the old server running throughout. You copy the files from its shares to
shares on a second server running the new version, then switch clients over.

1. **Install the new version beside the old one**, with its own configuration:

   ```bash
   dfs init --config /etc/dittofs-new/config.yaml
   ```

   In that file, change everything the two servers would otherwise share:
   - `controlplane.port`, for example `8081`;
   - `database.sqlite.path` (or the PostgreSQL `database:` name): a new, empty
     control-plane database. Do not point it at the old one, whose store records
     the new version refuses;
   - `blockstore.journal.path`, a new directory.

   Start it with its own PID and log files:

   ```bash
   dfs start --config /etc/dittofs-new/config.yaml \
     --pid-file /run/dittofs-new.pid --log-file /var/log/dittofs-new.log
   dfsctl login --server http://127.0.0.1:8081 --username admin
   ```

2. **Recreate the setup on the new instance.** Its control plane starts empty, so
   create again the users, groups and permissions the shares need
   ([Access control](access-control.md)), then the stores and shares:

   ```bash
   dfsctl store metadata add --name md --db-path /var/lib/dittofs-new/md
   dfsctl store block add --name s3 --type s3 --config '<JSON>'
   dfsctl share create --name /export --metadata md --block-store s3
   ```

   Give the new block store **its own bucket, or its own prefix in the bucket**.
   Two servers must never use one bucket and prefix: each counts and garbage-collects
   the objects it finds there as its own.

3. **Enable the adapters on other ports** than the old server's:

   ```bash
   dfsctl adapter enable nfs --port 12050
   dfsctl adapter enable smb --port 12446
   ```

4. **Mount both shares on one client and copy.** From Linux over NFS, `rsync`
   keeps owners, modes, timestamps and hard links:

   ```bash
   sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/old
   sudo mount -t nfs -o tcp,port=12050,mountport=12050 localhost:/export /mnt/new
   sudo rsync -aH --numeric-ids /mnt/old/ /mnt/new/
   ```

   Add `-A` (POSIX ACLs) and `-X` (extended attributes) when your files carry them.
   If your clients rely on Windows ACLs or alternate data streams, copy over SMB from
   Windows instead, with `robocopy <old> <new> /MIR /COPYALL /DCOPY:DAT`
   ([SMB](smb.md) covers mounting a non-default port).

5. **Verify.** A dry run that compares contents should list nothing:

   ```bash
   sudo rsync -aHn --checksum --numeric-ids --itemize-changes /mnt/old/ /mnt/new/
   ```

6. **Switch over.** Stop the old server, move the new instance's adapters to the
   ports clients use (`dfsctl adapter edit nfs --port 12049`, and the same for
   SMB), and have clients remount: file handles from the old server do not carry
   over.

## What does not move

- **Snapshots.** Snapshots belong to the old server's shares. Keep the old server,
  or its data, for as long as you may need to restore one.
- **The recycle bin.** A share's recycle bin is the `#recycle` folder at its root,
  and a copy over NFS or SMB brings it across as an ordinary folder. Empty it first,
  or leave it out (`rsync --exclude '/#recycle'`), and restore anything you want to
  keep before you start.
- **Open files and locks.** Clients remount the new server, so nothing held on the
  old one carries over.
