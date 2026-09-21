#!/usr/bin/env bash
#
# Cross-PR combined-tree check.
#
# Every PR matrix tests one tree: that PR's head merged into the base branch as
# it stood when the matrix ran. Nothing tests the tree that results from two
# green PRs landing one after the other, and "mergeable" answers a different
# question -- whether the texts conflict -- which is why a clean auto-merge of
# two disjoint file lists inside one Go package reaches the base branch broken.
#
# Three modes, so an empty selection costs a shallow checkout and a couple of
# API calls per open PR, and never pays for a Go toolchain or a full clone:
#
#   select   print one JSON object per combination worth building, to stdout
#   run      build/vet/test each combination from stdin, on a detached tree
#   selftest prove the run mode actually refuses a clean-merging broken tree
#
# decision: same-repository PRs only. This mode is selecting code that the run
# mode will merge and execute as `go test`, in a job holding a repository token,
# so a fork PR here would be running an unreviewed contributor's code against
# that token. Branches in this repository are already push-authorised, which is
# the property being relied on -- withdraw the exclusion only alongside a run
# mode that holds no credential worth taking.
#
# Selection picks open non-draft PRs against the base branch and pairs them:
#
#   base + PR        when the base has moved since that PR's matrix started,
#                    and the packages it moved intersect the PR's packages
#   base + PR + PR   for every pair of open PRs whose packages intersect --
#                    neither matrix has ever seen the other's change
#
# The filter is Go PACKAGE intersection, never file overlap. File overlap is
# the filter that waves through exactly the class that reaches the base branch.
set -euo pipefail

REPO="${REPO:-${GITHUB_REPOSITORY:-}}"
BASE="${BASE_BRANCH:-develop}"
# ponytail: a flat cap, not a cost model, and not the binding one -- the job's
# own wall-clock timeout stops the run well before twelve combinations of
# package tests do, and it stops it without printing the summary. The observed
# rate is 3-4 combinations a day; raise this only once the summary reports that
# it stopped at the cap.
MAX_COMBOS="${MAX_COMBOS:-12}"

# Map changed files to the Go packages they belong to. Test files count: they
# compile into the package and a test-only change breaks a sibling just as well.
#
# ponytail: three ceilings, all live. It reads the package a file sits in, not
# the packages that import it, so two changes meeting only through an import
# edge are invisible. It reads only `.go` paths, so two PRs bumping the same
# dependency map to no package and never pair. And a repository-root `.go` file
# belongs to no directory and is dropped. Walking the import graph is the
# upgrade for the first, and subsumes the second; take it once a combination is
# found to have broken a package neither side named.
JQ_PKGS='def pkgs(f): [f[] | select(endswith(".go")) | select(contains("/")) | sub("/[^/]*$"; "")] | unique;'
JQ_ISECT='def isect(x; y): [x[] | select(IN(y[]))];'

