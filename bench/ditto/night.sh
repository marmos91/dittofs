#!/usr/bin/env bash
# The nightly performance run on a dedicated Linux host: DittoFS, built from develop, against a
# Samba baseline. Each serves one share over SMB on loopback, mounted by the kernel client with the
# same options (SMB 3.1.1, cache=none, signing on, encryption and compression off), one target after
# the other, never both at once. On each target, every step with its own time box:
#   matrix   fio: read and write, sequential and random, 4k, 64k and 1m blocks, 1, 2, 4 and 8
#            threads; buffered I/O; reads are time based, writes are one pass over each thread's
#            file and fsync it at its end
#   mailbox  the mailbox simulator (bench/workloads/mailbox) with 2, 8 and 16 users, scaled down
#   files    5,000 files of 4 KiB, 64 KiB and 1.75 MiB, created (each fsynced), then read back
# Each target starts after an fstrim of the data's file system, and the two take turns going first:
# one order on odd repeats, the other on even ones, swapped from one date to the next.
# score.py then compares the two targets point by point and writes the summary. Slack gets it by the
# scenarios' rules, through their reporting (test/scenarios/lib/report.sh), same bot, same channel:
# a quiet night posts nothing; the first new flag or failure is a message; while that alert is open,
# a new one replies in its thread and a repeat posts nothing; the night with no flag and no failure
# replies there, shown in the channel too, and closes it. See post.
#
#   night.sh              as root, from cron at 18:45 (setup.sh writes the cron file)
#   SMOKE=yes night.sh    one short point of each kind, a few minutes; never posts
#
# The checkout's working tree is not what runs. night.sh fetches develop and the branch the checkout
# is on, then hands over to the night.sh of that branch as fetched, extracted under
# /var/tmp/dittofs-perf/harness/<commit>: its scripts, the mailbox jobs and the scenarios' reporting
# all come from there, and dfs is built from develop as fetched. Both commits are recorded. A fetch
# that fails leaves the last fetched commits.
#
# Environment:
#   CALIBRATION=yes  every point three times, and no Slack post: the first week, whose spread sets
#                    each point's noise band
#   SLACK_DIR        the bot's slack-token and slack-channel; without them nothing posts
#   LOCK             the lock the scenario runs take (default /var/tmp/dittofs-scenarios)
#   STOP_AT          the hard stop, HH:MM local time (default 05:45): steps are cut to end before it
#   HARNESS          set by hand: this tree runs as it is, with dfs from develop as last fetched (a
#                    test of changes not pushed yet)
# Results: /var/tmp/dittofs-perf/<date>/ (a smoke run: smoke-<date>-<time>/), kept 30 days.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
TREE="$(cd "$HERE/../.." && pwd)"                               # the scripts, jobs and reporting
REPO="${REPO:-$(git -C "$HERE" rev-parse --show-toplevel)}"     # the checkout git works in
WORK=/var/tmp/dittofs-perf      # results, the Go caches and the run's state; smb.conf names it too
DATA=/srv/perf/data             # the targets' data, emptied after each target
MNT=/srv/perf/mnt
# Loopback ports. smb.conf holds PORT_SAMBA; PORT_DEAD is the block store endpoint nothing answers.
PORT_API=18480 PORT_DFS=18445 PORT_SAMBA=18446 PORT_DEAD=18409
MOUNT_OPTS=vers=3.1.1,cache=none,sign,uid=0,gid=0
PERF_USER="${PERF_USER:-dittofs-perf}"
LOCK="${LOCK:-/var/tmp/dittofs-scenarios}"
CALIBRATION="${CALIBRATION:-no}"
SMOKE="${SMOKE:-no}"

if test "$SMOKE" = yes; then
    : "${BOX:=600}" "${FILE_SIZE:=64m}" "${RUNTIME:=5}" "${RAMP:=1}" "${BS_LIST:=64k}" "${THREADS_LIST:=2}"
    : "${USERS_LIST:=2}" "${MAILBOX:=01-logon-outlook-start 04-search-reindex}" "${VHDX_SIZE:=64m}"
    : "${MAILBOX_RUNTIME:=10}" "${FILE_SETS:=4k}" "${FILES:=200}"
