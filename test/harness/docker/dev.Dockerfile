# DittoFS dev/test image: the whole harness in Linux, so a contributor's Mac only
# needs Docker Desktop and git. `harness/bin/dtc` runs `dt` in a privileged container
# of this image on Docker Desktop's Linux kernel, the same way CI runs on Linux:
# kernel NFS/CIFS clients, root without sudo, GNU userland, bash 5.
#
# Pins follow the repo: Go from go.mod/CI (1.26.x), golangci-lint from lint.yml,
# pjdfstest and pynfs from flake.lock. Kernel limits of Docker Desktop still apply:
# no NFS Kerberos (rpcsec_gss_krb5) and no dm-flakey.

# pjdfstest at the flake.lock revision, in the layout run-posix.sh looks for.
FROM debian:bookworm-slim AS pjdfstest
ARG PJDFSTEST_REV=03eb25706d8dbf3611c3f820b45b7a5e09a36c06
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential autoconf automake pkg-config libacl1-dev git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN git clone https://github.com/pjd/pjdfstest.git /src && cd /src && git checkout "$PJDFSTEST_REV" \
    && autoreconf -ifs && ./configure && make pjdfstest \
    && mkdir -p /opt/pjdfstest/bin /opt/pjdfstest/share/pjdfstest \
    && cp pjdfstest /opt/pjdfstest/bin/ && cp pjdfstest /opt/pjdfstest/share/pjdfstest/ \
    && cp -r tests /opt/pjdfstest/share/pjdfstest/

FROM docker:29-cli AS dockercli

FROM golang:1.26.8-bookworm
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        # protocol clients and services the suites use (e2e-tests.yml's list and more)
        nfs-common cifs-utils smbclient krb5-user rpcbind nfs4-acl-tools keyutils \
        # userland the repo scripts assume (GNU, bash 5, perl/prove for pjdfstest)
        bash coreutils util-linux procps psmisc lsof iproute2 iptables netcat-openbsd \
        socat curl ca-certificates openssl netbase kmod acl attr perl git \
        jq xmlstarlet gettext-base shellcheck fio \
        # gawk becomes /usr/bin/awk: bookworm's mawk 1.3.4-20200120 has no regex
        # intervals, so the pynfs grader's /^\*{50}$/ never matches
        gawk \
        # pynfs (Python 3.11 still ships xdrlib)
        python3 python3-ply python3-setuptools \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/samba && printf '[global]\n' > /etc/samba/smb.conf

# Docker CLI + compose/buildx plugins, for the suites that start containers
# (SMB conformance compose, testcontainers, the harness services) through the
# host's Docker socket.
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins /usr/local/libexec/docker/cli-plugins

# golangci-lint pinned to lint.yml, built with this Go.
RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 \
    && mv /go/bin/golangci-lint /usr/local/bin/ && rm -rf /root/.cache/go-build /go/pkg/mod

COPY --from=pjdfstest /opt/pjdfstest /opt/pjdfstest

# pynfs at the flake.lock revision, with pynfs-4.0/4.1 wrappers like the flake's.
ARG PYNFS_REV=cd4701827a8261fedbfb4c6e39029fb9671321a6
RUN git clone https://github.com/kofemann/pynfs.git /opt/pynfs && cd /opt/pynfs && git checkout "$PYNFS_REV" \
    && python3 setup.py build >/tmp/pynfs-build.log 2>&1 \
    && test -f nfs4.1/xdrdef/nfs4_const.py && test -f nfs4.1/xdrdef/nfs4_pack.py \
    && for v in 4.0 4.1; do \
        printf '#!/bin/sh\ncd /opt/pynfs/nfs%s && exec python3 /opt/pynfs/nfs%s/testserver.py "$@"\n' "$v" "$v" > /usr/local/bin/pynfs-$v; \
        chmod +x /usr/local/bin/pynfs-$v; done

ENV PATH=/opt/pjdfstest/bin:$PATH \
    GOTOOLCHAIN=local \
    DT_IN_CONTAINER=1
# The repo is bind-mounted from the host and owned by another uid.
RUN git config --system --add safe.directory '*'
