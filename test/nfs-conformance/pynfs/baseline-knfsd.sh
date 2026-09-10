#!/usr/bin/env bash
# Run the pynfs suite against the Linux kernel NFS server, for comparison.
#
# This is what stops KNOWN_FAILURES from becoming a place to park bugs. A pynfs
# failure means one of two quite different things, and only a second server can
# tell them apart:
#
#   knfsd passes, DittoFS fails  -> ours: a real protocol bug, or a feature we
#                                   deliberately do not implement
#   knfsd fails too              -> the assertion is not one a conformant server
#                                   is expected to satisfy, and blacklisting it
#                                   says nothing about DittoFS
#
# Only the second case justifies a `suite` row. The output is a Markdown table
# of per-test knfsd verdicts, written to baseline-knfsd.md and committed, so
# triage can cite it and later readers can see the evidence without re-running.
#
# Linux only, and deliberately not containerised: knfsd hands its listening
# sockets to the kernel, which refuses them from a container's network namespace
# (rpc.nfsd fails with errno 111). Host networking gets past that, but then the
# export is reachable only from inside the VM on a macOS Docker host, so pynfs
# cannot reach it either. Running both directly on a Linux host avoids the whole
# problem — on macOS, run it from CI instead.
#
# Not a CI gate: it measures a third-party server, so its verdict says nothing
# about any given commit.
#
# Usage:
#   sudo ./baseline-knfsd.sh [--export-dir DIR] [--lease-time 30] [--keep]

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

EXPORT_DIR="/srv/pynfs-baseline"
LEASE_TIME=30
KEEP=false

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
log()       { echo -e "${GREEN}[BASELINE]${NC} $*"; }
log_error() { echo -e "${RED}[BASELINE]${NC} $*" >&2; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --export-dir) EXPORT_DIR="$2"; shift 2 ;;
        --lease-time) LEASE_TIME="$2"; shift 2 ;;
        --keep)       KEEP=true; shift ;;
        --help|-h)
            echo "Usage: sudo $0 [--export-dir DIR] [--lease-time SECONDS] [--keep]"
            echo ""
            echo "Runs pynfs 4.0 and 4.1 against the Linux kernel NFS server and"
            echo "writes per-test verdicts to baseline-knfsd.md."
            exit 0
            ;;
        *) log_error "Unknown option: $1"; exit 1 ;;
    esac
done

if [[ "$(uname -s)" != "Linux" ]]; then
    log_error "The baseline needs the in-kernel nfsd, so it only runs on Linux."
    log_error "From a macOS box, run it in CI instead:"
    log_error "  gh workflow run nfs-pynfs.yml -f baseline=true"
    exit 1
fi

if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root (sudo): it exports a filesystem."
    exit 1
fi

PYNFS_PREFIX=""
if ! command -v pynfs-4.0 >/dev/null 2>&1; then
    log "Building pynfs via nix..."
    PYNFS_OUT="$(nix build "${REPO_ROOT}#pynfs" --no-link --print-out-paths)" || {
        log_error "nix build .#pynfs failed"
        exit 1
    }
    PYNFS_PREFIX="${PYNFS_OUT}/bin/"
fi

EXPORTS_FILE="/etc/exports.d/pynfs-baseline.exports"

cleanup() {
    if [[ "$KEEP" == true ]]; then
        log "Leaving the export in place (--keep)."
        return
    fi
    log "Removing the export..."
    rm -f "$EXPORTS_FILE"
    exportfs -ra 2>/dev/null || true
    rm -rf "$EXPORT_DIR"
}
trap cleanup EXIT

if ! command -v exportfs >/dev/null 2>&1; then
    log "Installing nfs-kernel-server..."
    apt-get update -qq && apt-get install -y -qq nfs-kernel-server || {
        log_error "Could not install nfs-kernel-server"
        exit 1
    }
fi

# `insecure` is required: pynfs connects from an unprivileged source port, which
# knfsd rejects by default. `fsid=0` makes this the NFSv4 pseudo-root, so the
# path pynfs asks for is "/".
log "Exporting ${EXPORT_DIR}..."
mkdir -p "$EXPORT_DIR" /etc/exports.d
chmod 777 "$EXPORT_DIR"
echo "${EXPORT_DIR} *(rw,insecure,no_root_squash,no_subtree_check,fsid=0)" > "$EXPORTS_FILE"

