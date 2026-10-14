#!/usr/bin/env bash
# The environment commands, which dt hands to this script with their arguments
# (docs/internals/rfc-test-harness.md, section 4.4). They start and stop the
# containers the suites use, so they stay shell.
#
#   dt services up|down|status [localstack|postgres|kmip]
#   dt stack up [--memory]|down [-v]|status|logs|mount|umount
#   dt cleanup [--volumes]
#   dt hooks install [--container]|uninstall|status
#
# Their state, such as the KMIP certificates and environment and the stack's
# dfsctl login, is kept in
# test/harness/state, which git ignores. DITTOFS_REPO points them at another
# checkout. Runs under bash 3.2 too, so empty arrays are guarded.
set -euo pipefail

HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="${DITTOFS_REPO:-$(cd "$HARNESS/../.." && pwd)}"
STATE="$HARNESS/state"
mkdir -p "$STATE"
OS="$(uname -s)"

# dtc's image sets DT_IN_CONTAINER=1. A dtc container shares the host's network
# when DT_HOSTNET=1; otherwise it reaches the ports the host publishes through
# host.docker.internal.
IN_CONTAINER=false
[[ "${DT_IN_CONTAINER:-}" == 1 ]] && IN_CONTAINER=true
HOSTNET=true
$IN_CONTAINER && [[ "${DT_HOSTNET:-}" != 1 ]] && HOSTNET=false

# --- services, as the suites expect them ----------------------------------------
# Images by full name: Docker reads a short name as Docker Hub's, but Podman
# resolves short names only through registries.conf and refuses them without it.
LOCALSTACK_NAME=dittofs-localstack
LOCALSTACK_IMAGE=docker.io/localstack/localstack:4.13.1
PG_NAME=dittofs-harness-pg-it         # the integration tests' postgres/postgres on 15432
PG_IMAGE=docker.io/library/postgres:16
PG_PORT=15432
KMIP_NAME=dittofs-pykmip
SERVICES=(localstack postgres kmip)

# --- output ----------------------------------------------------------------------
if [[ -t 1 ]]; then C_G=$'\033[0;32m' C_Y=$'\033[1;33m' C_R=$'\033[0;31m' C_0=$'\033[0m'
else C_G='' C_Y='' C_R='' C_0=''; fi
info() { echo "${C_G}[dt]${C_0} $*"; }
warn() { echo "${C_Y}[dt]${C_0} $*" >&2; }
err()  { echo "${C_R}[dt]${C_0} $*" >&2; }
die()  { err "$*"; exit 2; }

# --- ports ---------------------------------------------------------------------------
# On Linux, ss asks the kernel for every socket in the network namespace; lsof
# finds sockets through processes and misses other users' and other PID
# namespaces'. macOS has no ss, and its lsof sees Docker's published ports.
use_ss() { [[ "$OS" == Linux ]] && command -v ss >/dev/null 2>&1; }
port_busy() {
    if use_ss; then ss -Hltn "sport = :$1" 2>/dev/null | grep -q .
    else lsof -w -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1; fi
}
# A port published on the Docker host: from a bridge-network dtc container only
# a connect to host.docker.internal can tell; sharing the host's network, the
# local check sees Docker's published ports.
dockerhost_port_busy() {
    if $HOSTNET; then port_busy "$1"; else nc -z -w 1 host.docker.internal "$1" >/dev/null 2>&1; fi
}
listeners() { # listeners PORT...: the listening sockets on those ports, for messages
    local re; re="$(IFS='|'; echo "$*")"
    if use_ss; then ss -ltnp | awk -v re=":($re)$" 'NR==1 || $4 ~ re'
    else lsof -w -nP -iTCP -sTCP:LISTEN | awk -v re=":($re) " 'NR==1 || $0 ~ re'; fi
}
owner_of() { stat -f %Su "$1" 2>/dev/null || stat -c %U "$1" 2>/dev/null; }  # BSD, then GNU

