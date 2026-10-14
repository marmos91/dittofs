#!/usr/bin/env bash
# Client network namespaces for the cthon04 and nfstest suites. Sourced, not
# executed; every function needs root.
#
# The server stays on the host. Each client runs in a network namespace of its
# own, joined to the host by a veth pair on one bridge:
#
#   host (dfs, rpcbind)   dfsnfs0  10.51.0.1
#   client 1              dfsnfs1  10.51.0.2   the client under test
#   client 2              dfsnfs2  10.51.0.3   nfstest's second client
#
# A client that shares the server's network stack cannot test it faithfully:
# its kernel lockd takes over the server's NLM registration in the one rpcbind
# both would share, so NFSv3 locks never reach the server; nfstest cannot tell
# client from server traffic when both are 127.0.0.1; and a second client on
# the same stack is the same NFSv4 client to the server.
#
# client_ns_exec runs a command inside a client's namespace with a private
# /run, its own hostname and its own rpcbind and rpc.statd, the way the NLM
# interop test (test/e2e/testdata/nlm/nlm_axis_interop.sh) runs its client.
# Mounts made under it live in a mount namespace that ends with the command,
# so a client's whole run has to happen inside one client_ns_exec.

NS_BRIDGE=dfsnfs0
NS_SUBNET=10.51.0
NS_SERVER_ADDR="$NS_SUBNET.1"

ns_name() { echo "dfsnfs$1"; }
ns_addr() { echo "$NS_SUBNET.$(($1 + 1))"; }

# ns_forward_rule — the iptables rule that lets the clients reach each other.
# With br_netfilter loaded, traffic between two ports of one bridge passes the
# FORWARD chain, whose policy may be DROP.
ns_forward_rule() { echo "FORWARD -i $NS_BRIDGE -o $NS_BRIDGE -j ACCEPT"; }

# client_ns_up N — create client N's namespace and attach it to the bridge.
client_ns_up() {
    local ns host_if="dfsnfs$1h" ns_if="dfsnfs$1c" addr
    ns="$(ns_name "$1")"
    addr="$(ns_addr "$1")"
    if ! ip link show "$NS_BRIDGE" >/dev/null 2>&1; then
        ip link add "$NS_BRIDGE" type bridge || return 1
        ip addr add "$NS_SERVER_ADDR/24" dev "$NS_BRIDGE" || return 1
        ip link set "$NS_BRIDGE" up || return 1
        # shellcheck disable=SC2046  # the rule is a word list on purpose
        if command -v iptables >/dev/null && ! iptables -C $(ns_forward_rule) 2>/dev/null; then
            iptables -I $(ns_forward_rule)
        fi
    fi
    ip netns add "$ns" || return 1
    ip link add "$host_if" type veth peer name "$ns_if" || return 1
    ip link set "$ns_if" netns "$ns" || return 1
    ip link set "$host_if" master "$NS_BRIDGE" up || return 1
    ip netns exec "$ns" ip link set lo up || return 1
    ip netns exec "$ns" ip addr add "$addr/24" dev "$ns_if" || return 1
    ip netns exec "$ns" ip link set "$ns_if" up || return 1
    # nfstest finds its own address from the route to a public one, so a
    # client needs a default route even though it never leaves the subnet.
    ip netns exec "$ns" ip route add default via "$NS_SERVER_ADDR" || return 1
    # The bridge port takes a moment to start forwarding. The probe is the
    # host's rpcbind, which every client needs to reach anyway.
    local _
    for _ in $(seq 1 20); do
        ip netns exec "$ns" nc -z -w 1 "$NS_SERVER_ADDR" 111 2>/dev/null && return 0
        sleep 0.5
    done
    echo "client namespace $ns cannot reach rpcbind on $NS_SERVER_ADDR" >&2
    return 1
}

# client_ns_mirror FROM TO — copy every frame client FROM sends or receives
# into client TO's interface, the way a shared segment would show it. nfstest
# captures on its own client and expects to see the second client's calls in
# that one capture; on a veth each client sees only its own. TO's stack drops
# the copies of traffic between FROM and the server, which are not addressed
# to it. Traffic between FROM and TO themselves reaches TO twice, which TCP
# discards as duplicates.
client_ns_mirror() {
    local from="dfsnfs$1h" to="dfsnfs$2h" dir
    tc qdisc add dev "$from" clsact || return 1
    for dir in ingress egress; do
        tc filter add dev "$from" "$dir" matchall \
            action mirred egress mirror dev "$to" || return 1
    done
}

# client_ns_down_all — remove every client namespace and the bridge. Kills what
# still runs inside first, which also drops the mounts held there. Safe to run
# when nothing exists, so it doubles as cleanup before setup.
client_ns_down_all() {
    local ns n
    for n in 1 2; do
        ns="$(ns_name "$n")"
        if ip netns list 2>/dev/null | grep -qw "$ns"; then
            ip netns pids "$ns" 2>/dev/null | xargs -r kill -9 2>/dev/null
            ip netns del "$ns" 2>/dev/null
        fi
        ip link del "dfsnfs${n}h" 2>/dev/null
    done
    if ip link show "$NS_BRIDGE" >/dev/null 2>&1; then
        ip link del "$NS_BRIDGE" 2>/dev/null
    fi
    # shellcheck disable=SC2046
    if command -v iptables >/dev/null; then
        while iptables -C $(ns_forward_rule) 2>/dev/null; do
            iptables -D $(ns_forward_rule) || break
        done
    fi
    return 0
}

# client_ns_exec N CMD... — run CMD as root inside client N's namespace.
client_ns_exec() {
    local n="$1"
    shift
    ip netns exec "$(ns_name "$n")" unshare --mount --uts --propagation private \
        bash -c "$(declare -p NS_SUBNET; declare -f ns_name ns_addr _client_ns_init); _client_ns_init $n || { echo \"could not set up client namespace $n\" >&2; exit 1; }; exec \"\$@\"" \
        client-ns "$@"
}

# _client_ns_init N — runs inside the new namespaces, before the command.
_client_ns_init() {
    local name n
    name="$(ns_name "$1")"
    # A private /run: rpcbind, statd and sshd keep their sockets and pid files
    # there, so they do not collide with the host's or another client's.
    mount -t tmpfs -o mode=0755 tmpfs /run || return 1
    mkdir -p /run/rpcbind /run/sendsigs.omit.d /run/sshd /run/statd/sm /run/statd/sm.bak
    # NLM identifies a client by its hostname, and the server would take two
    # clients with the same one for one. The hosts file is private as well, so
    # sudo can resolve the name.
    hostname "$name"
    {
        echo "127.0.0.1 localhost"
        for n in 1 2; do echo "$(ns_addr "$n") $(ns_name "$n")"; done
    } >/run/hosts
    mount --bind /run/hosts /etc/hosts || return 1
    # The NFSv4 client id is derived from the hostname unless this identifier
    # is set; a unique one keeps two clients' leases apart either way.
    if [[ -w /sys/fs/nfs/net/nfs_client/identifier ]]; then
        cat /proc/sys/kernel/random/uuid >/sys/fs/nfs/net/nfs_client/identifier
    fi
    rpcbind || return 1
    rpc.statd --no-notify -P /run/statd || return 1
}
