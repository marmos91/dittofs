# Troubleshooting DittoFS

This guide covers common issues and their solutions when working with DittoFS.

## Table of Contents

- [Connection Issues](#connection-issues)
- [Mount Issues](#mount-issues)
  - [SMB mount permission denied (macOS)](#smb-mount-permission-denied-macos)
- [Permission Issues](#permission-issues)
- [File Handle Issues](#file-handle-issues)
- [Performance Issues](#performance-issues)
- [Logging and Debugging](#logging-and-debugging)

## Connection Issues

### Cannot mount: Connection refused

**Symptoms:**
```
mount.nfs: Connection refused
```

**Solutions:**

1. **Check if DittoFS is running:**
   ```bash
   ps aux | grep dfs
   ```

2. **Verify the port is correct:**
   ```bash
   netstat -an | grep 12049
   # or
   lsof -i :12049
   ```

3. **Check firewall rules:**
   ```bash
   # Linux
   sudo iptables -L | grep 12049

   # macOS
   sudo pfctl -s rules | grep 12049
   ```

4. **Verify configuration:**
   ```bash
   # Check the config file
   cat ~/.config/dittofs/config.yaml

   # Start with debug logging
   DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
   ```

### Connection timeout

**Symptoms:**
```
mount.nfs: Connection timed out
```

**Solutions:**

1. **Check network connectivity:**
   ```bash
   ping localhost
   telnet localhost 12049
   ```

2. **Review timeout settings in config:**
   ```yaml
   adapters:
     nfs:
       timeouts:
         read: 5m
         write: 30s
         idle: 5m
   ```

## Mount Issues

### Invalid file system

**Symptoms:**
```
mount: /mnt/nfs: invalid file system.
```

**Cause:** The mount point directory does not exist.

**Solution:** Create the mount point before mounting:
```bash
# Linux
sudo mkdir -p /mnt/nfs
sudo mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /mnt/nfs

# macOS (use /tmp since /mnt doesn't exist, sudo not required)
mkdir -p /tmp/nfs
mount -t nfs -o tcp,port=12049,mountport=12049 localhost:/export /tmp/nfs
```

### Permission denied when mounting

**Symptoms:**
```
mount.nfs: access denied by server while mounting
```

**Solutions:**

1. **On Linux, allow non-privileged ports:**
   ```bash
   sudo sysctl -w net.ipv4.ip_unprivileged_port_start=0
   ```

2. **On macOS, use resvport option:**
   ```bash
   sudo mount -t nfs -o tcp,port=12049,mountport=12049,resvport localhost:/export /mnt/test
   ```

3. **Check export configuration:**
   ```yaml
   shares:
     - name: /export
       allowed_clients:
         - 192.168.1.0/24  # Make sure your IP is in this range
       denied_clients: []
   ```

4. **Verify authentication settings:**
   ```yaml
   shares:
     - name: /export
       require_auth: false  # Set to false for development
       allowed_auth_methods: [anonymous, unix]
   ```

### No such file or directory

**Symptoms:**
```
mount.nfs: mounting localhost:/export failed, reason given by server: No such file or directory
```

**Solutions:**

1. **Verify the export path exists in configuration:**
   ```yaml
   shares:
     - name: /export  # This is the export path
   ```

2. **Check share names are correct:**
   ```bash
   # Mount using the exact share name from config
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
   ```

### SMB mount permission denied (macOS)

**Symptoms:**
```
zsh: permission denied: /tmp/smb-test/file.txt
```

This happens after mounting an SMB share, even with 0777 permissions.

**Cause:** macOS has a security restriction where **only the mount owner can access files**,
regardless of Unix permissions. This is enforced at a level below file permissions - no SMB
traffic even reaches the server. Apple confirmed this is "works as intended".

**Solution - use dfsctl (handles this automatically):**

The `dfsctl share mount` command automatically handles this by using `sudo -u $SUDO_USER`
to mount as your user instead of root:

```bash
# This works - mount owned by your user, not root
sudo dfsctl share mount --protocol smb /export /mnt/share
```

**Alternative - mount to user directory without sudo:**
```bash
mkdir -p ~/mnt/share
dfsctl share mount --protocol smb /export ~/mnt/share
```

**If using manual mount_smbfs:**
```bash
# Mount as your user, not root
sudo -u $USER mount_smbfs //user:pass@localhost:12445/export /mnt/share
```

**Note:** This is a macOS-specific issue. On Linux, `dfsctl share mount` uses uid/gid
options which work correctly.

See [Known Limitations](KNOWN_LIMITATIONS.md#macos-mount-owner-only-access) for details.

## Permission Issues

### Permission denied on file operations

**Symptoms:**
```
touch: cannot touch 'file.txt': Permission denied
```

**Solutions:**

1. **Check identity mapping configuration:**
   ```yaml
   shares:
     - name: /export
       identity_mapping:
         map_all_to_anonymous: true  # Try this for development
         anonymous_uid: 65534
         anonymous_gid: 65534
   ```

2. **Verify root directory permissions:**
   ```yaml
   shares:
     - name: /export
       root_attr:
         mode: 0777  # Wide open for debugging
         uid: 0
         gid: 0
   ```

3. **Check your client UID/GID:**
   ```bash
   id  # Check your UID and GID
   ```

4. **Enable debug logging to see auth context:**
   ```bash
   DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
   # Look for lines showing UID/GID in requests
   ```

### Read-only filesystem

**Symptoms:**
```
touch: cannot touch 'file.txt': Read-only file system
```

**Solutions:**

1. **Check share configuration:**
   ```yaml
   shares:
     - name: /export
       read_only: false  # Must be false for writes
   ```

2. **Verify mount options:**
   ```bash
   # Make sure you're not mounting with 'ro'
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049,rw localhost:/export /mnt/test
   ```

## File Handle Issues

### Stale file handle

**Symptoms:**
```
ls: cannot access 'file.txt': Stale file handle
```

**Causes:**
- Server was restarted with in-memory metadata (handles lost)
- File was deleted while client held a handle
- Metadata backend was changed

**Solutions:**

1. **Unmount and remount the filesystem:**
   ```bash
   sudo umount /mnt/test
   sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
   ```

2. **For persistent handles, use BadgerDB metadata:**
   ```yaml
   metadata:
     stores:
       persistent:
         type: badger
         badger:
           db_path: /var/lib/dittofs/metadata

   shares:
     - name: /export
       metadata_store: persistent
   ```

3. **Clear client NFS cache (Linux):**
   ```bash
   # This varies by distribution
   sudo service nfs-common restart
   ```

## Performance Issues

### Slow read/write operations

**Diagnostics:**
```bash
# Run benchmarks to identify bottleneck
./scripts/benchmark.sh --profile

# Check server logs for slow operations
tail -f ~/.config/dittofs/dittofs.log | grep -i "slow\|timeout"
```

**Solutions:**

1. **Tune buffer sizes:**
   ```yaml
   metadata:
     global:
       filesystem_capabilities:
         max_read_size: 1048576   # 1MB
         max_write_size: 1048576  # 1MB
   ```

2. **Use memory stores for development:**
   ```yaml
   metadata:
     stores:
       fast:
         type: memory

   content:
     stores:
       fast:
         type: memory
   ```

3. **Enable async writes (if safe):**
   ```yaml
   shares:
     - name: /export
       async: true  # Faster but less safe
   ```

4. **For S3, tune part size:**
   ```yaml
   content:
     stores:
       s3-store:
         type: s3
         s3:
           part_size: 10485760  # 10MB parts
   ```

### High memory usage

**Diagnostics:**
```bash
# Profile memory usage
go test -bench=. -memprofile=mem.prof ./test/e2e/
go tool pprof mem.prof
```

**Solutions:**

1. **Check for memory leaks in logs**
2. **Reduce max connections:**
   ```yaml
   adapters:
     nfs:
       max_connections: 100
   ```

3. **Monitor metrics:**
   ```yaml
   server:
     metrics:
       enabled: true
       port: 9090
   ```
   Then visit `http://localhost:9090/metrics`

## Logging and Debugging

### Enable Debug Logging

**Via environment variable:**
```bash
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start
```

**Via configuration:**
```yaml
logging:
  level: DEBUG
  format: text  # or json
  output: stdout  # or file path
```

### Understanding Log Output

**Key log patterns:**

- `[INFO] NFS: Accepted connection from 127.0.0.1:54321` - Client connected
- `[DEBUG] NFS: LOOKUP(handle=..., name=file.txt)` - Operation details
- `[ERROR] NFS: Failed to read file: no such file` - Error conditions
- `[DEBUG] Auth: UID=1000, GID=1000, GIDs=[1000,4,20]` - Authentication context

### Capture Traffic

For deep debugging, capture NFS traffic:

```bash
# Linux
sudo tcpdump -i lo -w nfs.pcap port 12049

# macOS
sudo tcpdump -i lo0 -w nfs.pcap port 12049

# Analyze with Wireshark
wireshark nfs.pcap
```

### Check Server Health

```bash
# Check if server is responding
./dfs status

# Check metrics (if enabled)
curl http://localhost:9090/metrics

# Check configuration
DITTOFS_LOGGING_LEVEL=DEBUG ./dfs start 2>&1 | grep -i "config"
```

## Common Error Messages

### "export not found"

**Cause:** The share name in the mount command doesn't match configuration.

**Solution:** Check share names in config and use exact match:
```bash
# If config has "name: /export"
sudo mount -t nfs -o nfsvers=3,tcp,port=12049,mountport=12049 localhost:/export /mnt/test
```

### "authentication failed"

**Cause:** Server requires authentication but client isn't providing it.

**Solution:** Either disable authentication or configure it properly:
```yaml
shares:
  - name: /export
    require_auth: false
    allowed_auth_methods: [anonymous, unix]
```

### "metadata store not found"

**Cause:** Share references a non-existent metadata store.

**Solution:** Ensure the store is defined:
```yaml
metadata:
  stores:
    my-store:
      type: memory

shares:
  - name: /export
    metadata_store: my-store  # Must match store name above
```

## Getting More Help

If you're still experiencing issues:

1. **Check existing issues:** [GitHub Issues](https://github.com/marmos91/dittofs/issues)
2. **Enable debug logging** and capture relevant output
3. **Open a new issue** with:
   - DittoFS version
   - Operating system and version
   - Configuration file (redact sensitive info)
   - Full error messages
   - Debug logs showing the problem
   - Steps to reproduce