docker_ok() {
    docker info >/dev/null 2>&1 && return 0
    if [[ "$OS" == Darwin ]]; then die "Docker is not running. Start Docker Desktop (open -a Docker)."
    else die "Docker is not reachable. Start the daemon (sudo systemctl start docker), or check DOCKER_HOST / 'docker context ls'."; fi
}

# --- services ---------------------------------------------------------------------
container_running() { [[ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" == true ]]; }

wait_for() { # wait_for DESC TRIES CMD...
    local desc="$1" tries="$2" i; shift 2
    for ((i = 1; i <= tries; i++)); do "$@" >/dev/null 2>&1 && return 0; sleep 2; done
    die "$desc did not become ready"
}

# docker_pull IMAGE: pull with retries, unless the image is already here, so a
# transient registry error doesn't fail the run before any test starts.
docker_pull() {
    docker image inspect "$1" >/dev/null 2>&1 && return 0
    local attempt
    for attempt in 1 2 3 4 5; do
        docker pull -q "$1" >/dev/null && return 0
        warn "docker pull $1 failed (attempt $attempt), retrying"
        sleep $((attempt * 5))
    done
    die "docker pull $1 failed after 5 attempts"
}

svc_localstack_up() {
    if container_running "$LOCALSTACK_NAME"; then info "localstack already running"; return; fi
    docker rm -f "$LOCALSTACK_NAME" >/dev/null 2>&1 || true
    if dockerhost_port_busy 4566; then die "port 4566 is taken by something else"; fi
    docker_pull "$LOCALSTACK_IMAGE"
    docker run -d --name "$LOCALSTACK_NAME" -p 127.0.0.1:4566:4566 \
        -e SERVICES=s3 -e EAGER_SERVICE_LOADING=1 -e GATEWAY_LISTEN=0.0.0.0:4566 \
        -e DISABLE_CORS_CHECKS=1 -e DISABLE_CUSTOM_CORS_S3=1 -e DEBUG=0 -e LOCALSTACK_HOST=localhost:4566 \
        "$LOCALSTACK_IMAGE" >/dev/null
    wait_for "localstack" 60 localstack_ready
    info "localstack S3 on http://127.0.0.1:4566"
}
localstack_ready() { curl -s http://127.0.0.1:4566/_localstack/health | grep -Eq '"s3": "(available|running)"'; }

svc_postgres_up() {
    if container_running "$PG_NAME"; then info "postgres already running"; return; fi
    docker rm -f "$PG_NAME" >/dev/null 2>&1 || true
    if dockerhost_port_busy "$PG_PORT"; then die "port $PG_PORT is taken by something else"; fi
    docker_pull "$PG_IMAGE"
    docker run -d --name "$PG_NAME" -p "127.0.0.1:${PG_PORT}:5432" \
        -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=postgres -e POSTGRES_DB=dittofs_test \
        "$PG_IMAGE" >/dev/null
    # Over TCP: the init-time server answers on its Unix socket before the port is up.
    wait_for "postgres" 30 docker exec "$PG_NAME" pg_isready -h 127.0.0.1 -U postgres
    info "postgres on 127.0.0.1:${PG_PORT} postgres/postgres db dittofs_test"
}

# The steps of test/kmip/start-pykmip.sh (certificates, pinned PyKMIP, readiness,
# key provisioning, environment file), except that the port is published on
# 127.0.0.1 only, never on every interface of a laptop's network. The image and
# version are read from that script, so a bump there is picked up here.
svc_kmip_up() {
    # Reuse a running server only with its environment complete and its
    # certificates, which live two days, good for another hour.
    if container_running "$KMIP_NAME" && [[ -s "$STATE/kmip.env" ]] &&
        openssl x509 -checkend 3600 -noout -in "$STATE/kmip-certs/server-cert.pem" >/dev/null 2>&1; then
        info "pykmip already running"; return
    fi
    rm -f "$STATE/kmip.env"
    if dockerhost_port_busy 5696 && ! container_running "$KMIP_NAME"; then die "port 5696 is taken by something else"; fi
    local kdir="$REPO/test/kmip" image version certs
    image="$(kmip_pin IMAGE "$kdir/start-pykmip.sh")"
    # The script names a Docker Hub image by its short name; see LOCALSTACK_IMAGE.
    case "$image" in */*/*|*.*/*|*:*/*) ;; */*) image="docker.io/$image" ;; *) image="docker.io/library/$image" ;; esac
    version="$(kmip_pin PYKMIP_VERSION "$kdir/start-pykmip.sh")"
    [[ -n "$image" && -n "$version" ]] || die "could not read IMAGE and PYKMIP_VERSION from $kdir/start-pykmip.sh"
    certs="$("$kdir/gen-certs.sh" "$STATE/kmip-certs")"
    certs="$(cd "$certs" && pwd)"
    docker rm -f "$KMIP_NAME" >/dev/null 2>&1 || true
    docker_pull "$image"
    docker run -d --name "$KMIP_NAME" -p 127.0.0.1:5696:5696 \
        -v "$certs:/certs:ro" -v "$kdir:/work:ro" "$image" \
        sh -c "pip install --no-cache-dir pykmip==$version && mkdir -p /etc/pykmip/policies && exec pykmip-server -f /work/server.conf" \
        >/dev/null
    wait_for "pykmip" 60 docker exec "$KMIP_NAME" python -c \
        'import socket,sys; s=socket.socket(); sys.exit(s.connect_ex(("127.0.0.1",5696)))'
    # Written whole or not at all: a failed provisioning must not leave a file
    # that says the server is ready.
    {
        echo "DITTOFS_TEST_KMIP=1"
        echo "DITTOFS_TEST_KMIP_ENDPOINT=127.0.0.1:5696"
        echo "DITTOFS_TEST_KMIP_CERT=$certs/client-cert.pem"
        echo "DITTOFS_TEST_KMIP_KEY=$certs/client-key.pem"
        echo "DITTOFS_TEST_KMIP_CA=$certs/ca-cert.pem"
        docker exec "$KMIP_NAME" python /work/provision.py
    } >"$STATE/kmip.env.tmp"
    mv "$STATE/kmip.env.tmp" "$STATE/kmip.env"
    info "pykmip $version on 127.0.0.1:5696 only (environment: $STATE/kmip.env)"
}

# kmip_pin NAME FILE: the value FILE assigns to NAME, without quotes.
kmip_pin() { sed -n "s/^$1=//p" "$2" | head -n 1 | tr -d "\"'" | sed 's/[[:space:]].*//'; }

svc_name() { # the container of service $1
    case "$1" in localstack) echo "$LOCALSTACK_NAME" ;; postgres) echo "$PG_NAME" ;; kmip) echo "$KMIP_NAME" ;; esac
}

