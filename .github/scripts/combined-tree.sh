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
# decision: same-repository PRs only. This mode selects code that the run mode
# merges and executes as `go test`, in a job whose checkout persists a token
# into .git/config where that code can read it. Two things make that safe and
# only one of them is this rule: the token carries read-only scopes on a public
# repository, so it grants nothing a stranger lacks, and branches here are
# already push-authorised. The first is the load-bearing one. Adding any write
# scope to the workflow's permissions, or making the repository private, breaks
# the exemption whether or not this rule stays.
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
# ponytail: a flat cap, not a cost model. It counts combinations actually
# built, and it is set low enough that the job's wall-clock timeout is not the
# thing that ends a busy run -- a timeout keeps the per-combination lines
# already appended to the summary but loses the closing tally, so a run that
# ends that way under-reports rather than reporting nothing. The observed rate
# is 3-4 combinations a day; raise this only once the summary reports that it
# stopped at the cap, and raise the job timeout with it.
MAX_COMBOS="${MAX_COMBOS:-6}"

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
                      | .startedAt | select(. != null)] | sort | first) })' <<<"$prs")

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
  local head cutoff then_sha moved compared
  while IFS=$'\t' read -r n head cutoff; do
    then_sha=$(gh api "repos/$REPO/commits?sha=$BASE&until=$cutoff&per_page=1" \
                 --jq '.[0].sha // empty')
    [ -n "$then_sha" ] || continue
    [ "$then_sha" != "$dev_sha" ] || continue

    # Packages the base branch moved in that window, intersected with the PR's.
    #
    # ponytail: one compare call, whose file list the API caps at 300 and does
    # not paginate. This base branch reaches ~190 distinct files in a day and
    # ~860 in a week, so a PR whose matrix last ran days ago -- a draft marked
    # ready, say, since no workflow here triggers on ready_for_review -- really
    # does hit the cap. A truncated answer is therefore not treated as the
    # moved set: the window is taken as unknown and the PR's own packages
    # stand in, which builds more than it must rather than skipping in silence.
    # Walking the commits instead is the upgrade.
    compared=$(gh api "repos/$REPO/compare/$then_sha...$dev_sha" \
                 --jq "$JQ_PKGS"'{n: ([.files[]?.filename] | length),
                                  pkgs: pkgs([.files[]?.filename])}')
    if [ "$(jq -r '.n' <<<"$compared")" -ge 300 ]; then
      echo "combined-tree: #$n compares $then_sha..$dev_sha at the API file cap;" \
           "taking its own packages as the window" >&2
      moved=$(jq -c '.pkgs' <<<"$(jq -c --argjson n "$n" '.[] | select(.number == $n)' <<<"$prs")")
    else
      moved=$(jq -c '.pkgs' <<<"$compared")
    fi
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
  local combos built=0 broken=0 conflicted=0 skipped=0 stale=0 rc=0
  combos=$(cat)
  [ -n "$combos" ] || { summary "No combination to build."; return 0; }

  # Via the environment, not `git config`: worktrees share one config file, so
  # writing an identity there would reach every other checkout of the repo.
  export GIT_AUTHOR_NAME="combined-tree" GIT_AUTHOR_EMAIL="combined-tree@invalid"
  export GIT_COMMITTER_NAME="combined-tree" GIT_COMMITTER_EMAIL="combined-tree@invalid"

  local combo base label patterns
  while IFS= read -r combo; do
    [ -n "$combo" ] || continue
    # The cap counts combinations built, not combinations read: a run whose
    # first entries all conflict or share no buildable package has spent
    # nothing and should keep going.
    if [ "$built" -ge "$MAX_COMBOS" ]; then
      summary "- stopped at the cap of $MAX_COMBOS built combination(s); the rest were not built"
      break
    fi

    base=$(jq -r '.base' <<<"$combo")
    label="$BASE@${base:0:9} + $(jq -r '[.prs[].n | "#\(.)"] | join(" + ")' <<<"$combo")"
    patterns=$(jq -r '.pkgs[] | "./" + .' <<<"$combo" | tr '\n' ' ')

    # Clean first: the previous combination left a merge commit and possibly
    # build output, and a checkout onto a dirty tree fails the whole job.
    git reset --quiet --hard
    git clean -qfd
    git checkout --quiet --detach "$base"

    # A head that selection pinned but the fetch never brought down was
    # force-pushed in between. Merging it fails, and calling that a conflict
    # would be a false statement about two trees that were never compared.
    local head missing=0
    for head in $(jq -r '.prs[].head' <<<"$combo"); do
      git cat-file -e "$head^{commit}" 2>/dev/null || missing=1
    done
    if [ "$missing" = 1 ]; then
      stale=$((stale + 1))
      summary "- moved     $label -- a head moved after it was selected; not built"
      continue
    fi

    local conflict=0
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
    # red is routinely false is a gate nobody reads. It cannot hide a real
    # break: `-e` still lists a package that fails to type-check or imports
    # something that does not exist, because it has files.
    # shellcheck disable=SC2086 # patterns is a deliberate list of package paths
    patterns=$(go list -e \
      -f '{{if or .GoFiles .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' \
      $patterns 2>/dev/null | tr '\n' ' ')
    if [ -z "$patterns" ]; then
      # Not optional: `go vet` with no argument vets the current directory.
      skipped=$((skipped + 1))
      summary "- skipped   $label -- no shared package go builds here"
      continue
    fi

    built=$((built + 1))

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
  summary "$built built: $broken broken. Not built: $conflicted conflicting,"
  summary "$skipped with no buildable shared package, $stale with a moved head."
  return "$rc"
}