fi
test "$CALIBRATION" = no || : "${REPEATS:=3}"
: "${REPEATS:=1}" "${BOX:=3600}" "${FILE_SIZE:=512m}" "${RUNTIME:=30}" "${RAMP:=5}"
: "${RW_LIST:=read randread write randwrite}" "${BS_LIST:=4k 64k 1m}" "${THREADS_LIST:=1 2 4 8}"
: "${USERS_LIST:=2 8 16}" "${VHDX_SIZE:=512m}" "${MAILBOX_RUNTIME:=60}"
: "${MAILBOX:=01-logon-outlook-start 02-mail-flow 03-folder-resort 04-search-reindex 05-ost-full-sync 06-logoff-compaction}"
: "${FILE_SETS:=4k 64k 1792k}" "${FILES:=5000}"
export HERE PERF_USER SMOKE PORT_API PORT_DFS PORT_DEAD FILE_SIZE RUNTIME RAMP RW_LIST BS_LIST THREADS_LIST \
    USERS_LIST VHDX_SIZE MAILBOX_RUNTIME MAILBOX FILE_SETS FILES

log() { echo "$(date '+%F %T') $*"; }
secret() { od -An -N16 -tx1 /dev/urandom | tr -d ' \n'; }
drop_caches() { sync && echo 3 >/proc/sys/vm/drop_caches; }
listening() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
# wait_port PORT SECONDS: waits until something listens on PORT.
wait_port() { for _ in $(seq "$2"); do listening "$1" && return; sleep 1; done; return 1; }

# The steps that run under a time box are this script again: night.sh STEP. They read the target's
# mount from DIR, its results directory from RES and its state directory from RUN.

build() {
    local src="$WORK/build/src"
    rm -rf "$WORK/build"
    mkdir -p "$src" "$WORK/build/bin" "$WORK/go"
    git -C "$REPO" rev-parse "${DEVELOP:-origin/develop}" >"$NIGHT/commit"
    git -C "$REPO" archive "$(cat "$NIGHT/commit")" | tar -x -C "$src"
    cd "$src"
    CGO_ENABLED=0 GOFLAGS=-buildvcs=false GOTOOLCHAIN=auto GOPATH="$WORK/go" GOMODCACHE="$WORK/go/mod" \
        GOCACHE="$WORK/go/build" go build -o "$WORK/build/bin/" ./cmd/dfs ./cmd/dfsctl
    rm -rf "$src"
}

# dfs and dfsctl, with their home, config and state in the run's directory.
dfs_env() { env HOME="$RUN" XDG_CONFIG_HOME="$RUN/config" XDG_STATE_HOME="$RUN/state" PATH="$WORK/build/bin:$PATH" "$@"; }

start_dittofs() {
    local data="$DATA/dittofs"
    mkdir -p "$data"
    cat >"$RUN/config.yaml" <<EOF
logging: { level: INFO, format: text, output: stdout }
controlplane:
  port: $PORT_API
  jwt: { secret: "$(secret)$(secret)" }
database:
  type: sqlite
  sqlite: { path: "$data/controlplane.db" }
blockstore:
  journal: { path: "$data/journal" }
EOF
    # The first start creates the NFS and SMB adapters on their default ports, which another server
    # on the host may hold. So it runs in a network namespace of its own (setup_boot), where the
    # stores, the user, the share and the adapters are set up, and the measured server is the second
    # start. The admin password goes through the environment only, not as an argument (of env either).
    DITTOFS_ADMIN_INITIAL_PASSWORD="$(secret)"
    export DITTOFS_ADMIN_INITIAL_PASSWORD
    dfs_env unshare --net "$HERE/night.sh" setup_boot "$data" >>"$RES/dfs-setup.log" 2>&1 9<&-
    unset DITTOFS_ADMIN_INITIAL_PASSWORD
    env HOME="$RUN" XDG_CONFIG_HOME="$RUN/config" XDG_STATE_HOME="$RUN/state" \
        setsid "$WORK/build/bin/dfs" start --foreground --config "$RUN/config.yaml" >>"$RES/dfs.log" 2>&1 </dev/null 9<&- &
    echo "$!" >"$RUN/pid"
    # Up to three minutes: the start waits for the block store's first probe, ten attempts with backoff.
    wait_port "$PORT_DFS" 280
}