# Match run-pynfs.sh's lease time: expiry tests sleep for a full lease, and at
# the 90s default they dominate the run. nfsv4leasetime lives in the nfsd
# filesystem, which rpc.nfsd mounts, and the kernel refuses to change it while
# threads are running — so start, drop to zero threads, set, start again.
# Best-effort: a baseline at the kernel default is slower but just as valid,
# since lease time changes how long expiry tests wait, not their verdict.
modprobe nfsd 2>/dev/null || true
systemctl start nfs-server 2>/dev/null || service nfs-kernel-server start 2>/dev/null || true
rpc.nfsd --no-udp 2049 2>/dev/null || true
rpc.nfsd 0 2>/dev/null || true
if echo "$LEASE_TIME" > /proc/fs/nfsd/nfsv4leasetime 2>/dev/null; then
    log "nfsv4leasetime set to $(cat /proc/fs/nfsd/nfsv4leasetime)s"
else
    log "WARNING: could not set nfsv4leasetime; using the kernel default"
fi

if ! rpc.nfsd --no-udp 2049; then
    log_error "rpc.nfsd failed to start"
    exit 1
fi
exportfs -ra
exportfs -v

if ! exportfs -v | grep -q insecure; then
    log_error "The export is not marked 'insecure'; pynfs connects from a high"
    log_error "port and every test would fail at connect."
    exit 1
fi

RESULTS_DIR="${SCRIPT_DIR}/results/baseline-knfsd-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"

# Flags must match run-pynfs.sh exactly, or this compares two different
# experiments. fsid=0 makes the export path "/".
for v in 4.0 4.1; do
    case "$v" in
        4.0) minor=0 ;;
        4.1) minor=1 ;;
    esac
    log "Running pynfs ${v} against knfsd..."
    "${PYNFS_PREFIX}pynfs-${v}" "localhost:2049/" \
        --minorversion "$minor" \
        --security sys \
        --uid 0 --gid 0 \
        --maketree \
        -v \
        all > "${RESULTS_DIR}/knfsd-v${v}.log" 2>&1
    tail -2 "${RESULTS_DIR}/knfsd-v${v}.log"
done

# Emit the evidence table. Only the final results block is read, for the same
# reason parse-results.sh reads only that: -v reprints every test as it runs.
OUT="${SCRIPT_DIR}/baseline-knfsd.md"
{
    echo "# pynfs against Linux knfsd — baseline"
    echo ""
    echo "Generated by \`baseline-knfsd.sh\`. Do not hand-edit."
    echo ""
    echo "Every pynfs failure is either something DittoFS gets wrong or does not"
    echo "implement, or an assertion no server satisfies. This table separates the"
    echo "second case from the first by measuring rather than assuming: a test the"
    echo "Linux kernel server also fails is evidence about the suite, not about"
    echo "DittoFS, and is the only thing that justifies a \`suite\` row in"
    echo "KNOWN_FAILURES."
    echo ""
    echo "- Kernel: \`$(uname -r)\`"
    echo "- Lease time: \`$(cat /proc/fs/nfsd/nfsv4leasetime 2>/dev/null || echo unknown)s\`"
    echo "- Generated: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo ""
    for v in 4.0 4.1; do
        log_file="${RESULTS_DIR}/knfsd-v${v}.log"
        echo "## NFSv${v}"
        echo ""
        grep -E '^Of those: ' "$log_file" | tail -1
        echo ""
        echo "Tests knfsd does not pass:"
        echo ""
        echo "| Test | knfsd |"
        echo "|------|-------|"
        awk '
            /^\*{50}$/ { n++; starts[n] = NR }
            { line[NR] = $0 }
            END {
                if (n < 2) exit
                for (i = starts[n-1]; i <= starts[n]; i++) {
                    if (line[i] ~ / : (FAILURE|WARNING|UNSUPPORTED|OMIT)[ \t]*$/) {
                        split(line[i], f, " ")
                        printf "| %s | %s |\n", f[1], f[length(f)]
                    }
                }
            }
        ' "$log_file"
        echo ""
    done
} > "$OUT"

log "Wrote $OUT"
log "Logs: $RESULTS_DIR"
