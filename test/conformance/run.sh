#!/usr/bin/env bash
# One entrypoint for every conformance suite.
#
# The suites differ in substrate — Docker Compose, host plus Nix, a Python NFSv4
# client — but they all need the same scaffolding around them: resolve a profile,
# pick the blacklist that matches the variant, put results somewhere predictable,
# write a step summary, and report a verdict. That scaffolding lives here and the
# per-suite specifics live in suites.json.
#
# Usage:
#   ./run.sh --list
#   ./run.sh --suite wpts --profile memory
#   ./run.sh --suite pjdfstest --profile memory --variant 4.1
#   ./run.sh --suite wpts --tier pull_request        # every profile in that tier
#   ./run.sh --suite pjdfstest --profile memory --dry-run
#   ./run.sh --matrix pull_request                   # JSON matrix for CI
#
# Anything after `--` is passed through to the suite's own runner untouched.
#
# Exit status is the suite's own status, not a boolean. The graders exit with the
# NUMBER of failures that are not on the blacklist, and collapsing that to 1
# throws away the only number that says how bad a red run is.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
MANIFEST="${CONFORMANCE_MANIFEST:-${SCRIPT_DIR}/suites.json}"
# Step commands are relative to the manifest's parent, so a test can point
# CONFORMANCE_MANIFEST at a synthetic tree and get its own scripts dispatched.
TEST_DIR="$(cd "$(dirname "$MANIFEST")/.." && pwd)"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[CONFORMANCE]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[CONFORMANCE]${NC} $*"; }
log_error() { echo -e "${RED}[CONFORMANCE]${NC} $*" >&2; }
log_step() { echo -e "${CYAN}[CONFORMANCE]${NC} $*"; }

die() {
    log_error "$*"
    exit 2
}

command -v jq >/dev/null 2>&1 || die "jq is required to read ${MANIFEST}"
[[ -f "$MANIFEST" ]] || die "manifest not found: ${MANIFEST}"
jq -e . "$MANIFEST" >/dev/null 2>&1 || die "manifest is not valid JSON: ${MANIFEST}"

# mq [jq flags...] FILTER — query the manifest.
mq() { jq -r "$@" "$MANIFEST"; }

suite_names() { mq '.suites | keys_unsorted[]'; }

suite_exists() { [[ "$(mq --arg s "$1" '.suites | has($s)')" == "true" ]]; }

# suite_profiles SUITE
suite_profiles() { mq --arg s "$1" '.suites[$s].profiles[]'; }

# suite_variants SUITE — the variant axis values, or nothing when the suite has none.
suite_variants() { mq --arg s "$1" '.suites[$s].variant.values // [] | .[]'; }

# suite_variant_name SUITE
suite_variant_name() { mq --arg s "$1" '.suites[$s].variant.name // ""'; }

