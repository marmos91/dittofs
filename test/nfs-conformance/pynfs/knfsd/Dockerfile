# Linux kernel NFS server, for baselining pynfs against a reference
# implementation. Needs --privileged and a host kernel with nfsd; the export
# must be a volume, because overlayfs cannot be NFS-exported.
FROM ubuntu:24.04

RUN apt-get update \
    && apt-get install -y --no-install-recommends nfs-kernel-server \
    && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 2049/tcp
ENTRYPOINT ["/entrypoint.sh"]