svc_status() {
    printf '%-12s %-24s %-10s %s\n' SERVICE CONTAINER STATE PORTS
    local s n state ports
    for s in "${SERVICES[@]}"; do
        n="$(svc_name "$s")"
        state="$(docker inspect -f '{{.State.Status}}' "$n" 2>/dev/null)" || state=absent
        ports="$(docker port "$n" 2>/dev/null | tr '\n' ' ' || true)"
        printf '%-12s %-24s %-10s %s\n' "$s" "$n" "$state" "$ports"
    done
}

cmd_services() {
    local usage="usage: dt services up|down|status [localstack|postgres|kmip]"
    local action="${1:-status}"; shift || true
    case "$action" in up|down|status) ;; *) die "$usage" ;; esac
    local which=("$@") s
    ((${#which[@]})) || which=("${SERVICES[@]}")
    for s in "${which[@]}"; do
        [[ -n "$(svc_name "$s")" ]] || die "unknown service: $s ($usage)"
    done
    docker_ok
    case "$action" in
        up)
            for s in "${which[@]}"; do "svc_${s}_up"; done ;;
        down)
            for s in "${which[@]}"; do
                docker rm -f "$(svc_name "$s")" >/dev/null 2>&1 || true
                [[ "$s" == kmip ]] && rm -f "$STATE/kmip.env"
                info "stopped $s"
            done ;;
        status) svc_status ;;
    esac
}

