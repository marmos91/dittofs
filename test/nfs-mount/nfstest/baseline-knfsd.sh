#!/usr/bin/env bash
# Run nfstest's dio and delegation modules, and nfstest_cache on NFSv3, against
# the Linux kernel NFS server, for comparison with the DittoFS runs.
#
# Most of what those modules assert is what the kernel *client* puts on the
# wire or sees: which WRITEs it sends, whether it caches them, when it returns
# a delegation, when it notices another client's change. A failure against DittoFS is therefore one of two things, and
# only a second server on the same kernel can tell them apart:
#
#   knfsd passes, DittoFS fails  -> ours: something in our replies changes
#                                   what the client decides to send
#   knfsd fails too              -> no server meets the assertion with this
#                                   client, and the known-failure row says
#                                   nothing about DittoFS
#
# The modules run through run.sh, unchanged, so the two runs differ only in the
# server. Each option set is graded against the same tables the DittoFS cell
# uses, and baseline-knfsd.md records, per option set, every assertion knfsd
# fails and what knfsd does on each dio/delegation/cache row of those tables.
#
# Linux only, and not containerised, for the reason baseline-knfsd.sh in
# test/nfs-conformance/pynfs gives: knfsd's sockets belong to the kernel. On a
# host without nfsd, dispatch the nfs-knfsd-baseline workflow instead.
#
# Not a gate: it measures a third-party server.
#
# Usage:
#   sudo ./baseline-knfsd.sh [--variants v4.0,v4.1] [--export-dir DIR] [--keep]
#                            [--compare DIR]...
#
#   ./baseline-knfsd.sh --report-only RESULTS_DIR [--compare DIR]...
#
# --export-dir must not exist yet: the script creates it, makes it
# world-writable and deletes it on exit. It is exported to this host's own
# address and loopback only, and an nfsd the script started is stopped on exit.
#
# --report-only rewrites baseline-knfsd.md from an earlier run's results
# directory (results/baseline-knfsd-<time>/, or the uploaded artifact) without
# running anything, so the report follows a known-failure table edit without
# another run. It needs neither Linux nor root, and reports the option sets the
# directory holds unless --variants narrows them.
#
# --compare names a DittoFS nfstest results directory, as run.sh writes one
# under DITTOFS_RESULTS_DIR. Each option set
# is looked up as a subdirectory named after it, or ending in -<set>, holding
# results.tsv or nfstest.log. Repeat it to compare several runs. A table row
# whose pattern covers assertions knfsd passes as well as ones it fails speaks
# for the client only on the latter, so the report then lists, per assertion,
# every dio/delegation/cache failure those runs have more often than knfsd, where
# knfsd passed it at least once or never reached it.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)"
# shellcheck source=../option-sets.sh
source "$SCRIPT_DIR/../option-sets.sh"
# shellcheck source=../netns.sh
source "$SCRIPT_DIR/../netns.sh"
# shellcheck source=../../common/known-failures.sh
source "$TEST_DIR/common/known-failures.sh"

EXPORT_DIR="/srv/nfstest-baseline"
VARIANTS="$(option_set_names | paste -sd, -)"
KEEP=false
REPORT_ONLY=""
VARIANTS_GIVEN=false
COMPARE=()

log() { echo "[BASELINE] $*"; }
die() { echo "[BASELINE] $*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
    --variants) VARIANTS="${2:?--variants requires a value}"; VARIANTS_GIVEN=true; shift 2 ;;
    --export-dir) EXPORT_DIR="${2:?--export-dir requires a value}"; shift 2 ;;
    --keep) KEEP=true; shift ;;
    --report-only) REPORT_ONLY="${2:?--report-only requires a results directory}"; shift 2 ;;
    --compare) COMPARE+=("${2:?--compare requires a results directory}"); shift 2 ;;
    -h | --help) sed -n '2,50p' "$0"; exit 0 ;;
    *) die "unknown argument: $1" ;;
    esac
done

# tables VARIANT: the known-failure tables that grade VARIANT, colon-separated,
# as run.sh takes them: the shared table, then its version's overlays. It
# fails for a set it doesn't name, so a new set needs its tables here.
tables() {
    local d="$SCRIPT_DIR" overlays
    case "$1" in
        v3) overlays="$d/KNOWN_FAILURES_V3.md" ;;
        v4.0) overlays="$d/KNOWN_FAILURES_V4.md:$d/KNOWN_FAILURES_V40.md" ;;
        default | v4.1 | v4.1-noac | v4.1-smallio) overlays="$d/KNOWN_FAILURES_V4.md" ;;
        *) return 1 ;;
    esac
    echo "$d/KNOWN_FAILURES.md:$overlays"
}