die() { echo "combined-tree: $*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# select
# ---------------------------------------------------------------------------
cmd_select() {
  [ -n "$REPO" ] || die "REPO or GITHUB_REPOSITORY must be set"

  local prs dev_sha n
  # One GraphQL round trip for every open PR: number, head, draft flag, changed
  # files and the check rollup that dates the last matrix.
  prs=$(gh pr list --repo "$REPO" --base "$BASE" --state open --limit 100 \
          --json number,headRefOid,isDraft,isCrossRepository,files,changedFiles,statusCheckRollup)

  prs=$(jq -c "$JQ_PKGS"'
    map(select((.isDraft | not) and (.isCrossRepository | not)))
    | map({ number, head: .headRefOid, pkgs: pkgs([.files[].path]),
            short: ((.files | length) < .changedFiles),
            cutoff: ([.statusCheckRollup[]? | select(.status == "COMPLETED")
                      | .startedAt] | sort | first) })' <<<"$prs")

  # That listing returns only the first hundred files of each PR, and a PR
  # sweeping enough to exceed it is exactly the one whose package set must not
  # come back short. Re-read those in full; the count says which.
  local full
  while read -r n; do
    full=$(gh api "repos/$REPO/pulls/$n/files" --paginate --jq '[.[].filename]' \
             | jq -sc 'add')
    prs=$(jq -c --argjson n "$n" --argjson f "$full" "$JQ_PKGS"'
      map(if .number == $n then .pkgs = pkgs($f) else . end)' <<<"$prs")
  done < <(jq -r '.[] | select(.short) | .number' <<<"$prs")

  prs=$(jq -c 'map(select((.pkgs | length) > 0))' <<<"$prs")
  [ "$(jq 'length' <<<"$prs")" -gt 0 ] || return 0

  dev_sha=$(gh api "repos/$REPO/commits/$BASE" --jq '.sha')

  # base + PR. The base moved if it carried a commit the PR's matrix never
  # checked out; the cutoff is when that matrix started, not when it finished.
  #
  # A PR with no completed check has no matrix to be stale against, so it is
  # skipped here -- and only here. It still pairs with other open PRs below,
  # where nothing reads the cutoff and a PR whose own suite is merely still
  # running is as unexamined against its neighbours as any other.
  local head cutoff then_sha moved
  while IFS=$'\t' read -r n head cutoff; do
    then_sha=$(gh api "repos/$REPO/commits?sha=$BASE&until=$cutoff&per_page=1" \
                 --jq '.[0].sha // empty')
    [ -n "$then_sha" ] || continue
    [ "$then_sha" != "$dev_sha" ] || continue

    # Packages the base branch moved in that window, intersected with the PR's.
    #
    # ponytail: one unpaginated compare, whose file list the API caps at 300.
    # A window that wide would have to hold a day of merges; page it if the
    # summary ever reports a combination the cap could have hidden.
    moved=$(gh api "repos/$REPO/compare/$then_sha...$dev_sha" \
              --jq "$JQ_PKGS"'pkgs([.files[]?.filename])')
    jq -c --argjson moved "$moved" --argjson n "$n" --arg head "$head" \
       --arg base "$dev_sha" "$JQ_ISECT"'
       isect(.pkgs; $moved) as $i
       | if ($i | length) == 0 then empty
         else { base: $base, prs: [{n: $n, head: $head}], pkgs: $i }
         end' <<<"$(jq -c --argjson n "$n" '.[] | select(.number == $n)' <<<"$prs")"
  done < <(jq -r '.[] | select(.cutoff != null) | [.number, .head, .cutoff] | @tsv' <<<"$prs")

  # base + PR + PR. Neither matrix has seen the other, so a package
  # intersection is the whole eligibility test.
  jq -c --arg base "$dev_sha" "$JQ_ISECT"'
    . as $p
    | range(0; length) as $i | range($i + 1; length) as $j
    | isect($p[$i].pkgs; $p[$j].pkgs) as $k
    | if ($k | length) == 0 then empty
      else { base: $base,
             prs: [{n: $p[$i].number, head: $p[$i].head},
                   {n: $p[$j].number, head: $p[$j].head}],
             pkgs: $k }
      end' <<<"$prs"
}

# ---------------------------------------------------------------------------
# run
# ---------------------------------------------------------------------------
summary() {
  echo "$*"
  [ -z "${GITHUB_STEP_SUMMARY:-}" ] || echo "$*" >>"$GITHUB_STEP_SUMMARY"
}

cmd_run() {
  local combos total=0 broken=0 conflicted=0 rc=0
  combos=$(cat)
  [ -n "$combos" ] || { summary "No combination to build."; return 0; }

  # Via the environment, not `git config`: worktrees share one config file, so
  # writing an identity there would reach every other checkout of the repo.
  export GIT_AUTHOR_NAME="combined-tree" GIT_AUTHOR_EMAIL="combined-tree@invalid"
  export GIT_COMMITTER_NAME="combined-tree" GIT_COMMITTER_EMAIL="combined-tree@invalid"

  local combo base label patterns
  while IFS= read -r combo; do
    [ -n "$combo" ] || continue
    if [ "$total" -ge "$MAX_COMBOS" ]; then
      summary "- stopped at the cap of $MAX_COMBOS combination(s); the rest were not built"
      break
    fi
    total=$((total + 1))

    base=$(jq -r '.base' <<<"$combo")
    label="$BASE@${base:0:9} + $(jq -r '[.prs[].n | "#\(.)"] | join(" + ")' <<<"$combo")"
    patterns=$(jq -r '.pkgs[] | "./" + .' <<<"$combo" | tr '\n' ' ')

    # Clean first: the previous combination left a merge commit and possibly
    # build output, and a checkout onto a dirty tree fails the whole job.
    git reset --quiet --hard
    git clean -qfd
    git checkout --quiet --detach "$base"

    local head conflict=0
    for head in $(jq -r '.prs[].head' <<<"$combo"); do
      if ! git merge --quiet --no-ff --no-edit "$head" >/dev/null 2>&1; then
        git merge --abort || true
        conflict=1
        break
      fi
    done
    if [ "$conflict" = 1 ]; then
      # decision: a conflicting pair is reported and skipped rather than failing
      # the job, because a textual conflict is already visible to whoever
      # rebases and says nothing about whether the merged tree would build.
      # Revisit if conflicts ever start hiding combinations worth building.
      conflicted=$((conflicted + 1))
      summary "- CONFLICT  $label -- does not merge cleanly, skipped"
      continue
    fi

    # Hand go only what it can build in this configuration. A directory whose
    # files all sit behind a build tag resolves to no package at all, and a
    # package both sides deleted resolves to nothing either; either one would
    # be reported as a broken tree rather than an absent one, and a gate whose
    # red is routinely false is a gate nobody reads.
    # shellcheck disable=SC2086 # patterns is a deliberate list of package paths
    patterns=$(go list -e \
      -f '{{if or .GoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' \
      $patterns 2>/dev/null | tr '\n' ' ')
    if [ -z "$patterns" ]; then
      # Not optional: `go vet` with no argument vets the current directory.
      summary "- skipped   $label -- no shared package go builds here"
      continue
    fi

    # decision: vet and test, not build. `go vet` type-checks everything
    # `go build` would reject, and `go test` builds the package under test,
    # while `go build` alone fails outright on the test-only packages this
    # filter deliberately keeps. The ceiling is linking: a main package with no
    # tests is compiled but never linked, so a pure link error in cmd/ slips.
    # shellcheck disable=SC2086 # patterns is a deliberate list of package paths
    if go vet $patterns && go test -count=1 -timeout=10m $patterns; then
      summary "- ok        $label -- $patterns"
    else
      broken=$((broken + 1))
      rc=1
      summary "- **BROKEN** $label -- $patterns"
      summary "  Each PR is green on its own; the merged tree is not."
    fi
  done <<<"$combos"

  summary ""
  summary "$total combination(s): $broken broken, $conflicted conflicting."
  return "$rc"
}

# ---------------------------------------------------------------------------
# selftest
# ---------------------------------------------------------------------------
# The run mode only earns its slot if it refuses a tree that merges cleanly and
# does not build. Two branches touch different files in one package; git merges
# them without a murmur and the result does not compile.
cmd_selftest() {
  local self; self=$(cd "$(dirname "$0")" && pwd)/$(basename "$0")
  # Not `local`: the EXIT trap fires after this function has returned, so the
  # path it removes has to outlive the frame that made it.
  SELFTEST_DIR=$(mktemp -d)
  trap 'rm -rf "$SELFTEST_DIR"' EXIT
  (
    cd "$SELFTEST_DIR"
    git init --quiet -b main .
    export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@invalid
    export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@invalid
    printf 'module selftest\n\ngo 1.26\n' >go.mod
    mkdir -p p
    printf 'package p\n\nfunc Old() int { return 1 }\n' >p/a.go
    git add -A && git commit --quiet -m base
    local base; base=$(git rev-parse HEAD)

    git checkout --quiet -b renamer
    printf 'package p\n\nfunc New() int { return 1 }\n' >p/a.go
    git commit --quiet -am rename
    local a; a=$(git rev-parse HEAD)

    git checkout --quiet "$base"
    git checkout --quiet -b caller
    printf 'package p\n\nfunc Use() int { return Old() }\n' >p/b.go
    git add -A && git commit --quiet -m caller
    local b; b=$(git rev-parse HEAD)

    git checkout --quiet --detach "$base"
    git merge --quiet --no-ff --no-edit "$a" >/dev/null
    git merge --quiet --no-ff --no-edit "$b" >/dev/null \
      || { echo "selftest: fixture was supposed to merge cleanly"; exit 1; }
    if go build ./p/ >/dev/null 2>&1; then
      echo "selftest: fixture was supposed to break the build" >&2
      exit 1
    fi

    local combo
    combo=$(printf '{"base":"%s","prs":[{"n":1,"head":"%s"},{"n":2,"head":"%s"}],"pkgs":["p"]}' \
              "$base" "$a" "$b")
    if BASE_BRANCH=main "$self" run <<<"$combo" >/dev/null 2>&1; then
      echo "selftest: run mode accepted a broken combined tree" >&2
      exit 1
    fi
    echo "selftest: run mode refused the broken combined tree"
  )
}

case "${1:-}" in
  select)   cmd_select ;;
  run)      cmd_run ;;
  selftest) cmd_selftest ;;
  *)        die "usage: $(basename "$0") select|run|selftest" ;;
esac
