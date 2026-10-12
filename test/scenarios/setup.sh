#!/usr/bin/env bash
# Sets up DittoFS in a throwaway rootless podman container, then opens a shell in it or runs
# scenarios, each in its own container. Silent unless a scenario fails.
#   test/scenarios/setup.sh                       a bash shell in the set-up container
#   test/scenarios/setup.sh a.sh                  one scenario
#   test/scenarios/setup.sh a.sh b.sh | all       several; one PASS, FAIL or SLOW line each
# CACHE (/var/tmp/dittofs-scenarios) keeps the Go caches and the kept logs. How a run is reported,
# with its timing and Slack messages, is in lib/report.sh. SCENARIO_TIMEOUT caps a run in seconds
# (default 900). CACHE is the one volume, at /cache; CACHE/inputs is at /inputs, for large inputs
# worth keeping, and the run's logs directory at /logs.
set -euo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
CACHE="${CACHE:-/var/tmp/dittofs-scenarios}"
# shellcheck source-path=SCRIPTDIR source=lib/report.sh
. "$HERE/lib/report.sh"
SCENARIOS=("$@")
if test "${SCENARIOS[0]:-}" = all; then
    SCENARIOS=()
    for F in "$HERE"/*.sh; do test "${F##*/}" = setup.sh || SCENARIOS+=("${F##*/}"); done
fi
for SCENARIO in "${SCENARIOS[@]}"; do test -f "$HERE/$SCENARIO"; done

mkdir -p "$CACHE/go/mod" "$CACHE/go/build" "$CACHE/failed" "$CACHE/slow" "$CACHE/inputs"
P=(podman --cgroup-manager cgroupfs --events-backend none)
"${P[@]}" pull -q --policy missing docker.io/library/ubuntu:26.04 docker.io/library/golang:1.26 docker.io/chrislusf/seaweedfs:4.48 >/dev/null
IMAGES=(--mount "type=image,source=docker.io/library/golang:1.26,destination=/opt/go"
    --mount "type=image,source=docker.io/chrislusf/seaweedfs:4.48,destination=/opt/seaweedfs"
    -v "$HERE/../..:/src:ro" -v "$CACHE:/cache")

# The setup that runs inside every container: tools, DittoFS, SeaweedFS, share /test.
SETUP="$(cat <<'SETUP'
set -euxo pipefail
export DEBIAN_FRONTEND=noninteractive
ln -s /cache/inputs /inputs
ln -s "/cache/$LOGS" /logs

# Downloads are kept in CACHE/downloads and reused: apt's packages, so a run fetches only the
# indexes, and the libnfs source, fetched once and checked every run.
mkdir -p /cache/downloads/apt/partial
echo 'Dir::Cache::Archives "/cache/downloads/apt";' >/etc/apt/apt.conf.d/99cache
rm -f /etc/apt/apt.conf.d/docker-clean

# Tools: SMB and NFS clients, rclone for S3, libnfs's nfs-io example (for NFS deletes) built from its
# sha256-checked source, and the scenario tools in lib/tools, the C ones built here.
apt-get update -qq
apt-get install -y -qq --no-install-recommends ca-certificates curl jq rclone smbclient libnfs-utils libnfs-dev libsmb2-dev libc6-dev tcc >/dev/null
test -f /cache/downloads/libnfs-5.0.2.tar.gz || { curl -sfL -o /cache/downloads/libnfs-5.0.2.tar.gz.part https://github.com/sahlberg/libnfs/archive/refs/tags/libnfs-5.0.2.tar.gz && mv /cache/downloads/libnfs-5.0.2.tar.gz.part /cache/downloads/libnfs-5.0.2.tar.gz; }
echo '637e56643b19da9fba98f06847788c4dad308b723156a64748041035dcdf9bd3  /cache/downloads/libnfs-5.0.2.tar.gz' | sha256sum -c --quiet
tar xzf /cache/downloads/libnfs-5.0.2.tar.gz -C /tmp
tcc -o /usr/local/bin/nfs-io /tmp/libnfs-libnfs-5.0.2/examples/nfs-io.c -lnfs
for T in /src/test/scenarios/lib/tools/*; do
    case "$T" in *.c) tcc -o "/usr/local/bin/$(basename "$T" .c)" "$T" -lnfs -lsmb2 ;; *) install "$T" /usr/local/bin/ ;; esac
done

# DittoFS, built from this checkout.
cd /src
CGO_ENABLED=0 GOFLAGS=-buildvcs=false GOMODCACHE=/cache/go/mod GOCACHE=/cache/go/build /opt/go/usr/local/go/bin/go build -o /usr/local/bin/ ./cmd/dfs ./cmd/dfsctl
cd /

# A local S3 (SeaweedFS, run by s3 start) with bucket dittofs. rclone reaches it as s3:, and DittoFS
# block stores through the config in /etc/dittofs-s3.json.
export RCLONE_CONFIG_S3_TYPE=s3 RCLONE_CONFIG_S3_PROVIDER=SeaweedFS RCLONE_CONFIG_S3_ENDPOINT=http://127.0.0.1:9000 RCLONE_CONFIG_S3_ACCESS_KEY_ID=test RCLONE_CONFIG_S3_SECRET_ACCESS_KEY=test-secret RCLONE_LOG_LEVEL=ERROR
echo '{"identities":[{"name":"test","credentials":[{"accessKey":"test","secretKey":"test-secret"}],"actions":["Admin","Read","Write","List","Tagging"]}]}' >/tmp/seaweedfs-identities.json
mkdir /tmp/seaweedfs
s3 start
rclone mkdir s3:dittofs
echo '{"endpoint":"http://127.0.0.1:9000","access_key_id":"test","secret_access_key":"test-secret","allow_private_endpoint":true}' >/etc/dittofs-s3.json

# DittoFS: share /test (metadata store md, block store s3: bucket dittofs, prefix test/) for user
# tester, SMB on 445 and NFS on 2049. smbclient defaults to SMB3 and, through USER, to tester.
dfs init
dfs-server start
dfsctl store metadata add --name md --type badger --db-path /tmp/md
dfsctl store block add --name s3 --type s3 --config "$(jq -c '.bucket = "dittofs" | .prefix = "test/"' /etc/dittofs-s3.json)"
dfsctl share create --name /test --metadata md --block-store s3 --default-permission none
dfsctl user create --username tester --uid 1000 --gid 1000 --password first-tester-secret --role user
XDG_CONFIG_HOME=/tmp/tester dfsctl login --server http://127.0.0.1:8080 --username tester --password first-tester-secret
XDG_CONFIG_HOME=/tmp/tester dfsctl user change-password --current first-tester-secret --new tester-secret
dfsctl share permission grant /test --user tester --level read-write
dfsctl adapter enable smb --port 445
dfsctl adapter enable nfs --port 2049
mkdir -p /etc/samba
printf '[global]\nclient min protocol = SMB3\n' >/etc/samba/smb.conf
SETUP
)"

# No scenario: a shell in a set-up container, removed when the shell exits. It takes no lock and
# keeps its logs apart, so an open shell never blocks a scheduled run.
if test "${#SCENARIOS[@]}" -eq 0; then
    mkdir -p "$CACHE/shell/logs"
    exec "${P[@]}" run --rm -it "${IMAGES[@]}" -e LOGS=shell/logs \
        docker.io/library/ubuntu:26.04 bash -c "$SETUP
set +x
echo 'Set up: share /test, user tester, SMB on 445, NFS on 2049, logs in /logs.'
echo 'Run a scenario: scenario NAME (see ls /src/test/scenarios). Leave: exit.'
export USER=tester%tester-secret
exec bash"
fi

# One run at a time per CACHE. A second run fails rather than exiting 0: a scheduled run that tested
# nothing must not read as a pass. 75 (EX_TEMPFAIL) tells it apart from a scenario failure (1).
exec 9<"$CACHE"
flock -n 9 || { echo "not run: another run holds $CACHE" >&2; exit 75; }

# A scenario runs after the same setup. Each of its commands is traced as
# "+ <epoch seconds> <line> <command>".
NODE="$SETUP
$(cat <<'RUN'
USER=tester%tester-secret bash -c 'PS4="+ \$EPOCHREALTIME \$LINENO "; set -euxo pipefail; . /scenario.sh'
RUN
)"

FAILED=0
for SCENARIO in "${SCENARIOS[@]}"; do
    rm -rf "$CACHE/run"
    mkdir -p "$CACHE/run/logs"
    timeout "${SCENARIO_TIMEOUT:-900}" "${P[@]}" run --rm -i --log-driver k8s-file "${IMAGES[@]}" \
        -v "$HERE/$SCENARIO:/scenario.sh:ro" -e LOGS=run/logs \
        --name "dittofs-${SCENARIO%.sh}" --replace \
        docker.io/library/ubuntu:26.04 bash -s <<<"$NODE" >"$CACHE/run/run.log" 2>&1 && RESULT=pass || RESULT=fail
    report "$SCENARIO" "$RESULT"
    test "$RESULT" = pass || FAILED=$((FAILED + 1))
done
test "$FAILED" -eq 0