for d in "${COMPARE[@]}"; do
    [[ -d "$d" ]] || die "--compare $d: no such directory"
done

if [[ -n "$REPORT_ONLY" && "$VARIANTS_GIVEN" == false ]]; then
    # The option sets the results directory holds, in option-sets.sh order.
    VARIANTS="$(option_set_names | while read -r v; do
        [[ -d "$REPORT_ONLY/$v" ]] && echo "$v"
    done | paste -sd, -)"
fi
IFS=',' read -ra variants <<<"$VARIANTS"
for v in "${variants[@]}"; do
    option_set_opts "$v" >/dev/null || die "unknown option set: $v"
    tables "$v" >/dev/null || die "no known-failure tables for option set $v"
done

# run_knfsd — export a directory from knfsd and run every option set against
# it, leaving results.tsv per set and a "meta" file describing the run under
# RESULTS_ROOT.
run_knfsd() {
    if [[ "$(uname -s)" != Linux ]]; then
        echo "[BASELINE] The baseline needs the in-kernel nfsd, so it only runs on Linux." >&2
        die "Elsewhere, dispatch it: gh workflow run nfs-knfsd-baseline.yml"
    fi
    [[ $EUID -eq 0 ]] || die "must run as root (sudo): it exports a filesystem and mounts it"
    [[ ! -e "$EXPORT_DIR" ]] || die "$EXPORT_DIR already exists; pass an --export-dir that does not, since it is deleted on exit"

    if ! command -v exportfs >/dev/null 2>&1; then
        log "Installing nfs-kernel-server..."
        { apt-get update -qq && apt-get install -y -qq nfs-kernel-server; } || die "could not install nfs-kernel-server"
    fi

    # run.sh's clients mount from their own network namespaces, on the subnet
    # netns.sh puts behind the host bridge, so the export is limited to it.
    CLIENTS="$NS_SUBNET.0/24"

    modprobe nfsd 2>/dev/null || true
    # An nfsd already serving is someone else's: cleanup leaves it running.
    local threads
    threads="$(cat /proc/fs/nfsd/threads 2>/dev/null || echo 0)"
    NFSD_WAS_RUNNING=false
    [[ "$threads" =~ ^[1-9] ]] && NFSD_WAS_RUNNING=true

    EXPORTS_FILE="/etc/exports.d/nfstest-baseline.exports"
    cleanup() {
        pkill -f 'tcpdump .*nfstest_' 2>/dev/null || true
        if [[ "$KEEP" == true ]]; then
            log "Leaving the export and nfsd in place (--keep)."
            return
        fi
        rm -f "$EXPORTS_FILE"
        exportfs -ra 2>/dev/null || true
        rm -rf "$EXPORT_DIR"
        if [[ "$NFSD_WAS_RUNNING" == false ]]; then
            log "Stopping the nfsd this run started..."
            rpc.nfsd 0 2>/dev/null || true
            systemctl stop nfs-server 2>/dev/null || service nfs-kernel-server stop 2>/dev/null || true
        fi
    }
    trap cleanup EXIT

    # No fsid=0: the export is then reachable at its own path over NFSv3 and
    # through the kernel's pseudo-root over NFSv4, so one --export serves every
    # option set. Root is squashed, as DittoFS's export for these suites
    # squashes it: nfstest runs as an ordinary user and only mounts as root,
    # and the world-writable directory lets that user create its files.
    log "Exporting $EXPORT_DIR to $CLIENTS..."
    mkdir -p "$EXPORT_DIR" /etc/exports.d
    chmod 777 "$EXPORT_DIR"
    local opts="rw,insecure,root_squash,no_subtree_check"
    echo "$EXPORT_DIR $CLIENTS($opts)" >"$EXPORTS_FILE"
    systemctl start nfs-server 2>/dev/null || service nfs-kernel-server start 2>/dev/null || true
    rpc.nfsd 8 || die "rpc.nfsd failed to start"
    exportfs -ra
    exportfs -v

    {
        echo "kernel=$(uname -r)"
        echo "nfstest=$(git -C "$("$SCRIPT_DIR/../fetch.sh" nfstest)" rev-parse --short HEAD 2>/dev/null || echo unknown)"
        echo "run=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
    } >"$RESULTS_ROOT/meta"

    local v modules
    for v in "${variants[@]}"; do
        modules="dio"
        # The same split run.sh makes by default: cache on NFSv3 only, where no
        # delegation recall overrides the attribute-cache timing it checks.
        if [[ "$(option_set_version "$v")" == 3 ]]; then
            modules+=",cache"
        else
            modules+=",delegation"
        fi
        mkdir -p "$RESULTS_ROOT/$v"
        log "Running $modules under option set $v against knfsd..."
        # An empty NFSTEST_MOUNTPORT leaves the v3 MOUNT port to rpcbind, where
        # knfsd's mountd registers. run.sh exits with the count of failures not on
        # the tables, which here is a finding, not an error.
        NFS_PORT=2049 NFSTEST_EXPORT="$EXPORT_DIR" NFSTEST_MOUNTPORT="" \
            DITTOFS_RESULTS_DIR="$RESULTS_ROOT/$v" DITTOFS_KNOWN_FAILURES="$(tables "$v")" \
            "$SCRIPT_DIR/run.sh" --variant "$v" --modules "$modules" || true
    done
}

