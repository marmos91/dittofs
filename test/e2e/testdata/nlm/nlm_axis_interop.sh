#!/usr/bin/env bash
# Real NFSv3 NLM cross-protocol lock-interop driver (issue #1503).
#
# Proves DittoFS's unified lock manager coordinates byte-range locks across the
# NFSv3 NLM axis and the NFSv4 / SMB axes, using a REAL kernel NFSv3 client (no
# `nolock`). Requires dual network-namespace isolation — see
# docs/internals/testing.md ("Real NFSv3 NLM lock testing").
#
# Layout:
#   sns (server netns, 10.50.0.1) — dfs + its own rpcbind (private /run)
#   cns (client netns, 10.50.0.2) — kernel NFSv3 client + own rpcbind/statd
#   veth-s <-> veth-c joins them.
#
# Usage: nlm_axis_interop.sh <path-to-nlmlock.py>
# Emits one line per case:  "<CASE> conflict=<BLOCKED|ACQUIRED|...> afterRelease=<...>"
# and finally "NLM_AXIS_RESULT=PASS" or "=FAIL".
set -u

# System tools (ip, rpcbind, mount.nfs, mount.cifs, rpcinfo) live in sbin, which
# a restricted/Nix PATH may omit. Ensure they resolve.
export PATH="/usr/sbin:/sbin:/usr/bin:/bin:$PATH"

LOCKPY="${1:?path to nlmlock.py required}"
WORK="$(mktemp -d /tmp/nlm-e2e.XXXXXX)"
API=18080; NFSP=12049; SMBP=12445; PW=adminpassword
SIP=10.50.0.1; CIP=10.50.0.2
DFS="${DFS_BIN:-dfs}"; DFSCTL="${DFSCTL_BIN:-dfsctl}"

log() { echo "[nlm] $*"; }

clean_ns() {
  set +e
  for ns in sns cns; do
    ip netns pids "$ns" 2>/dev/null | xargs -r kill -9 2>/dev/null
  done
  ip netns del cns 2>/dev/null
  ip netns del sns 2>/dev/null
  ip link del veth-s 2>/dev/null
}
teardown() {
  clean_ns
  rm -rf "$WORK" 2>/dev/null
}
trap teardown EXIT

# --- 0. pre-clean leftovers from a prior aborted run (idempotent) ----------
clean_ns
sleep 0.3

# --- 1. netns + veth -------------------------------------------------------
ip netns add sns || exit 1
ip netns add cns || exit 1
ip link add veth-s type veth peer name veth-c || exit 1
ip link set veth-s netns sns
ip link set veth-c netns cns
ip netns exec sns ip addr add $SIP/24 dev veth-s
ip netns exec sns ip link set veth-s up
ip netns exec sns ip link set lo up
ip netns exec cns ip addr add $CIP/24 dev veth-c
ip netns exec cns ip link set veth-c up
ip netns exec cns ip link set lo up
ip netns exec cns ping -c1 -W2 $SIP >/dev/null 2>&1 && log "veth up" || { log "veth FAIL"; exit 1; }

# --- 2. server (dfs) in sns with a private /run + rpcbind ------------------
cat > "$WORK/config.yaml" <<CFG
logging: {level: INFO, format: text, output: stdout}
controlplane:
  port: $API
  jwt: {secret: "test-secret-key-for-e2e-testing-only-must-be-32-chars"}
database: {type: sqlite, sqlite: {path: "$WORK/dittofs.db"}}
CFG

ip netns exec sns unshare --mount bash -c "
  mount -t tmpfs tmpfs /run
  mkdir -p /run/rpcbind /run/sendsigs.omit.d
  rpcbind
  sleep 0.5
  export DITTOFS_ADMIN_INITIAL_PASSWORD=$PW
  exec $DFS start --foreground --config $WORK/config.yaml --pid-file $WORK/dfs.pid --log-file $WORK/dfs.log
" >"$WORK/dfs.out" 2>&1 &

for _ in $(seq 1 80); do
  ip netns exec sns curl -sf http://127.0.0.1:$API/health >/dev/null 2>&1 && { log "server ready"; break; }
  sleep 0.3
done
ip netns exec sns curl -sf http://127.0.0.1:$API/health >/dev/null 2>&1 || { log "server never ready"; tail -20 "$WORK/dfs.log" 2>/dev/null; exit 1; }

# --- 3. provision (dfsctl, inside sns so the API is reachable) -------------
NS="ip netns exec sns"
export XDG_CONFIG_HOME="$WORK/cfg"; mkdir -p "$XDG_CONFIG_HOME"
$NS env XDG_CONFIG_HOME="$XDG_CONFIG_HOME" $DFSCTL login --server "http://127.0.0.1:$API" --username admin --password "$PW" || { log "login FAIL"; exit 1; }
dctl() { $NS env XDG_CONFIG_HOME="$XDG_CONFIG_HOME" $DFSCTL "$@"; }

dctl store metadata add --name meta --type memory || { log "meta store FAIL"; exit 1; }
dctl store block local add --name blk --type memory || { log "block store FAIL"; exit 1; }
dctl share create --name /export --metadata meta --local blk --default-permission read-write || { log "share FAIL"; exit 1; }
dctl share nfs-config set /export --squash root_to_admin 2>/dev/null || dctl share nfs-config set /export --squash root_to_admin || true
dctl adapter settings nfs update --portmapper-register-with-system --udp-enabled || { log "nfs settings FAIL"; exit 1; }
dctl adapter enable nfs --port $NFSP || { log "nfs enable FAIL"; exit 1; }
dctl adapter enable smb --port $SMBP || { log "smb enable FAIL"; exit 1; }
sleep 1