# --- compose stack (dfs and Localstack S3, NFS and SMB) -------------------------
STACK_ADMIN_PW=dittofs-compose-dev-password
STACK_USER=dev
STACK_USER_PW=dittofs-dev-password-123
NFS_MNT="$HOME/mnt/dittofs-nfs"
SMB_MNT="$HOME/mnt/dittofs-smb"
# Ports the stack, the SMB conformance stack and a host dfs all bind.
STACK_PORTS=(8080 12049 12445)
# The stack publishes on the Docker host: localhost natively and in a
# host-network dtc container, host.docker.internal from a bridge-network one.
STACK_HOST=127.0.0.1
$HOSTNET || STACK_HOST=host.docker.internal
# Linux needs root to mount; macOS mounts user-owned mount points without it.
as_root() { if ((EUID == 0)); then "$@"; else sudo "$@"; fi; }

stack_compose() { (cd "$REPO" && docker compose "$@"); }

# The stack's ports must be free on the Docker host before it starts.
stack_ports_free() {
    local busy=() p
    for p in "${STACK_PORTS[@]}"; do
        if dockerhost_port_busy "$p"; then busy+=("$p"); fi
    done
    ((${#busy[@]})) || return 0
    if $HOSTNET; then
        err "ports in use: ${busy[*]} (compose stack or another dfs is running)."
        listeners "${busy[@]}" >&2 || true
    else
        err "ports in use on the Docker host: ${busy[*]} (compose stack, SMB stack, or a dfs on the host)."
        docker ps --format '{{.Names}}  {{.Ports}}' | grep -E ':(8080|12049|12445)->' >&2 || true
    fi
    die "stop it first: 'dt stack down', or 'sudo $REPO/test/posix/teardown-posix.sh'."
}

cmd_stack() {
    local usage="usage: dt stack up [--memory]|down [-v]|status|logs|mount|umount"
    local action="${1:-status}"; shift || true
    local profile=s3-backend
    case "$action:$#:${1:-}" in
        up:0:|down:0:|down:1:-v|status:0:|logs:0:|mount:0:|umount:0:) ;;
        up:1:--memory) profile=default ;;
        *) die "$usage" ;;
    esac
    if $IN_CONTAINER && [[ "$action" == mount || "$action" == umount ]]; then
        # A mount made here lives and dies with this container's own mount
        # namespace, where no host program can see it.
        info "mounts belong on the host: run 'dt stack $action' there"
        info "(inside 'dtc shell' you can also mount -t nfs|cifs from $STACK_HOST, for this container only)"
        return 0
    fi
    docker_ok
    case "$action" in
        up)
            if ! stack_compose ps --status running --services 2>/dev/null | grep -qx dittofs; then
                stack_ports_free
            fi
            info "starting compose stack (profile: $profile)"
            if [[ "$profile" == default ]]; then stack_compose up -d --build
            else COMPOSE_PROFILES="$profile" stack_compose up -d --build; fi
            wait_for "bootstrap" 90 bootstrap_done
            stack_compose logs bootstrap 2>/dev/null | tail -3
            stack_dev_user
            info "stack up: API http://127.0.0.1:8080  NFS 127.0.0.1:12049  SMB 127.0.0.1:12445  (admin / $STACK_ADMIN_PW)"
            ;;
        down)
            $IN_CONTAINER || cmd_stack umount || true
            stack_compose --profile s3-backend down "$@"
            info "stack down (dt stack down -v also drops its volumes)"
            ;;
        status) stack_compose --profile s3-backend ps -a ;;
        logs) stack_compose logs --tail=200 dittofs ;;
        mount)
            mkdir -p "$NFS_MNT" "$SMB_MNT"
            if mount | grep -q " on $NFS_MNT "; then info "NFS already mounted at $NFS_MNT"
            elif [[ "$OS" == Darwin ]]; then
                mount -t nfs -o vers=3,tcp,port=12049,mountport=12049,nolocks,noresvport 127.0.0.1:/export "$NFS_MNT" \
                    && info "NFSv3 mounted at $NFS_MNT"
            else
                as_root mount -t nfs -o vers=3,tcp,port=12049,mountport=12049,nolock 127.0.0.1:/export "$NFS_MNT" \
                    && info "NFSv3 mounted at $NFS_MNT"
            fi
            if mount | grep -q " on $SMB_MNT "; then info "SMB already mounted at $SMB_MNT"
            elif [[ "$OS" == Darwin ]]; then
                mount_smbfs -N "//${STACK_USER}:${STACK_USER_PW}@127.0.0.1:12445/export" "$SMB_MNT" \
                    && info "SMB mounted at $SMB_MNT (user $STACK_USER)"
            else
                # The files show as yours: uid and gid map the mount to the caller.
                as_root mount -t cifs -o "username=$STACK_USER,password=$STACK_USER_PW,port=12445,vers=3.0,uid=$(id -u),gid=$(id -g)" \
                    //127.0.0.1/export "$SMB_MNT" && info "SMB mounted at $SMB_MNT (user $STACK_USER)"
            fi
            ;;
        umount)
            local m
            for m in "$NFS_MNT" "$SMB_MNT"; do
                mount | grep -q " on $m " || continue
                if [[ "$OS" == Darwin ]]; then { umount "$m" || diskutil unmount force "$m"; } && info "unmounted $m"
                else { as_root umount "$m" || as_root umount -f -l "$m"; } && info "unmounted $m"; fi
            done; true ;;
    esac
}

