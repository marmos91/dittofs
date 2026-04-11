FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive

# Retry apt-get to handle transient network/mirror failures (exit code 100).
# See: https://forums.docker.com/t/docker-build-returns-exit-code-100/119303
RUN set -eu; \
    for i in 1 2 3; do \
        apt-get update \
          && apt-get install -y --no-install-recommends \
               krb5-kdc krb5-admin-server krb5-user \
          && rm -rf /var/lib/apt/lists/* \
          && exit 0; \
        echo "apt-get attempt $i failed, retrying in 5s..." >&2; \
        sleep 5; \
    done; \
    echo "ERROR: failed to install krb5 packages after 3 attempts" >&2; \
    exit 1

COPY --chmod=0755 entrypoint.sh /entrypoint.sh

EXPOSE 88/tcp 88/udp

ENTRYPOINT ["/entrypoint.sh"]
