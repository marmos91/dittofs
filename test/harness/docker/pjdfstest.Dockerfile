# pjdfstest runner for macOS hosts. The POSIX suite is meant to run through the
# Linux kernel NFS client (as in CI), so this image mounts the export itself from
# a privileged container instead of testing the Mac's NFS client.
#
# pjdfstest is pinned to the revision in the repo's flake.lock, which is what CI
# builds. The repo's test/posix/Dockerfile.pjdfstest clones HEAD unpinned.
FROM debian:bookworm-slim AS builder
ARG PJDFSTEST_REV=03eb25706d8dbf3611c3f820b45b7a5e09a36c06
RUN apt-get update && apt-get install -y --no-install-recommends \
        build-essential autoconf automake pkg-config libacl1-dev git ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN git clone https://github.com/pjd/pjdfstest.git /src \
    && cd /src && git checkout "$PJDFSTEST_REV" \
    && autoreconf -ifs && ./configure && make pjdfstest

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        bash perl nfs-common netbase acl util-linux coreutils procps openssl \
    && rm -rf /var/lib/apt/lists/*
# openssl: pjdfstest's misc.sh generates names with `openssl rand` (CI runners ship it).
# netbase: mount.nfs resolves "tcp" through /etc/protocols, which -slim omits;
# without it an NFSv3 mount fails with "Protocol not supported".
# Layout run-posix.sh expects: <prefix>/bin/pjdfstest + <prefix>/share/pjdfstest/tests
COPY --from=builder /src/pjdfstest /opt/pjdfstest/bin/pjdfstest
COPY --from=builder /src/tests /opt/pjdfstest/share/pjdfstest/tests
# The flake also places the binary beside tests/, because misc.sh walks up from tests/.
COPY --from=builder /src/pjdfstest /opt/pjdfstest/share/pjdfstest/pjdfstest
ENV PATH=/opt/pjdfstest/bin:$PATH