# setup_boot DATA: the first start of dfs, in its own network namespace (see start_dittofs).
# The block store is S3, as a share needs one. Its endpoint (s3stub.py) answers only here, since
# creating a share waits for its health probe; the measured server finds nothing there, starts with
# the store unhealthy, and keeps every write in its journal with the sync paused. So the run measures
# the frontend alone. No password goes on a command line, where any user of the host can read it:
# dfsctl reads the admin's at its login prompt, and the API creates the user, through curl's stdin.
setup_boot() {
    local api="http://127.0.0.1:$PORT_API" token
    ip link set lo up
    python3 "$HERE/s3stub.py" "$PORT_DEAD" &
    STUB=$!
    dfs start --foreground --config "$RUN/config.yaml" &
    DFS=$!
    trap 'kill "$STUB" "$DFS" 2>/dev/null; wait' EXIT
    for _ in $(seq 60); do
        printf '%s\n' "$DITTOFS_ADMIN_INITIAL_PASSWORD" | dfsctl login --server "$api" --username admin >/dev/null 2>&1 && break
        sleep 0.5
    done
    dfsctl adapter edit nfs --enabled false
    dfsctl store metadata add --name md --type badger --db-path "$1/metadata"
    dfsctl store block add --name s3 --type s3 --config "{\"endpoint\":\"http://127.0.0.1:$PORT_DEAD\",\"bucket\":\"perf\",\"access_key_id\":\"perf\",\"secret_access_key\":\"unreachable\",\"allow_private_endpoint\":true}"
    # dfsctl takes a new user's password only as an argument, or twice at a prompt that a pipe can't
    # answer, so the user comes from the API. The passwords are hex, so they need no JSON escaping.
    token="$(printf 'data = "{\\"username\\":\\"admin\\",\\"password\\":\\"%s\\"}"\n' "$DITTOFS_ADMIN_INITIAL_PASSWORD" |
        curl -sf -K - -H 'Content-Type: application/json' "$api/api/v1/auth/login" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p')"
    printf 'header = "Authorization: Bearer %s"\ndata = "{\\"username\\":\\"%s\\",\\"password\\":\\"%s\\",\\"uid\\":%s,\\"gid\\":%s,\\"role\\":\\"user\\"}"\n' \
        "$token" "$PERF_USER" "$PASSWORD" "$(id -u "$PERF_USER")" "$(id -g "$PERF_USER")" |
        curl -sf -K - -H 'Content-Type: application/json' "$api/api/v1/users" >/dev/null
    dfsctl share create --name /perf --metadata md --block-store s3 --journal-size 64GiB --owner "$PERF_USER"
    dfsctl share permission grant /perf --user "$PERF_USER" --level read-write
    dfsctl adapter edit smb --port "$PORT_DFS" --enabled true \
        --config '{"bind_address":"127.0.0.1","encryption":{"encryption_mode":"disabled"}}'
    dfsctl adapter list >"$RES/dittofs-adapters.txt"
}

start_samba() {
    mkdir -p "$WORK/samba/private" "$WORK/samba/lock" "$WORK/samba/state" "$WORK/samba/cache" "$WORK/samba/run" "$DATA/samba"
    chown "$PERF_USER:" "$DATA/samba"
    printf '%s\n%s\n' "$PASSWORD" "$PASSWORD" | smbpasswd -c "$HERE/smb.conf" -s -a "$PERF_USER" >/dev/null
    setsid smbd --foreground --no-process-group --configfile="$HERE/smb.conf" >>"$RES/smbd.out" 2>&1 </dev/null 9<&- &
    echo "$!" >"$RUN/pid"
    wait_port "$PORT_SAMBA" 60
}