# ---------------------------------------------------------------------------
# selftest
# ---------------------------------------------------------------------------
# The run mode only earns its slot if it refuses a tree that merges cleanly and
# does not build, AND passes one that does. Asserting only a non-zero exit
# would be satisfied by a run mode that is red on everything, and by no Go
# toolchain on PATH at all -- both of which leave the gate reporting nothing
# real while reading green here. So both directions are asserted, and the
# refusal is matched on the verdict it printed rather than on its exit status.
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
    # Signing is off for the fixture: it inherits the developer's global config
    # otherwise, and a repository that asks contributors to sign would make
    # this block on a passphrase rather than run.
    git config commit.gpgsign false

    printf 'module selftest\n\ngo 1.26\n' >go.mod
    mkdir -p p
    printf 'package p\n\nfunc Old() int { return 1 }\n' >p/a.go
    git add -A && git commit --quiet -m base
    local base; base=$(git rev-parse HEAD)

    # Renames the function the other branch calls.
    git checkout --quiet -b renamer
    printf 'package p\n\nfunc New() int { return 1 }\n' >p/a.go
    git commit --quiet -am rename
    local a; a=$(git rev-parse HEAD)

    # Calls it, in a different file of the same package.
    git checkout --quiet "$base" && git checkout --quiet -b caller
    printf 'package p\n\nfunc Use() int { return Old() }\n' >p/b.go
    git add -A && git commit --quiet -m caller
    local b; b=$(git rev-parse HEAD)

    # Touches the same package without disagreeing with either.
    git checkout --quiet "$base" && git checkout --quiet -b harmless
    printf 'package p\n\nfunc Other() int { return 2 }\n' >p/c.go
    git add -A && git commit --quiet -m harmless
    local c; c=$(git rev-parse HEAD)

    git checkout --quiet --detach "$base"
    git merge --quiet --no-ff --no-edit "$a" >/dev/null
    git merge --quiet --no-ff --no-edit "$b" >/dev/null \
      || { echo "selftest: fixture was supposed to merge cleanly" >&2; exit 1; }
    # Asserted with the same command the run mode uses, so a fixture that
    # drifts into breaking only `go build` cannot quietly stop proving anything.
    if go vet ./p/ >/dev/null 2>&1; then
      echo "selftest: fixture was supposed to fail the checks run mode makes" >&2
      exit 1
    fi
    git checkout --quiet --detach "$base"

    local combo out
    combo() {
      printf '{"base":"%s","prs":[{"n":1,"head":"%s"},{"n":2,"head":"%s"}],"pkgs":["p"]}' \
        "$base" "$1" "$2"
    }

    # The tree that does not build must be reported BROKEN, by that name.
    combo="$(combo "$a" "$b")"
    out=$(BASE_BRANCH=main "$self" run <<<"$combo" 2>&1) && {
      echo "selftest: run mode accepted a broken combined tree" >&2
      printf '%s\n' "$out" >&2
      exit 1
    }
    case "$out" in
      *BROKEN*) ;;
      *) echo "selftest: run mode failed, but not by reporting a broken tree:" >&2
         printf '%s\n' "$out" >&2
         exit 1 ;;
    esac

    # And the tree that does build must come back clean. Without this, a run
    # mode that is red on everything passes the check above.
    combo="$(combo "$a" "$c")"
    out=$(BASE_BRANCH=main "$self" run <<<"$combo" 2>&1) || {
      echo "selftest: run mode rejected a sound combined tree" >&2
      printf '%s\n' "$out" >&2
      exit 1
    }
    case "$out" in
      *"0 broken"*) ;;
      *) echo "selftest: run mode passed a sound tree without building it:" >&2
         printf '%s\n' "$out" >&2
         exit 1 ;;
    esac

    echo "selftest: run mode refused the broken tree and passed the sound one"
  )
}

case "${1:-}" in
  select)   cmd_select ;;
  run)      cmd_run ;;
  selftest) cmd_selftest ;;
  *)        die "usage: $(basename "$0") select|run|selftest" ;;
esac