bootstrap_done() { stack_compose ps -a bootstrap --format '{{.State}} {{.ExitCode}}' | grep -q 'exited 0'; }

# An SMB login needs a real user with a password; the stack's bootstrap only
# creates the share.
stack_dev_user() {
    local bin uid
    # Built each time, so it matches the checkout; a binary per platform, since
    # the host and a dtc container share the checkout.
    bin="$STATE/bin/$(uname -s)-$(uname -m)/dfsctl"
    (cd "$REPO" && go build -o "$bin" ./cmd/dfsctl)
    export XDG_CONFIG_HOME="$STATE/dfsctl"
    "$bin" login --server "http://$STACK_HOST:8080" --username admin --password "$STACK_ADMIN_PW" >/dev/null
    if ! "$bin" user get "$STACK_USER" >/dev/null 2>&1; then
        # The host's uid, which dtc passes in, so files made through the host's
        # mounts are the caller's.
        uid="${DT_HOST_UID:-$(id -u)}"
        "$bin" user create --username "$STACK_USER" --password "$STACK_USER_PW" --uid "$uid" >/dev/null
        info "created SMB user '$STACK_USER' (uid $uid)"
    fi
    "$bin" share permission grant /export --user "$STACK_USER" --level read-write >/dev/null 2>&1 || true
}

# --- cleanup -------------------------------------------------------------------------
# A host dfs that test/posix/setup-posix.sh started and a killed run left behind:
# keep the server's log, stop the server by the PID file it wrote, never by name,
# and clear the /tmp state it leaves. /tmp is everyone's and a PID can be reused,
# so it signals only a PID from a file of the caller's that names a process
# still running `dfs ... start`.
host_teardown() {
    local pidf=/tmp/dittofs-server.pid pid p
    if [[ -f "$pidf" && -O "$pidf" ]]; then
        pid="$(cat "$pidf")"
        if [[ "$pid" =~ ^[1-9][0-9]*$ ]] && is_dfs_start "$pid"; then
            kill -TERM "$pid" 2>/dev/null || true
            for _ in 1 2 3 4 5; do kill -0 "$pid" 2>/dev/null || break; sleep 1; done
            if kill -0 "$pid" 2>/dev/null; then kill -KILL "$pid" 2>/dev/null || true; fi
            info "stopped host dfs (pid $pid)"
        fi
    fi
    [[ -f /tmp/dittofs-posix-server.log ]] && cp /tmp/dittofs-posix-server.log "$STATE/dfs-server-$(date +%Y%m%d-%H%M%S).log"
    for p in /tmp/dittofs-posix-* /tmp/dittofs-admin-password "$pidf"; do
        [[ -e "$p" ]] || continue
        if [[ -O "$p" ]]; then rm -rf "$p"
        else warn "$p is owned by $(owner_of "$p"); clear it with: sudo $REPO/test/posix/teardown-posix.sh"; fi
    done
}