# tier_profiles SUITE EVENT — resolves "all" to the full profile list.
tier_profiles() {
    mq --arg s "$1" --arg e "$2" '
        .suites[$s] as $suite
        | ($suite.tiers[$e] // .defaults.tiers[$e] // "all") as $tier
        | (if $tier == "all" then $suite.profiles else $tier end)[]
    '
}

# known_failures SUITE VARIANT — path relative to test/, or empty when the suite
# has no blacklist. Suites that grade each variant against its own table carry an
# object here rather than a string; pynfs is the reason that shape exists.
known_failures() {
    mq --arg s "$1" --arg v "$2" '
        .suites[$s].known_failures as $kf
        | if $kf == null then ""
          elif ($kf | type) == "string" then $kf
          else ($kf[$v] // "")
          end
    '
}

usage() {
    cat <<EOF
Usage: run.sh [options]

  --list                    Show every suite, its profiles and variants
  --suite NAME              Suite to run (see --list)
  --profile NAME            Storage profile (default: the suite's first)
  --variant VALUE           Variant axis value, e.g. an NFS minor version
  --tier EVENT              Run every profile this suite runs for that GitHub
                            event (pull_request, push, schedule)
  --matrix EVENT            Print the CI job matrix for that event as JSON
  --results-dir DIR         Where to write logs (default: test/conformance/results)
  --keep                    Skip the teardown steps
  --dry-run                 Print what would run and exit
  -h, --help                This message
  --                        Pass the remaining arguments to the suite's runner

Suites: $(suite_names | tr '\n' ' ')
EOF
}

list_suites() {
    local suite variant_name variants profiles
    for suite in $(suite_names); do
        printf '%s\n' "$suite"
        printf '  %s\n' "$(mq --arg s "$suite" '.suites[$s].description')"
        profiles="$(suite_profiles "$suite" | tr '\n' ' ')"
        printf '  profiles: %s\n' "$profiles"
        variant_name="$(suite_variant_name "$suite")"
        if [[ -n "$variant_name" ]]; then
            variants="$(suite_variants "$suite" | tr '\n' ' ')"
            printf '  %s: %s\n' "$variant_name" "$variants"
        fi
        printf '  presubmit: %s\n' "$(tier_profiles "$suite" pull_request | tr '\n' ' ')"
        printf '\n'
    done
}

# print_matrix EVENT — the profile/variant cross product CI should run, as JSON.
print_matrix() {
    local event="$1" suite profile variant
    local entries=()
    for suite in $(suite_names); do
        for profile in $(tier_profiles "$suite" "$event"); do
            if [[ -n "$(suite_variant_name "$suite")" ]]; then
                for variant in $(suite_variants "$suite"); do
                    entries+=("$(jq -cn --arg s "$suite" --arg p "$profile" --arg v "$variant" \
                        '{suite: $s, profile: $p, variant: $v}')")
                done
            else
                entries+=("$(jq -cn --arg s "$suite" --arg p "$profile" \
                    '{suite: $s, profile: $p, variant: ""}')")
            fi
        done
    done
    printf '%s\n' "${entries[@]}" | jq -cs '{include: .}'
}

# A dfs left behind by a killed run answers on the adapter port and grades a
# suite against a build nobody is testing. Clearing it is cheap; guessing which
# build answered is not.
clear_orphan_server() {
    local port="${DITTOFS_NFS_PORT:-12049}"
    local pids
    pids="$(lsof -ti "tcp:${port}" -sTCP:LISTEN 2>/dev/null || true)"
    [[ -z "$pids" ]] && return 0
    log_warn "port ${port} already has a listener (pid ${pids//$'\n'/ }); stopping it"
    # shellcheck disable=SC2086
    kill $pids 2>/dev/null || sudo kill $pids 2>/dev/null || true
}

SUITE=""
PROFILE=""
VARIANT=""
TIER=""
MATRIX_EVENT=""
RESULTS_ROOT="${SCRIPT_DIR}/results"
KEEP=false
DRY_RUN=false
PASSTHROUGH=()

while [[ $# -gt 0 ]]; do
    case "$1" in
        --list) list_suites; exit 0 ;;
        --suite) SUITE="${2:?--suite requires a value}"; shift 2 ;;
        --suite=*) SUITE="${1#*=}"; shift ;;
        --profile) PROFILE="${2:?--profile requires a value}"; shift 2 ;;
        --profile=*) PROFILE="${1#*=}"; shift ;;
        --variant) VARIANT="${2:?--variant requires a value}"; shift 2 ;;
        --variant=*) VARIANT="${1#*=}"; shift ;;
        --tier) TIER="${2:?--tier requires a value}"; shift 2 ;;
        --tier=*) TIER="${1#*=}"; shift ;;
        --matrix) MATRIX_EVENT="${2:?--matrix requires a value}"; shift 2 ;;
        --matrix=*) MATRIX_EVENT="${1#*=}"; shift ;;
        --results-dir) RESULTS_ROOT="${2:?--results-dir requires a value}"; shift 2 ;;
        --results-dir=*) RESULTS_ROOT="${1#*=}"; shift ;;
        --keep) KEEP=true; shift ;;
        --dry-run) DRY_RUN=true; shift ;;
        -h|--help) usage; exit 0 ;;
        --) shift; PASSTHROUGH=("$@"); break ;;
        *) die "unknown argument: $1 (try --help)" ;;
    esac
done

if [[ -n "$MATRIX_EVENT" ]]; then
    print_matrix "$MATRIX_EVENT"
    exit 0
fi

[[ -n "$SUITE" ]] || { usage >&2; die "no --suite given"; }
suite_exists "$SUITE" || die "unknown suite: ${SUITE} (known: $(suite_names | tr '\n' ' '))"

