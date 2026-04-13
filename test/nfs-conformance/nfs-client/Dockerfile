FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive

RUN set -eu; \
    for i in 1 2 3; do \
        apt-get update \
          && apt-get install -y --no-install-recommends \
               nfs-common krb5-user rpcbind kmod \
          && rm -rf /var/lib/apt/lists/* \
          && exit 0; \
        echo "apt-get attempt $i failed, retrying in 5s..." >&2; \
        sleep 5; \
    done; \
    echo "ERROR: failed to install packages after 3 attempts" >&2; \
    exit 1

COPY --chmod=0755 test-nfs-krb5.sh /test-nfs-krb5.sh

ENTRYPOINT ["/test-nfs-krb5.sh"]
