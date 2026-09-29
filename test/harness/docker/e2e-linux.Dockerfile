# Linux e2e runner for macOS hosts.
#
# The e2e suite skips its NFSv4 cases on darwin, and multi-user SMB cases cannot run
# there (macOS allows one SMB mount per share). Docker Desktop's Linux kernel has
# nfs, nfs4, cifs and smb3, so a privileged container can run those cases with the
# Linux clients CI uses. What it cannot do: NFS Kerberos (no rpcsec_gss_krb5 in the
# kernel) and the dm-flakey crash tests (no dm_flakey).
#
# Packages mirror e2e-tests.yml: nfs-common cifs-utils smbclient krb5-user rpcbind.
# Plus what the suite probes for and skips without: nfs4-acl-tools (nfs4_setfacl, the
# NFSv4 ACL tests), iptables (NFSv4.1 connection-disruption), iproute2 (`ip`, the NLM
# network-namespace interop test).
FROM golang:1.26-bookworm
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        nfs-common cifs-utils smbclient krb5-user rpcbind netbase \
        procps openssl ca-certificates kmod util-linux acl attr keyutils \
        nfs4-acl-tools iptables iproute2 \
    && rm -rf /var/lib/apt/lists/*
# smbclient warns without a config file; an empty one means built-in defaults.
RUN mkdir -p /etc/samba && printf '[global]\n' > /etc/samba/smb.conf
