FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive

# Retry apt-get commands to handle transient network/mirror failures (exit code 100)
# See: https://forums.docker.com/t/docker-build-returns-exit-code-100/119303
RUN set -e && \
    for i in 1 2 3; do \
        apt-get update && \
        apt-get install -y --no-install-recommends krb5-kdc krb5-admin-server && \
        break || \
        (echo "Attempt $i failed, retrying in 5s..." && sleep 5); \
    done && \
    rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