if [[ -n "$REPORT_ONLY" ]]; then
    [[ -f "$REPORT_ONLY/meta" ]] || die "$REPORT_ONLY has no meta file; is it a baseline results directory?"
    RESULTS_ROOT="$(cd "$REPORT_ONLY" && pwd)"
else
    RESULTS_ROOT="$SCRIPT_DIR/results/baseline-knfsd-$(date +%Y%m%d-%H%M%S)"
    mkdir -p "$RESULTS_ROOT"
    run_knfsd
fi

# meta KEY — one field of the run's meta file.
meta() { sed -n "s/^$1=//p" "$RESULTS_ROOT/meta"; }

# knfsd_verdict PATTERN TSV — what knfsd did on the assertions a table row
# names: "fails too (N of M)", "passes (M)", or "not reached" if the run
# produced none. A row's glob can cover assertions knfsd passes as well as ones
# it fails, so the counts say how much of the row the verdict speaks for. When
# the row's module did not finish, the later assertions it names never ran,
# and the verdict says so.
knfsd_verdict() {
    local pattern="$1" tsv="$2" verdict name failed=0 total=0 out
    while IFS=$'\t' read -r verdict name; do
        kf_match_pattern "$pattern" "$name" || continue
        total=$((total + 1))
        [[ "$verdict" == PASS ]] || failed=$((failed + 1))
    done <"$tsv"
    if [[ $total -eq 0 ]]; then
        out="not reached"
    elif [[ $failed -gt 0 ]]; then
        out="fails too ($failed of $total)"
    else
        out="passes ($total)"
    fi
    grep -qE "^(TIMEOUT|INCOMPLETE)"$'\t'"${pattern%%/*}:" "$tsv" && out+=", module did not finish"
    echo "$out"
}

CMP_TMP="$(mktemp -d)"

# dittofs_tsv DIR VARIANT — the per-assertion verdicts of the DittoFS run in
# DIR for VARIANT: its results.tsv, or one graded from its nfstest.log for runs
# that predate results.tsv. Prints nothing if DIR has no such option set.
dittofs_tsv() {
    local dir="$1" v="$2" f parent kind
    for kind in results.tsv nfstest.log; do
        while IFS= read -r f; do
            parent="$(basename "$(dirname "$f")")"
            [[ "$parent" == "$v" || "$parent" == *-"$v" ]] || continue
            if [[ "$kind" == results.tsv ]]; then
                echo "$f"
            else
                local graded
                graded="$(mktemp -d "$CMP_TMP/graded.XXXXXX")"
                "$SCRIPT_DIR/parse-results.sh" "$f" "$(tables "$v")" "$graded" >/dev/null 2>&1
                [[ -s "$graded/results.tsv" ]] && echo "$graded/results.tsv"
            fi
            return
        done < <(find "$dir" -type f -name "$kind" | sort)
    done
}