# start TARGET: a fresh server for TARGET (dittofs or samba), its share mounted on DIR. The servers
# don't keep the lock's descriptor (9), so one that outlived the run could not hold the lock.
start() {
    local port=$PORT_DFS p
    for p in "$PORT_API" "$PORT_DFS" "$PORT_SAMBA" "$PORT_DEAD"; do
        ! listening "$p" || { log "port $p is in use"; return 1; }
    done
    # The host is shared: a target needs room for its data and the journal's dead data with some to spare.
    p="$(df --output=avail -BG "$DATA" | tail -1 | tr -dc 0-9)"
    test "$p" -ge "${MIN_FREE_GB:-80}" || { log "only ${p} GiB free on $DATA"; return 1; }
    mkdir -p "$RUN" "$RES" "$DIR"
    chmod 700 "$RUN"
    if test "$1" = samba; then port=$PORT_SAMBA && start_samba; else start_dittofs; fi
    printf 'username=%s\npassword=%s\n' "$PERF_USER" "$PASSWORD" >"$RUN/credentials"
    mount -t cifs //127.0.0.1/perf "$DIR" -o "$MOUNT_OPTS,port=$port,credentials=$RUN/credentials"
    grep " $DIR " /proc/mounts >"$RES/mount.txt"
    grep -E 'Dialect|Signing|signed|Encrypt|Compress|Security' /proc/fs/cifs/DebugData >>"$RES/mount.txt" 2>/dev/null || true
}

matrix() {
    local dir="$DIR/matrix" out="$RES/matrix" rw bs t name failed=0 max=0
    mkdir -p "$dir" "$out"
    for t in $THREADS_LIST; do test "$t" -le "$max" || max=$t; done
    # One file per thread, laid out once: the reads read them and the writes write them over.
    # shellcheck disable=SC2016 # $jobnum is fio's
    fio --name=layout --directory="$dir" --filename_format='bench.$jobnum' --size="$FILE_SIZE" --rw=write \
        --bs=1m --numjobs="$max" --ioengine=psync --end_fsync=1 --output-format=json --output="$out/layout.json"
    for rw in $RW_LIST; do
        for bs in $BS_LIST; do
            for t in $THREADS_LIST; do
                case "$rw" in
                read) name=seq-read ;; randread) name=rand-read ;; write) name=seq-write ;; randwrite) name=rand-write ;;
                esac
                name="$name-$bs-t$t"
                # A write ends with an fsync of its file; 120 s caps a slow one.
                local how=(--runtime=120 --end_fsync=1)
                case "$rw" in *read) how=(--time_based --runtime="$RUNTIME" --ramp_time="$RAMP") ;; esac
                drop_caches
                # shellcheck disable=SC2016 # $jobnum is fio's
                fio --name="$name" --directory="$dir" --filename_format='bench.$jobnum' --size="$FILE_SIZE" \
                    --rw="$rw" --bs="$bs" --numjobs="$t" --ioengine=psync --direct=0 "${how[@]}" --group_reporting \
                    --write_bw_log="$out/$name" --write_iops_log="$out/$name" --log_avg_msec=1000 --per_job_logs=0 \
                    --output-format=json --output="$out/$name.json" || failed=$((failed + 1))
            done
        done
    done
    rm -rf "$dir"
    test "$failed" -eq 0
}

