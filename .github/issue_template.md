---
name: Bug Report
about: Report a bug or unexpected behavior in DittoFS
title: '[BUG] '
labels: bug
assignees: ''
---

## Bug Description
<!-- A clear and concise description of what the bug is -->

## Steps to Reproduce

1.
2.
3.

## Expected Behavior
<!-- What you expected to happen -->

## Actual Behavior
<!-- What actually happened -->

## Environment

- **DittoFS Version**: <!-- e.g., commit hash or release version -->
- **Operating System**: <!-- e.g., Ubuntu 24.04, macOS 14.0 -->
- **Go Version**: <!-- output of `go version` -->
- **NFS Client**: <!-- e.g., Linux NFS client, macOS mount_nfs -->

## Configuration

```bash
# Command used to start DittoFS
./dittofs -port 2049 -log-level DEBUG

# Mount command used
sudo mount -t nfs -o nfsvers=3,tcp,port=2049,mountport=2049 localhost:/export /mnt/nfs
```

## Logs

```
<!-- Paste relevant log output here -->
<!-- Use -log-level DEBUG for verbose output -->
```

## Metadata Repository
<!-- Which metadata repository are you using? -->
- [ ] In-Memory (default)
- [ ] Custom implementation

## Content Repository
<!-- Which content repository are you using? -->
- [ ] Filesystem (FSContentStore)
- [ ] Custom implementation

## Additional Context
<!-- Add any other context about the problem here -->
<!-- Screenshots, network traces, or related issues -->

## Possible Solution
<!-- Optional: suggest a fix or workaround if you have one -->
