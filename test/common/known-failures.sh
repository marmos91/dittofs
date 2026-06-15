#!/usr/bin/env bash
# Shared known-failures (blacklist) loader for conformance test harnesses.
#
# Both the SMB conformance harnesses (smbtorture, WPTS) and the POSIX
# compliance harness (pjdfstest) grade their results against a blacklist of
# tests that are EXPECTED to fail — protocol-inherent limitations, upstream
# bugs, or environment quirks. A run is green when every failure is on the
# blacklist; only NEW failures (not on the list) fail CI.
#
# Blacklist file format (a Markdown table, identical across protocols so the
# files render nicely in GitHub and share this parser):
#
#   | Test Name | Category | Reason | Issue |
#   |-----------|----------|--------|-------|
#   | open/03.t | env      | PATH_MAX + mount prefix | - |
#   | flock/*   | proto    | NLM not implemented     | - |
#
# Only the first column (Test Name) and third column (Reason) are consumed;
# Category/Issue are documentation for humans. Lines that are blank, start
# with '#', do not start with '|', are separator rows (|---), or are the
# header row ("Test Name") are ignored.
#
# Pattern matching supports two wildcard styles so SMB and POSIX names both
# work:
#   - trailing ".*"  : prefix match on dotted test names (e.g. smb2.lock.*)
#   - shell globs    : '*' / '?' anywhere (e.g. flock/*, utimensat/0?.t)
#
# Usage:
#   source "/path/to/test/common/known-failures.sh"
#   kf_load "/path/to/KNOWN_FAILURES.md"
#   if kf_is_known "open/03.t"; then echo "expected: $(kf_reason open/03.t)"; fi
#   echo "loaded $KF_COUNT known failures"

# Associative arrays populated by kf_load. Declared here so callers that run
# under `set -u` can reference KF_COUNT/KF_KNOWN before the first load.
declare -A KF_KNOWN
declare -A KF_REASON
# bash `set -u` treats an empty associative array as unbound; seed+unset.
KF_KNOWN[_]="" ; unset 'KF_KNOWN[_]'
KF_REASON[_]="" ; unset 'KF_REASON[_]'
KF_COUNT=0

# kf_trim VALUE — echo VALUE with leading/trailing whitespace removed.
kf_trim() {
    local v="$1"
    v="${v#"${v%%[![:space:]]*}"}"
    v="${v%"${v##*[![:space:]]}"}"
    printf '%s' "$v"
}

# kf_load FILE — load the blacklist table from FILE into KF_KNOWN/KF_REASON.
# May be called more than once to merge multiple files (e.g. a base list plus
# a protocol-version overlay); later entries override earlier ones.
kf_load() {
    local file="$1"
    [[ -f "$file" ]] || return 0

    local line name reason
    while IFS= read -r line; do
        [[ -z "$line" ]] && continue
        [[ "$line" == \#* ]] && continue
        # Only Markdown table rows.
        [[ "$line" != \|* ]] && continue
        # Separator rows: | --- | --- |
        [[ "$line" =~ ^\|[[:space:]]*-+ ]] && continue

        # Leading '|' makes field 1 empty; field 2 = name, field 4 = reason.
        IFS='|' read -r _ name _ reason _ <<< "$line"
        name="$(kf_trim "$name")"
        reason="$(kf_trim "$reason")"

        # Header row.
        [[ "$name" == "Test Name" ]] && continue
        [[ -z "$name" ]] && continue

        KF_KNOWN["$name"]=1
        KF_REASON["$name"]="${reason:-unknown}"
    done < "$file"

    KF_COUNT=${#KF_KNOWN[@]}
}

# kf_match_pattern PATTERN NAME — true if NAME matches PATTERN. Supports a
# trailing ".*" prefix match and shell-glob patterns.
kf_match_pattern() {
    local pattern="$1" name="$2"

    # Exact.
    [[ "$pattern" == "$name" ]] && return 0

    # Dotted prefix wildcard: "smb2.lock.*" matches "smb2.lock.lock1".
    if [[ "$pattern" == *'.*' ]]; then
        local prefix="${pattern%.\*}"
        [[ "$name" == "$prefix"* ]] && return 0
    fi

    # Shell glob (covers flock/*, utimensat/0?.t, etc.). Also accept the bare
    # pattern matched against NAME with an implicit ".t" or "/..." suffix so a
    # blacklist entry like "open/etxtbsy" matches "open/etxtbsy.t".
    # shellcheck disable=SC2254
    case "$name" in
        $pattern) return 0 ;;
    esac
    if [[ "$pattern" != *.t && "$pattern" != *'*'* ]]; then
        # shellcheck disable=SC2254
        case "$name" in
            ${pattern}.t) return 0 ;;
            ${pattern}/*) return 0 ;;
        esac
    fi

    return 1
}

# kf_is_known NAME — true if NAME is a known/expected failure.
kf_is_known() {
    local name="$1" pattern
    [[ -n "${KF_KNOWN[$name]+_}" ]] && return 0
    for pattern in "${!KF_KNOWN[@]}"; do
        kf_match_pattern "$pattern" "$name" && return 0
    done
    return 1
}

# kf_reason NAME — echo the documented reason for NAME (or "unknown").
kf_reason() {
    local name="$1" pattern
    if [[ -n "${KF_REASON[$name]+_}" ]]; then
        printf '%s' "${KF_REASON[$name]}"
        return
    fi
    for pattern in "${!KF_REASON[@]}"; do
        if kf_match_pattern "$pattern" "$name"; then
            printf '%s' "${KF_REASON[$pattern]}"
            return
        fi
    done
    printf 'unknown'
}