mailbox() {
    local mb="$TREE/bench/workloads/mailbox" u s point t0 failed=0 first=yes
    export TARGET_DIR="$DIR/mailbox" RESULTS_DIR="$RES/mailbox" RUNTIME="$MAILBOX_RUNTIME"
    # The simulator's sizes and rates, scaled to VHDX_SIZE and a short RUNTIME (see its README).
    export LOGON_IO=100m SORT_READ=50m SORT_WRITE=10m COMPACT_MOVE=128m SYNC_GROW=128m MAIL_INTERVAL=5s SEND_INTERVAL=30s
    test "$SMOKE" = no || export LOGON_IO=16m SORT_READ=8m SORT_WRITE=2m COMPACT_MOVE=8m SYNC_GROW=8m
    mkdir -p "$RESULTS_DIR"
    for u in $USERS_LIST; do
        # New files for each user count: 05 grows them and 06 cuts them.
        rm -rf "$TARGET_DIR"
        mkdir -p "$TARGET_DIR"
        USERS="$u" "$mb/run.sh" "$mb/00-prepare.fio" || { failed=$((failed + 1)) && continue; }
        for s in $MAILBOX; do
            point="mailbox-${s#*-}-u$u"
            if test "$s" = 03-folder-resort; then
                # One user whatever USERS says, so it runs once.
                test "$first" = yes || continue
                point="mailbox-${s#*-}-u1"
            fi
            drop_caches
            t0="$EPOCHREALTIME"
            if USERS="$u" "$mb/run.sh" "$mb/$s.fio"; then
                # The wall time is what a fixed-work scenario scores: until its last user is done.
                awk -v p="$point" -v a="$t0" -v b="$EPOCHREALTIME" 'BEGIN { printf "%s\t%.3f\n", p, b - a }' >>"$RESULTS_DIR/wall.tsv"
            else
                failed=$((failed + 1))
            fi
        done
        first=no
    done
    rm -rf "$TARGET_DIR"
    test "$failed" -eq 0
}

files() {
    local set bs dir out="$RES/files" jobs=4 failed=0
    mkdir -p "$out"
    for set in $FILE_SETS; do
        bs="$set"
        test "$set" != 1792k || bs=256k
        dir="$DIR/files-$set"
        mkdir -p "$dir"
        drop_caches
        # shellcheck disable=SC2016 # $jobnum and $filenum are fio's
        fio --name="files-$set-create" --directory="$dir" --filename_format='f.$jobnum.$filenum' \
            --rw=write --bs="$bs" --filesize="$set" --nrfiles=$((FILES / jobs)) --numjobs="$jobs" \
            --openfiles=1 --file_service_type=sequential --create_on_open=1 --fsync_on_close=1 --ioengine=psync \
            --group_reporting --output-format=json --output="$out/files-$set-create.json" || failed=$((failed + 1))
        drop_caches
        # shellcheck disable=SC2016
        fio --name="files-$set-read" --directory="$dir" --filename_format='f.$jobnum.$filenum' \
            --rw=read --bs="$bs" --filesize="$set" --nrfiles=$((FILES / jobs)) --numjobs="$jobs" \
            --openfiles=1 --file_service_type=sequential --ioengine=psync \
            --group_reporting --output-format=json --output="$out/files-$set-read.json" || failed=$((failed + 1))
        rm -rf "$dir"
    done
    test "$failed" -eq 0
}

# post NIGHT: posts NIGHT's summary by the rules above. score.py's NIGHT/alert says new, same or clear,
# and WORK/slack-ts keeps the open alert's message.
post() {
    local text ts
    text="$(cat "$1/slack.txt")"
    ts="$(cat "$WORK/slack-ts" 2>/dev/null || true)"
    # shellcheck disable=SC2034 # report.sh reads them
    HERE="$TREE/test/scenarios" CACHE="$WORK"
    # shellcheck source-path=SCRIPTDIR source=../../test/scenarios/lib/report.sh
    . "$TREE/test/scenarios/lib/report.sh"
    case "$(cat "$1/alert" 2>/dev/null)" in
    new)
        if test -n "$ts"; then
            slack "$text" "$ts" >/dev/null
        else
            ts="$(slack "$text")"
            test -z "$ts" || echo "$ts" >"$WORK/slack-ts"
        fi
        ;;
    clear)
        test -n "$ts" || return 0
        slack ":white_check_mark: *Nightly performance: nothing flagged or failing any more*
$text" "$ts" yes >/dev/null
        rm -f "$WORK/slack-ts"
        ;;
    esac
}

case "${1:-}" in
build | start | setup_boot | matrix | mailbox | files | post) "$@"; exit ;;
"") ;;
*) echo "usage: night.sh" >&2; exit 2 ;;
esac