# compare_rows VARIANT KNFSD_TSV — one line per dio/delegation/cache assertion that
# a --compare run fails more often than knfsd does, where knfsd passed it at
# least once or never reached it: assertion, run, DittoFS's failure count, what
# knfsd did, and the table row that holds it. An assertion name recurs within a
# module and knfsd can pass some occurrences and fail others, so counts are
# compared rather than single verdicts; one knfsd fails every time it reaches
# says nothing against the client, however often DittoFS fails it. Needs the
# variant's tables loaded.
compare_rows() {
    local v="$1" knfsd="$2" d tsv name dfails kfails ktotal kv row pattern
    for d in "${COMPARE[@]}"; do
        tsv="$(dittofs_tsv "$d" "$v")"
        [[ -n "$tsv" ]] || continue
        while IFS=$'\t' read -r name dfails kfails ktotal; do
            if [[ $ktotal -eq 0 ]]; then
                kv="not reached"
            elif [[ $kfails -eq 0 ]]; then
                kv="passes ($ktotal)"
            else
                kv="fails $kfails of $ktotal"
            fi
            row="none"
            for pattern in "${!KF_KNOWN[@]}"; do
                kf_match_pattern "$pattern" "$name" && { row="\`$pattern\`"; break; }
            done
            printf '%s\t%s\t%s\t%s\t%s\n' "$name" "$(basename "$d")" "$dfails" "$kv" "$row"
        done < <(awk -F'\t' '
            NR == FNR { total[$2]++; if ($1 != "PASS") kfail[$2]++; next }
            $1 != "PASS" && $2 ~ /^(dio|delegation|cache)\// { dfail[$2]++ }
            END {
                for (n in dfail)
                    if (dfail[n] > kfail[n] + 0 && (total[n] + 0 == 0 || total[n] > kfail[n] + 0))
                        print n "\t" dfail[n] "\t" kfail[n] + 0 "\t" total[n] + 0
            }' "$knfsd" "$tsv")
    done
}

OUT="$SCRIPT_DIR/baseline-knfsd.md"
{
    echo "# nfstest against Linux knfsd — baseline"
    echo ""
    echo "Generated by \`baseline-knfsd.sh\`. Do not hand-edit."
    echo ""
    echo "nfstest's \`dio\`, \`delegation\` and \`cache\` modules mostly assert what"
    echo "the kernel client sends or sees. This is the same run.sh run against the Linux kernel server on"
    echo "the same kernel. A row whose every match knfsd fails too is about the client,"
    echo "not DittoFS. A row knfsd *passes*, and an assertion DittoFS fails more often"
    echo "than knfsd in a compared DittoFS run, is DittoFS-specific. *Not reached* means"
    echo "knfsd produced no such assertion, so it is evidence neither way."
    echo ""
    echo "- Kernel: \`$(meta kernel)\`"
    echo "- nfstest: \`$(meta nfstest)\`"
    echo "- Run: $(meta run)"
    for d in "${COMPARE[@]}"; do
        echo "- Compared with DittoFS run: \`$(basename "$d")\`"
    done
    for v in "${variants[@]}"; do
        tsv="$RESULTS_ROOT/$v/results.tsv"
        echo ""
        echo "## $v"
        echo ""
        if [[ ! -s "$tsv" ]]; then
            echo "No verdicts: the run did not produce a gradable log. See \`$v/nfstest.log\` in the run's results."
            continue
        fi
        echo "Passed $(grep -c '^PASS' "$tsv"), failed $(grep -vc '^PASS' "$tsv")."
        echo ""
        echo "DittoFS known-failure rows for these modules:"
        echo ""
        echo "| Row | knfsd |"
        echo "|-----|-------|"
        # shellcheck disable=SC2034  # KF_REASON is read by kf_reason
        KF_KNOWN=() KF_REASON=()
        IFS=':' read -ra tbls <<<"$(tables "$v")"
        for t in "${tbls[@]}"; do kf_load "$t"; done
        for pattern in "${!KF_KNOWN[@]}"; do
            [[ "$pattern" == dio/* || "$pattern" == delegation/* || "$pattern" == cache/* ]] || continue
            echo "| \`$pattern\` | $(knfsd_verdict "$pattern" "$tsv") |"
        done | sort
        echo ""
        echo "Assertions knfsd fails:"
        echo ""
        echo "| Assertion | On a DittoFS table |"
        echo "|-----------|--------------------|"
        grep -v '^PASS' "$tsv" | sort -u | while IFS=$'\t' read -r verdict name; do
            kf_is_known "$name" && known=yes || known=no
            echo "| \`$name\` ($verdict) | $known |"
        done
        [[ ${#COMPARE[@]} -gt 0 ]] || continue
        echo ""
        echo "Assertions DittoFS fails more often than knfsd:"
        echo ""
        echo "| Assertion | DittoFS failures | knfsd | Row |"
        echo "|-----------|------------------|-------|-----|"
        # One line per assertion, naming every compared run that failed it.
        compare_rows "$v" "$tsv" | sort | awk -F'\t' '
            $1 != name {
                if (name != "") print line tail
                name = $1
                line = "| `" $1 "` | " $3 " in `" $2 "`"
                tail = " | " $4 " | " $5 " |"
                next
            }
            { line = line ", " $3 " in `" $2 "`" }
            END { if (name != "") print line tail }' >"$CMP_TMP/rows"
        if [[ -s "$CMP_TMP/rows" ]]; then
            cat "$CMP_TMP/rows"
        else
            echo "| none | | | |"
        fi
    done
} >"$OUT"
rm -rf "$CMP_TMP"

log "Wrote $OUT"
log "Logs: $RESULTS_ROOT"