# is_dfs_start PID: the process runs a dfs binary with start among its arguments.
is_dfs_start() {
    local cmd
    cmd="$(ps -p "$1" -o command= 2>/dev/null)" || return 1
    [[ "$cmd" =~ ^([^[:space:]]*/)?dfs([[:space:]].*)?[[:space:]]start([[:space:]]|$) ]]
}

# An SMB conformance stack a killed run left behind (its own cleanup tears down
# every profile, but a SIGKILL skips it), which makes every later run refuse with
# "another instance of this stack is live".
smb_stack_down() {
    local dir="$REPO/test/smb-conformance" proj
    proj="$(docker ps -a --format '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.project.working_dir"}}' 2>/dev/null |
        awk -F'|' -v d="$dir" '$1 ~ /^smb-conformance/ && $2 == d {print $1; exit}')"
    [[ -n "$proj" ]] || return 0
    (cd "$dir" && docker compose -p "$proj" --profile '*' down -v >/dev/null 2>&1) &&
        info "removed leftover SMB conformance stack $proj"
}

# Stop everything the harness can leave running: a host dfs, the compose stack
# and its mounts, a leftover SMB conformance stack and the services. --volumes
# also drops the stack's data volumes. dtc sessions are listed, not stopped.
cmd_cleanup() {
    local volumes=false
    case "$#:${1:-}" in
        0:) ;;
        1:--volumes) volumes=true ;;
        *) die "usage: dt cleanup [--volumes]" ;;
    esac
    docker_ok
    host_teardown
    if docker ps -a --format '{{.Label "com.docker.compose.project"}}' | grep -qx dittofs; then
        $IN_CONTAINER || cmd_stack umount || true
        if $volumes; then stack_compose --profile s3-backend down -v >/dev/null 2>&1
        else stack_compose --profile s3-backend down >/dev/null 2>&1; fi
        info "stopped the compose stack$($volumes && echo ' and dropped its volumes')"
    fi
    smb_stack_down
    cmd_services down
    local left
    left="$(docker ps --format '{{.Names}}' | grep -E '^(dittofs-|dt-|dtc-|smb-conformance-)' |
        grep -vxF "${DT_CONTAINER_NAME:-none}" || true)"
    if [[ -n "$left" ]]; then warn "still running: $(tr '\n' ' ' <<<"$left")"; else info "no harness containers running"; fi
}

# --- git hooks ---------------------------------------------------------------------
# install runs the repository's own .githooks on this machine; --container points
# git at test/harness/githooks, which run them in the dtc container instead, so
# the machine needs no Go toolchain. uninstall is install. Both paths are
# relative, so each worktree runs its own hooks; a branch that predates the
# harness has no harness hooks to run.
cmd_hooks() {
    local usage="usage: dt hooks install [--container]|uninstall|status"
    local action="${1:-status}" path=.githooks
    case "$action:$#:${2:-}" in
        install:1:|uninstall:1:|status:0:|status:1:) ;;
        install:2:--container) path=test/harness/githooks ;;
        *) die "$usage" ;;
    esac
    case "$action" in
        install|uninstall)
            git -C "$REPO" config core.hooksPath "$path"
            info "core.hooksPath -> $path" ;;
        status)
            echo "core.hooksPath = $(git -C "$REPO" config --get core.hooksPath || echo '(unset)')" ;;
    esac
}

case "${1:-}" in
    services) shift; cmd_services "$@" ;;
    stack) shift; cmd_stack "$@" ;;
    cleanup) shift; cmd_cleanup "$@" ;;
    hooks) shift; cmd_hooks "$@" ;;
    *) die "usage: env.sh services|stack|cleanup|hooks [ARGS]" ;;
esac