test "$(id -u)" -eq 0 || { echo "night.sh: run it as root" >&2; exit 2; }
if test -z "${HARNESS:-}"; then
    branch="$(git -C "$REPO" symbolic-ref --short HEAD)" || { echo "night.sh: $REPO is on no branch" >&2; exit 2; }
    git -C "$REPO" fetch -q origin develop "$branch" || echo "night.sh: the fetch failed, running what was fetched last" >&2
    HARNESS="$(git -C "$REPO" rev-parse "origin/$branch")" DEVELOP="$(git -C "$REPO" rev-parse origin/develop)"
    tree="$WORK/harness/$HARNESS"
    if test ! -d "$tree"; then
        mkdir -p "$tree.$$"
        git -C "$REPO" archive "$HARNESS" bench test/scenarios/lib | tar -x -C "$tree.$$"
        mv -T "$tree.$$" "$tree" 2>/dev/null || rm -rf "$tree.$$"
    fi
    touch "$tree"
    export REPO HARNESS DEVELOP
    exec "$tree/bench/ditto/night.sh" "$@"
fi
test -d "$LOCK" || { echo "night.sh: no lock directory $LOCK" >&2; exit 2; }
DEADLINE="$(date -d "${STOP_AT:-05:45}" +%s)"
test "$DEADLINE" -gt "$(date +%s)" || DEADLINE="$(date -d "tomorrow ${STOP_AT:-05:45}" +%s)"
RESERVE=600 # what the hard stop leaves for the cleanup and the summary
# A night's results are in <date>/, the history score.py compares with; a smoke run or a second run
# the same day goes elsewhere.
NIGHT="$WORK/$(date +%F)"
test "$SMOKE" = no || NIGHT="$WORK/smoke-$(date +%F-%H%M%S)"
test ! -e "$NIGHT" || NIGHT="$WORK/$(date +%F-%H%M%S)"
mkdir -p "$NIGHT"
export NIGHT RUN="$WORK/run" CALIBRATION
date +%s >"$NIGHT/started"
echo "$HARNESS" >"$NIGHT/harness"
echo "night.sh: $NIGHT"
exec >>"$NIGHT/night.log" 2>&1
log "start: harness $HARNESS, calibration $CALIBRATION, smoke $SMOKE, $REPEATS repeat(s), matrix files $FILE_SIZE, hard stop $(date -d "@$DEADLINE" '+%F %T')"
printf 'step\ttarget\trepeat\tstatus\tseconds\n' >"$NIGHT/steps.tsv"

# step NAME TARGET REPEAT BOX [ARGS]: runs night.sh NAME [ARGS] in a time box cut to end before the
# hard stop, and records it in steps.tsv: ok, fail, timeout (killed at the box) or skipped (no time).
step() {
    local name="$1" target="$2" repeat="$3" box="$4" left status=ok rc=0 t0="$EPOCHREALTIME"
    shift 4
    left=$((DEADLINE - RESERVE - $(date +%s)))
    if test "$left" -lt 60; then
        status=skipped
    else
        test "$box" -le "$left" || box="$left"
        log "$name $target r$repeat: box ${box}s"
        timeout --kill-after=30 "$box" "$HERE/night.sh" "$name" "$@" || rc=$?
        case "$rc" in 0) ;; 124 | 137) status=timeout ;; *) status=fail ;; esac
    fi
    awk -v OFS='\t' -v n="$name" -v t="$target" -v r="$repeat" -v s="$status" -v a="$t0" -v b="$EPOCHREALTIME" \
        'BEGIN { print n, t, r, s, sprintf("%.1f", b - a) }' >>"$NIGHT/steps.tsv"
    log "$name $target r$repeat: $status"
    test "$status" = ok
}

host_state() {
    {
        date '+%F %T %Z'
        uname -srm
        echo "load $(cut -d' ' -f1-3 /proc/loadavg), $(nproc) cpus, $(who | wc -l) logged in"
        free -h | sed -n 2p
        df -h "$DATA" "$WORK" | tail -n +2
        fio --version
        smbd --version
        go version
        test ! -x "$WORK/build/bin/dfs" || "$WORK/build/bin/dfs" version
        grep -A1 '^md' /proc/mdstat || true
        ps -eo pcpu,rss,user,comm --sort=-pcpu | head -6
    } >"$NIGHT/host-$1.txt" 2>&1
    local d
    for d in /dev/nvme[0-9]; do
        if test -e "$d" && command -v nvme >/dev/null; then
            nvme smart-log "$d" -o json >"$NIGHT/nvme-$1-${d##*/}.json" 2>/dev/null || true
        fi
    done
}