# run_one PROFILE VARIANT — runs a suite's steps once and returns the suite's
# own exit status.
run_one() {
    local profile="$1" variant="$2"
    local label="$profile"
    [[ -n "$variant" ]] && label="${profile}-${variant}"

    local results_dir="${RESULTS_ROOT}/${SUITE}/${label}"
    local kf
    kf="$(known_failures "$SUITE" "$variant")"
    if [[ -n "$kf" ]]; then
        [[ -f "${TEST_DIR}/${kf}" ]] || die "blacklist declared but missing: test/${kf}"
        kf="${TEST_DIR}/${kf}"
    fi

    local step_count
    step_count="$(mq --arg s "$SUITE" '.suites[$s].steps | length')"
    [[ "$step_count" -gt 0 ]] || die "suite ${SUITE} declares no steps"

    if [[ "$DRY_RUN" == false ]]; then
        mkdir -p "$results_dir"
        clear_orphan_server
    fi

    log_step "${SUITE} / ${label}"
    [[ -n "$kf" ]] && log_info "blacklist: ${kf#"${REPO_ROOT}/"}"

    local status=0 i
    for ((i = 0; i < step_count; i++)); do
        local name cmd needs_root always
        name="$(mq --arg s "$SUITE" --argjson i "$i" '.suites[$s].steps[$i].name')"
        cmd="$(mq --arg s "$SUITE" --argjson i "$i" '.suites[$s].steps[$i].cmd')"
        needs_root="$(mq --arg s "$SUITE" --argjson i "$i" '.suites[$s].steps[$i].root // false')"
        always="$(mq --arg s "$SUITE" --argjson i "$i" '.suites[$s].steps[$i].always // false')"

        # A step that already failed skips the rest, except teardown, which has
        # to run precisely when something went wrong.
        if [[ "$status" -ne 0 && "$always" != "true" ]]; then
            continue
        fi
        if [[ "$always" == "true" && "$KEEP" == true ]]; then
            log_info "skipping ${name} (--keep)"
            continue
        fi

        local args=()
        while IFS= read -r arg; do
            arg="${arg//\{profile\}/$profile}"
            arg="${arg//\{variant\}/$variant}"
            args+=("$arg")
        done < <(mq --arg s "$SUITE" --argjson i "$i" '.suites[$s].steps[$i].args[]?')

        local runner="${TEST_DIR}/${cmd}"
        [[ -x "$runner" ]] || die "step ${name}: ${cmd} is not executable"

        # Privilege is declared per step, never hoisted to the whole run: pynfs
        # is its own NFSv4 client and must stay runnable with no mount and no
        # root, and a root precondition at the top would take that away.
        local -a prefix=()
        if [[ "$needs_root" == "true" && "$EUID" -ne 0 ]]; then
            prefix=(sudo -E)
        fi

        if [[ "$DRY_RUN" == true ]]; then
            echo "  ${name}: ${prefix[*]-} ${runner} ${args[*]-} ${PASSTHROUGH[*]-}"
            continue
        fi

        local log="${results_dir}/${name}.log"
        log_info "step ${name}"

        # Never read $? after a pipe: tee's status would mask the runner's, and
        # the graders exit with a failure COUNT that has to survive to the caller.
        set -o pipefail
        DITTOFS_PROFILE="$profile" \
        DITTOFS_VARIANT="$variant" \
        DITTOFS_KNOWN_FAILURES="$kf" \
        DITTOFS_RESULTS_DIR="$results_dir" \
            "${prefix[@]}" "$runner" "${args[@]}" "${PASSTHROUGH[@]}" 2>&1 | tee "$log"
        local step_status="${PIPESTATUS[0]}"
        set +o pipefail

        if [[ "$step_status" -ne 0 ]]; then
            log_error "step ${name} exited ${step_status}"
            [[ "$always" == "true" ]] || status="$step_status"
        fi
    done

    [[ "$DRY_RUN" == true ]] || write_summary "$label" "$status" "${results_dir}"
    return "$status"
}

# write_summary LABEL STATUS RESULTS_DIR
write_summary() {
    local label="$1" status="$2" results_dir="$3"
    local verdict icon
    if [[ "$status" -eq 0 ]]; then
        verdict="pass"
        icon=":white_check_mark:"
    else
        verdict="${status} new failure(s)"
        icon=":x:"
    fi
    log_info "${SUITE} / ${label}: ${verdict}"

    [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] || return 0
    {
        echo "## ${SUITE} / ${label}"
        echo ""
        echo "| Suite | Profile | Verdict |"
        echo "|---|---|---|"
        echo "| ${SUITE} | ${label} | ${icon} ${verdict} |"
        echo ""
        local run_log="${results_dir}/run.log"
        [[ -f "$run_log" ]] || return 0
        echo "<details><summary>Last 40 lines</summary>"
        echo ""
        echo '```'
        tail -40 "$run_log"
        echo '```'
        echo ""
        echo "</details>"
    } >>"$GITHUB_STEP_SUMMARY"
}

# Resolve what to run.
PROFILES=()
if [[ -n "$TIER" ]]; then
    [[ -z "$PROFILE" ]] || die "--profile and --tier are mutually exclusive"
    while IFS= read -r p; do PROFILES+=("$p"); done < <(tier_profiles "$SUITE" "$TIER")
    [[ ${#PROFILES[@]} -gt 0 ]] || die "suite ${SUITE} runs no profiles for tier ${TIER}"
else
    if [[ -z "$PROFILE" ]]; then
        PROFILE="$(suite_profiles "$SUITE" | head -1)"
    fi
    suite_profiles "$SUITE" | grep -qxF "$PROFILE" \
        || die "profile ${PROFILE} is not one of ${SUITE}'s: $(suite_profiles "$SUITE" | tr '\n' ' ')"
    PROFILES=("$PROFILE")
fi

VARIANTS=("")
VARIANT_NAME="$(suite_variant_name "$SUITE")"
if [[ -n "$VARIANT_NAME" ]]; then
    if [[ -n "$VARIANT" ]]; then
        suite_variants "$SUITE" | grep -qxF "$VARIANT" \
            || die "${VARIANT_NAME} ${VARIANT} is not one of ${SUITE}'s: $(suite_variants "$SUITE" | tr '\n' ' ')"
        VARIANTS=("$VARIANT")
    else
        VARIANTS=()
        while IFS= read -r v; do VARIANTS+=("$v"); done < <(suite_variants "$SUITE")
    fi
elif [[ -n "$VARIANT" ]]; then
    die "suite ${SUITE} has no variant axis"
fi

WORST=0
for p in "${PROFILES[@]}"; do
    for v in "${VARIANTS[@]}"; do
        run_one "$p" "$v" || {
            rc=$?
            [[ "$rc" -gt "$WORST" ]] && WORST="$rc"
        }
    done
done

exit "$WORST"
