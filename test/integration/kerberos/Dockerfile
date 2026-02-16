FROM debian:bookworm-slim
ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update && \
    apt-get install -y --no-install-recommends krb5-kdc krb5-admin-server && \
    rm -rf /var/lib/apt/lists/*
CMD ["sleep", "infinity"]