# stop TARGET: unmounts it, stops its server and empties its data.
stop() {
    local mnt="$MNT/$1" pid
    timeout 60 umount "$mnt" 2>/dev/null || umount -l "$mnt" 2>/dev/null || true
    pid="$(cat "$RUN/pid" 2>/dev/null || true)"
    # SIGKILL: a graceful dfs stop would wait for uploads that never happen. Anything else started
    # from this run's configuration goes too.
    test -z "$pid" || kill -KILL -- "-$pid" 2>/dev/null || kill -KILL "$pid" 2>/dev/null || true
    pkill -KILL -f -- "--config $RUN/config.yaml" 2>/dev/null || true
    pkill -KILL -f -- "--configfile=$HERE/smb.conf" 2>/dev/null || true
    for _ in $(seq 50); do
        pgrep -f -- "--config $RUN/config.yaml|--configfile=$HERE/smb.conf" >/dev/null || break
        sleep 0.2
    done
    test "$1" != samba || cp "$WORK/samba/smbd.log" "$RES/" 2>/dev/null || true
    rm -rf "${DATA:?}/$1" "$RUN" "$WORK/samba"
    STARTED=""
}

finish() {
    local rc=$?
    set +e
    test -z "$STARTED" || stop "$STARTED"
    test -z "$STATE" || host_state after
    rm -rf "$WORK/build" "$RUN"
    if ! python3 "$HERE/score.py" "$NIGHT" "$REPO" >"$NIGHT/slack.txt"; then
        echo ":x: Nightly performance: scoring failed, see $NIGHT/night.log" >"$NIGHT/slack.txt"
        echo new >"$NIGHT/alert"
    fi
    if test "$SMOKE" = no && test -s "$NIGHT/points.csv"; then
        test -s "$WORK/trend.csv" || head -1 "$NIGHT/points.csv" >"$WORK/trend.csv"
        tail -n +2 "$NIGHT/points.csv" >>"$WORK/trend.csv"
    fi
    test "$CALIBRATION" = yes || test "$SMOKE" = yes || "$HERE/night.sh" post "$NIGHT"
    log "end ($rc): $(head -1 "$NIGHT/slack.txt")"
}
STARTED="" STATE=""
trap finish EXIT

# Results and fetched trees older than 30 days go (the tree in use was touched as it started).
find "$WORK" -mindepth 1 -maxdepth 1 -type d \( -name '20??-??-??*' -o -name 'smoke-*' \) -mtime +30 -exec rm -rf {} +
find "$WORK/harness" -mindepth 1 -maxdepth 1 -type d -mtime +30 -exec rm -rf {} + 2>/dev/null || true

exec 9<"$LOCK"
if ! flock -w "${LOCK_WAIT:-1800}" 9; then
    printf 'lock\t-\t-\tskipped\t0\n' >>"$NIGHT/steps.tsv"
    log "not run: $LOCK stayed locked"
    exit 75
fi
step build - - 900 || true
host_state before
STATE=yes

DAY=$(($(date +%s) / 86400))
if test -x "$WORK/build/bin/dfs"; then
    for repeat in $(seq "$REPEATS"); do
        order=(dittofs samba)
        test $(((DAY + repeat) % 2)) -eq 1 || order=(samba dittofs)
        for target in "${order[@]}"; do
            test "$SMOKE" = yes || timeout 300 fstrim -v "$DATA" || true
            export DIR="$MNT/$target" RES="$NIGHT/r$repeat/$target" PASSWORD
            PASSWORD="$(secret)"
            rm -rf "${DATA:?}/$target" "$RUN"
            STARTED="$target"
            if step start "$target" "$repeat" 420 "$target"; then
                for s in matrix mailbox files; do step "$s" "$target" "$repeat" "$BOX" || true; done
            fi
            stop "$target"
        done
    done
fi
