#!/usr/bin/env bash
# Usage: TARGET_DIR=/mnt/profiles ./run.sh 01-logon-outlook-start.fio [extra fio args]
# Override any default via environment, e.g. USERS=200 STORM_WINDOW=600 ./run.sh ...
set -euo pipefail

: "${TARGET_DIR:?set TARGET_DIR to the directory under test}"
export TARGET_DIR
export IOENGINE="${IOENGINE:-libaio}"
export VHDX_SIZE="${VHDX_SIZE:-30g}"       # allocated size of a typical container
export USERS="${USERS:-1}"
export RUNTIME="${RUNTIME:-600}"           # seconds, time-based scenarios
export STORM_WINDOW="${STORM_WINDOW:-0}"   # seconds over which logons start
export COMPRESS_PCT="${COMPRESS_PCT:-30}"
export LOGON_IO="${LOGON_IO:-300m}"
export MAIL_INTERVAL="${MAIL_INTERVAL:-120s}"
export SEND_INTERVAL="${SEND_INTERVAL:-900s}"
export SORT_READ="${SORT_READ:-150m}"
export SORT_WRITE="${SORT_WRITE:-30m}"
export CATALOG_IOPS="${CATALOG_IOPS:-50}"
export CATALOG_LOG_IOPS="${CATALOG_LOG_IOPS:-10}"
export SYNC_RATE="${SYNC_RATE:-10m}"
export SYNC_GROW="${SYNC_GROW:-6g}"        # bytes appended per user in a full sync
export SYNC_META_IOPS="${SYNC_META_IOPS:-200}"
export COMPACT_MOVE="${COMPACT_MOVE:-4g}"
export COMPACT_BS="${COMPACT_BS:-1m}"

job="${1:?usage: run.sh <job.fio> [fio args]}"; shift

# Results live next to this script, wherever it is called from.
results_dir="$(cd "$(dirname "$0")" && pwd)/results"
mkdir -p "$results_dir"
out="$results_dir/$(basename "$job" .fio)-u${USERS}-$(date +%Y%m%d-%H%M%S).json"
fio --output-format=json+ --output="$out" "$@" "$job"

# fio --parse-only writes nothing; drop the empty file and skip the summary.
if [ ! -s "$out" ]; then
  rm -f "$out"
  exit 0
fi
echo "Results: $out"

if command -v jq >/dev/null; then
  jq -r '
    def p99(side): (map((.[side].lat_ns.percentile // .[side].clat_ns.percentile // {})["99.000000"] // 0) | max) / 1000 | floor;
    .jobs | group_by(.jobname)[] |
    (map(.job_runtime) | sort) as $rt |
    "\(.[0].jobname): jobs=\(length)  runtime_ms p50=\($rt[(length*0.5|floor)]) p90=\($rt[(length*0.9|floor)]) max=\($rt[-1])  " +
    "read_iops=\(map(.read.iops)|add|floor) write_iops=\(map(.write.iops)|add|floor)  " +
    "read_MiBps=\((map(.read.bw)|add)/1024|floor) write_MiBps=\((map(.write.bw)|add)/1024|floor)  " +
    "worst_p99_us read=\(p99("read")) write=\(p99("write"))"
  ' "$out"
fi