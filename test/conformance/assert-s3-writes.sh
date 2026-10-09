#!/usr/bin/env bash
# Fails unless localstack accepted at least one PutObject.
#
# An S3 profile that never writes to S3 grades exactly like its local-only
# sibling, so a passing suite says nothing about whether the remote tier was
# exercised. localstack logs one line per request; this reads that log.
#
# Usage:
#   ./assert-s3-writes.sh <localstack-container>

set -euo pipefail

if [[ $# -ne 1 || -z "$1" ]]; then
    echo "Usage: $0 <localstack-container>" >&2
    exit 2
fi

log="$(docker logs "$1" 2>&1)"

count() { grep -c -- "$1" <<<"$log" || true; }

puts="$(count 'AWS s3.PutObject => 200')"
if ((puts > 0)); then
    echo "localstack accepted ${puts} PutObject requests"
    exit 0
fi

echo "ERROR: localstack accepted no PutObject requests; this run never reached S3." >&2
echo "  PutObject (any status):  $(count 'AWS s3.PutObject =>')" >&2
echo "  HeadBucket => 404:       $(count 'AWS s3.HeadBucket => 404')" >&2
echo "  NoSuchBucket responses:  $(count 'NoSuchBucket')" >&2
exit 1