# NLM (100021) must be registered in sns's rpcbind
if $NS rpcinfo -p $SIP 2>/dev/null | grep -q 100021; then
  log "NLM registered:"; $NS rpcinfo -p $SIP 2>/dev/null | grep 100021 | sed 's/^/[nlm]   /'
else
  log "NLM NOT registered in sns rpcbind"; $NS rpcinfo -p $SIP 2>/dev/null | sed 's/^/[nlm]   /'; exit 1
fi

# --- 4. client side: mounts + all cases in ONE cns/unshare shell -----------
ip netns exec cns unshare --mount bash -s "$LOCKPY" "$SIP" "$NFSP" "$SMBP" "$PW" <<'CLIENT'
set -u
LOCKPY="$1"; SIP="$2"; NFSP="$3"; SMBP="$4"; PW="$5"
cl() { echo "[cns] $*"; }

mount -t tmpfs tmpfs /run
mkdir -p /run/rpcbind /run/sendsigs.omit.d
rpcbind
sleep 0.3
( rpc.statd --no-notify 2>/dev/null || rpc.statd 2>/dev/null ) ; sleep 0.3

# A failed mount would leave a plain local dir behind, so locks would be
# resolved locally and the cases would be meaningless — hard-fail instead.
mkdir -p /mnt/v3 /mnt/v4 /mnt/smb
mount -t nfs -o nfsvers=3,tcp,port=$NFSP,mountport=$NFSP,actimeo=0 $SIP:/export /mnt/v3 || { cl "V3_MOUNT_FAIL"; exit 1; }
mount -t nfs -o nfsvers=4.0,tcp,port=$NFSP $SIP:/export /mnt/v4 || { cl "V4_MOUNT_FAIL"; exit 1; }
mount -t cifs -o port=$SMBP,username=admin,password=$PW,vers=3.0 //$SIP/export /mnt/smb || { cl "SMB_MOUNT_FAIL"; exit 1; }
mount | grep -E "/mnt/(v3|v4|smb)" | sed 's/^/[cns]   /'

# Per-run scratch for holder handshake files (avoids collisions between runs).
TMPD=$(mktemp -d)
ACQ="$TMPD/acq"; REL="$TMPD/rel"

# seed a shared file (same backing object on the server)
echo seed > /mnt/v3/lock.dat 2>/dev/null || echo seed > /mnt/smb/lock.dat 2>/dev/null || echo seed > /mnt/v4/lock.dat
sleep 0.3

# Warm-up: the client lockd/statd NLM handshake is not instant after the first
# NFSv3 mount; a lock can transiently fail with ENOLCK/NLM4_DENIED_NOLOCKS.
# Poll a throwaway NLM lock until it is granted before running the cases.
warm_ok=no
for i in $(seq 1 40); do
  if python3 "$LOCKPY" try /mnt/v3/warmup.dat w 0 100 2>/dev/null | grep -q ACQUIRED; then
    warm_ok=yes; cl "NLM warm-up ok after $((i*250))ms"; break
  fi
  sleep 0.25
done
# Hard-fail if NLM never came ready: otherwise a not-ready lock manager (ENOLCK)
# could be misread as a conflict and green-light a server that never coordinated.
[ "$warm_ok" = yes ] || { cl "NLM_WARMUP_FAILED"; exit 1; }

FAILS=0
run_case() { # name holder_path tester_path
  local name="$1" hp="$2" tp="$3"
  rm -f "$ACQ" "$REL"
  python3 "$LOCKPY" hold "$hp" w 0 100 "$ACQ" "$REL" &
  local hpid=$!
  for _ in $(seq 1 50); do [ -f "$ACQ" ] && break; sleep 0.1; done
  if ! grep -q ok "$ACQ" 2>/dev/null; then
    echo "$name conflict=HOLDER_FAIL($(tr '\n' ' ' <"$ACQ" 2>/dev/null)) afterRelease=NA"
    kill $hpid 2>/dev/null; wait $hpid 2>/dev/null; FAILS=$((FAILS+1)); return
  fi
  local res; res=$(python3 "$LOCKPY" try "$tp" w 0 100 2>/dev/null)
  touch "$REL"; wait $hpid 2>/dev/null
  sleep 0.3
  local res2; res2=$(python3 "$LOCKPY" try "$tp" w 0 100 2>/dev/null)
  echo "$name conflict=$res afterRelease=$res2"
  # A pass = the conflicting lock was denied by the server, then granted once
  # the holder released. Anything else (ACQUIRED-while-held, ERROR, ...) fails.
  [ "$res" = BLOCKED ] && [ "$res2" = ACQUIRED ] || FAILS=$((FAILS+1))
}

run_case NLM_vs_SMB   /mnt/v3/lock.dat  /mnt/smb/lock.dat
run_case NLM_vs_NFSv4 /mnt/v3/lock.dat  /mnt/v4/lock.dat
run_case SMB_vs_NLM   /mnt/smb/lock.dat /mnt/v3/lock.dat
run_case NFSv4_vs_NLM /mnt/v4/lock.dat  /mnt/v3/lock.dat

umount -f -l /mnt/v3 /mnt/v4 /mnt/smb 2>/dev/null
if [ "$FAILS" -eq 0 ]; then echo "NLM_AXIS_RESULT=PASS"; else echo "NLM_AXIS_RESULT=FAIL"; fi
exit "$FAILS"
CLIENT
client_rc=$?
[ "$client_rc" -eq 0 ] || { log "client cases failed (rc=$client_rc)"; exit "$client_rc"; }
