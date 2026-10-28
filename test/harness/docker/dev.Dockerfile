# DittoFS dev and test image: the whole harness in Linux, so a contributor's machine
# only needs Docker and git, on amd64 or arm64. `bin/dtc` runs `dt` in a privileged
# container of this image on the Docker host's Linux kernel: kernel NFS and CIFS
# clients, root without sudo, GNU userland, bash 5.
#
# Pins follow the repo: Go from go.mod's toolchain line, golangci-lint at the lint
# checks' version, pjdfstest, pynfs, cthon04 and nfstest from flake.lock, the test
# tools the suites' {tools} names.
# The kernel is the Docker host's; Docker Desktop's has no NFS Kerberos
# (rpcsec_gss_krb5) and no dm-flakey.

# pjdfstest at the flake.lock revision, in the layout dt looks for.
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

# cthon04 at the flake.lock revision, built in its tree, laid out as the flake's
# output: the tree in share/cthon04, upstream's entry points in bin.
FROM debian:bookworm-slim AS cthon04
ARG CTHON04_REV=86a4501a6e1e415844dc632894a85a5253cc1505
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential libtirpc-dev pkg-config git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN git clone git://git.linux-nfs.org/projects/steved/cthon04.git /opt/cthon04/share/cthon04 \
    && cd /opt/cthon04/share/cthon04 && git checkout "$CTHON04_REV" && make \
    && mkdir /opt/cthon04/bin && ln -s /opt/cthon04/share/cthon04/runtests /opt/cthon04/share/cthon04/server /opt/cthon04/bin/

FROM docker:29-cli AS dockercli

FROM golang:1.27.3-bookworm
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && apt-get install -y --no-install-recommends \
        # protocol clients and services the suites use
        nfs-common cifs-utils smbclient krb5-user rpcbind nfs4-acl-tools keyutils nfs-kernel-server \
        # userland the repo's scripts assume (GNU, bash 5, perl and prove for pjdfstest)
        bash coreutils util-linux procps psmisc lsof iproute2 iptables netcat-openbsd \
        socat curl ca-certificates openssl netbase kmod acl attr perl git \
        jq xmlstarlet gettext-base shellcheck fio \
        # gawk becomes /usr/bin/awk: bookworm's mawk 1.3.4-20200120 has no regex
        # intervals, so the pynfs grader's /^\*{50}$/ never matches
        gawk \
        # pynfs (Python 3.11 still ships xdrlib)
        python3 python3-ply python3-setuptools \
        # cthon04 compiles its general tests on the mount; nfstest captures
        # packets and drives its second client over ssh, with sudo as an
        # ordinary user, which the suites that run as the user dt runs as need
        libtirpc-dev pkg-config time tcpdump openssh-client openssh-server sudo \
    && rm -rf /var/lib/apt/lists/* \
    && mkdir -p /etc/samba && printf '[global]\n' > /etc/samba/smb.conf

# Docker CLI + compose/buildx plugins, for the suites that start containers
# (SMB conformance compose, testcontainers, the harness services) through the
# host's Docker socket.
COPY --from=dockercli /usr/local/bin/docker /usr/local/bin/docker
COPY --from=dockercli /usr/local/libexec/docker/cli-plugins /usr/local/libexec/docker/cli-plugins

# golangci-lint at the lint checks' version, built with this Go.
RUN go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0 \
    && mv /go/bin/golangci-lint /usr/local/bin/ && rm -rf /root/.cache/go-build /go/pkg/mod

COPY --from=pjdfstest /opt/pjdfstest /opt/pjdfstest

# pynfs at the flake.lock revision, with pynfs-4.0/4.1 wrappers like the flake's, in its
# bin as in the flake's output. That bin is on PATH for the scripts that run pynfs by name.
ARG PYNFS_REV=cd4701827a8261fedbfb4c6e39029fb9671321a6
RUN git clone https://github.com/kofemann/pynfs.git /opt/pynfs && cd /opt/pynfs && git checkout "$PYNFS_REV" \
    && python3 setup.py build >/tmp/pynfs-build.log 2>&1 \
    && test -f nfs4.1/xdrdef/nfs4_const.py && test -f nfs4.1/xdrdef/nfs4_pack.py \
    && mkdir bin && for v in 4.0 4.1; do \
        printf '#!/bin/sh\ncd /opt/pynfs/nfs%s && exec python3 /opt/pynfs/nfs%s/testserver.py "$@"\n' "$v" "$v" > bin/pynfs-$v; \
        chmod +x bin/pynfs-$v; done

COPY --from=cthon04 /opt/cthon04 /opt/cthon04

# nfstest at the flake.lock revision, with a wrapper per module in its bin, as
# in the flake's output.
ARG NFSTEST_REV=94089545caa18bbe4be1b055125d0721fe8dee80
RUN git clone git://git.linux-nfs.org/projects/mora/nfstest.git /opt/nfstest/share/nfstest \
    && cd /opt/nfstest/share/nfstest && git checkout "$NFSTEST_REV" && mkdir /opt/nfstest/bin \
    && for t in /opt/nfstest/share/nfstest/test/nfstest_*; do \
        printf '#!/bin/sh\nPYTHONPATH=/opt/nfstest/share/nfstest exec python3 %s "$@"\n' "$t" > "/opt/nfstest/bin/${t##*/}"; \
        chmod +x "/opt/nfstest/bin/${t##*/}"; done

ENV PATH=/opt/pjdfstest/bin:/opt/pynfs/bin:$PATH \
    GOTOOLCHAIN=local \
    DT_IN_CONTAINER=1
# The repo is bind-mounted from the host and owned by another uid.
RUN git config --system --add safe.directory '*'
