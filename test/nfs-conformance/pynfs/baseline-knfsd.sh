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
# knfsd runs in a privileged container, so this works the same on a macOS
# laptop and on a CI runner. It needs a host kernel with nfsd (Docker Desktop
# and GitHub's ubuntu runners both have it) and a Docker volume for the export,
# because overlayfs cannot be NFS-exported.
#
# Not a CI gate: it measures a third-party server, so its verdict says nothing
# about any given commit.
#
# Usage:
#   ./baseline-knfsd.sh [--port 12050] [--keep]

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../../.." && pwd)"

PORT=12050
KEEP=false
CONTAINER="dittofs-knfsd-baseline"
VOLUME="dittofs-knfsd-export"
IMAGE="dittofs-knfsd-baseline"

RED='\033[0;31m'; GREEN='\033[0;32m'; NC='\033[0m'
log()       { echo -e "${GREEN}[BASELINE]${NC} $*"; }
log_error() { echo -e "${RED}[BASELINE]${NC} $*" >&2; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --port) PORT="$2"; shift 2 ;;
        --keep) KEEP=true; shift ;;
        --help|-h)
            echo "Usage: $0 [--port PORT] [--keep]"
            echo ""
            echo "Runs pynfs 4.0 and 4.1 against the Linux kernel NFS server in a"
            echo "privileged container and writes per-test verdicts to baseline-knfsd.md."
            exit 0
            ;;
        *) log_error "Unknown option: $1"; exit 1 ;;
    esac
done

if ! docker info >/dev/null 2>&1; then
    log_error "Docker is not available; knfsd runs in a privileged container."
    exit 1
fi

# --------------------------------------------------------------------------
# Locate pynfs.
# --------------------------------------------------------------------------
PYNFS_PREFIX=""
if ! command -v pynfs-4.0 >/dev/null 2>&1; then
    log "Building pynfs via nix..."
    PYNFS_OUT="$(nix build "${REPO_ROOT}#pynfs" --no-link --print-out-paths)" || {
        log_error "nix build .#pynfs failed"
        exit 1
    }
    PYNFS_PREFIX="${PYNFS_OUT}/bin/"
fi

cleanup() {
    if [[ "$KEEP" == true ]]; then
        log "Leaving ${CONTAINER} running (--keep)."
        return
    fi
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
    docker volume rm "$VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT

log "Building the knfsd image..."
docker build -q -t "$IMAGE" "${SCRIPT_DIR}/knfsd" >/dev/null || {
    log_error "docker build failed"
    exit 1
}

cleanup
log "Starting knfsd on port ${PORT}..."
docker volume create "$VOLUME" >/dev/null
if ! docker run -d --name "$CONTAINER" --privileged \
        -v "${VOLUME}:/export" -p "${PORT}:2049" "$IMAGE" >/dev/null; then
    log_error "Could not start the container"
    exit 1
fi

for _ in $(seq 1 30); do
    if docker exec "$CONTAINER" exportfs -v >/dev/null 2>&1; then
        break
    fi
    sleep 1
done
if ! docker exec "$CONTAINER" exportfs -v 2>/dev/null | grep -q /export; then
    log_error "knfsd did not export /export — is nfsd available on the host kernel?"
    docker logs "$CONTAINER"
    exit 1
fi
docker exec "$CONTAINER" exportfs -v

RESULTS_DIR="${SCRIPT_DIR}/results/baseline-knfsd-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"

# --------------------------------------------------------------------------
# Run both trees. The flags must match run-pynfs.sh exactly, or this compares
# two different experiments. fsid=0 makes the export path "/".
# --------------------------------------------------------------------------
for v in 4.0 4.1; do
    case "$v" in
        4.0) minor=0 ;;
        4.1) minor=1 ;;
    esac
    log "Running pynfs ${v} against knfsd..."
    "${PYNFS_PREFIX}pynfs-${v}" "localhost:${PORT}/" \
        --minorversion "$minor" \
        --security sys \
        --uid 0 --gid 0 \
        --maketree \
        -v \
        all > "${RESULTS_DIR}/knfsd-v${v}.log" 2>&1
    tail -2 "${RESULTS_DIR}/knfsd-v${v}.log"
done

# --------------------------------------------------------------------------
# Emit the evidence table. Only the final results block is read, for the same
# reason parse-results.sh reads only that: -v reprints every test as it runs.
# --------------------------------------------------------------------------
OUT="${SCRIPT_DIR}/baseline-knfsd.md"
KERNEL="$(docker exec "$CONTAINER" uname -r 2>/dev/null || echo unknown)"
NFSD_VER="$(docker exec "$CONTAINER" rpc.nfsd --version 2>&1 | head -1 || echo unknown)"
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
    echo "- Kernel: \`${KERNEL}\`"
    echo "- nfs-utils: \`${NFSD_VER}\`"
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
            /^\*{50}$/ { n++; starts[n] = NR; next }
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
