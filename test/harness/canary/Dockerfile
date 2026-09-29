# The DittoFS canary's client: the Linux kernel SMB client (cifs-utils), rclone to
# read the S3 bucket, jq for dfsctl's JSON. The server's own dfsctl is mounted in at
# run time (run-canary.sh), so it always matches the server; this base matches the
# glibc of the test host it is built on (Ubuntu 26.04).
FROM ubuntu:26.04
RUN apt-get update && apt-get install -y --no-install-recommends \
        cifs-utils rclone jq curl ca-certificates coreutils util-linux \
    && rm -rf /var/lib/apt/lists/*
