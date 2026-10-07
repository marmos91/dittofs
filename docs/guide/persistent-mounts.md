# Mount at Boot

Mounts made with `mount` don't survive a reboot. To mount a DittoFS share
automatically, add it to `/etc/fstab`. This page covers Linux clients.

## Before you start

- The DittoFS server must be running when the mount happens. If you installed
  from the `.deb` or `.rpm` package, it runs as the `dfs` systemd service and
  starts at boot (see [Install & Deploy](install.md)). The quick install script
  does not set up a service.
- A broken `/etc/fstab` can stop the machine from booting. Back it up first:

```bash
sudo cp /etc/fstab /etc/fstab.bak
sudo mkdir -p /mnt/nfs /mnt/smb
```

Each entry below is one line to add to `/etc/fstab` (edit it with
`sudoedit /etc/fstab`). They are not shell commands.

## NFS

Recommended entry, mounted on first access:

```
localhost:/export  /mnt/nfs  nfs  tcp,port=12049,mountport=12049,_netdev,nofail,x-systemd.automount,x-systemd.idle-timeout=600  0  0
```

## SMB

Mounts at boot run as root, so keep the credentials in a root-owned file. Use the
password of the DittoFS user you mount as:

```bash
sudo install -m 600 /dev/null /etc/dittofs-smb.credentials
sudo tee /etc/dittofs-smb.credentials >/dev/null <<'EOF'
username=alice
password=ALICE_PASSWORD
EOF
```

Then add this entry, with your numeric `uid` and `gid` (from `id -u` and `id -g`):

```
//localhost/export  /mnt/smb  cifs  port=12445,credentials=/etc/dittofs-smb.credentials,vers=3.1.1,uid=1000,gid=1000,_netdev,nofail,x-systemd.automount,x-systemd.idle-timeout=600  0  0
```

## What the options do

| Option | Effect |
|---|---|
| `_netdev` | Waits for the network before mounting. |
| `nofail` | Boot continues if the mount fails (for example, the server is down). |
| `x-systemd.automount` | Mounts on first access instead of at boot. |
| `x-systemd.idle-timeout=600` | Optional. Unmounts after 10 minutes without use and remounts on the next access. Useful on laptops or when the server restarts often; remove it to keep the mount always active. |

## Server on the same host

If the DittoFS server runs on the same machine as the `dfs` systemd service
(package install), order the mount after it by adding these options to the entry:

```
x-systemd.after=dfs.service,x-systemd.requires=dfs.service
```

Skip them if you installed with the quick install script: there is no `dfs`
service, and the mount would fail. For a remote server, replace `localhost`
with the server's address and skip these options too.

## Apply and check without rebooting

```bash
sudo findmnt --verify          # checks /etc/fstab for errors
sudo systemctl daemon-reload
sudo mount -a
ls /mnt/nfs /mnt/smb           # first access triggers the automount
findmnt /mnt/nfs /mnt/smb
```