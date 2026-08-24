#!/bin/sh
# List every object under dittofs-bench/blocks/ as "size<TAB>lastmodified<TAB>key".
# Object size == PUT size (PutBlock is one PutObject), so this IS the wire
# distribution. Written to $1.
set -eu
: "${AWS_ACCESS_KEY_ID:?}" "${AWS_SECRET_ACCESS_KEY:?}"
aws --endpoint-url https://s3.fr-par.scw.cloud s3api list-objects-v2 \
  --bucket dittofs-bench --prefix blocks/ \
  --query 'Contents[].[Size,LastModified,Key]' --output text > "$1"
wc -l < "$1"
