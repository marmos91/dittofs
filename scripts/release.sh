#!/usr/bin/env bash
# Cut a release: bump flake.nix on develop, fast-forward main to it, tag it.
#
# Usage: scripts/release.sh X.Y.Z[-rcN]
#
# Refuses unless develop is clean and in sync with origin, main is an ancestor
# of develop, the tag is new, and (for a non-rc version) CHANGELOG.md has a
# "## [X.Y.Z]" section. Everything up to the push is local; the push asks first.
set -euo pipefail

die() { echo "release: $*" >&2; exit 1; }

version="${1:-}"
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?$ ]] || die "usage: $0 X.Y.Z[-rcN]"
tag="v$version"

cd "$(git rev-parse --show-toplevel)"
git fetch -q origin develop main --tags

[[ "$(git branch --show-current)" == develop ]] || die "not on develop"
[[ -z "$(git status --porcelain --untracked-files=no)" ]] || die "tracked files have uncommitted changes"
[[ "$(git rev-parse HEAD)" == "$(git rev-parse origin/develop)" ]] || die "develop is not in sync with origin/develop"
git merge-base --is-ancestor origin/main HEAD || die "origin/main is not an ancestor of develop"
! git rev-parse -q --verify "refs/tags/$tag" >/dev/null || die "tag $tag already exists"

# decision: a release candidate may ship without a changelog section; its notes
# are the commit list. A final release may not, because release-notes.py
# silently falls back to that list when the section is missing.
if [[ "$version" != *-rc* ]]; then
  grep -q "^## \[$version\]" CHANGELOG.md ||
    die "CHANGELOG.md has no '## [$version]' section; land it on develop first"
fi

# CI on the commit being released. Not a gate: a flaky conformance run does not
# block a tag, so it is listed for the confirmation below to weigh.
not_green="$(gh run list --commit "$(git rev-parse HEAD)" --limit 100 --json name,status,conclusion \
  --jq '.[] | select(.conclusion != "success" and .conclusion != "skipped") | "  \(.name): \(if .conclusion == "" then .status else .conclusion end)"' |
  sort -u)" || not_green="  (could not ask gh)"
echo "CI on $(git rev-parse --short HEAD):"
echo "${not_green:-  all green}"

sed -i.bak -E "s/^( *version = \")[0-9]+\.[0-9]+\.[0-9]+(-rc[0-9]+)?(\";)$/\1$version\3/" flake.nix
rm flake.nix.bak
[[ "$(git diff --numstat flake.nix | cut -f1)" == 1 ]] || { git checkout flake.nix; die "flake.nix version bump did not change exactly one line"; }

git commit -S -q -m "chore(release): $tag" flake.nix
git tag -s "$tag" -m "$tag"
sha="$(git rev-parse --short HEAD)"

echo
echo "Committed $sha and tagged $tag locally. Pushing will:"
echo "  push develop and fast-forward main to $sha, then push $tag (triggers release.yml)"
read -r -p "Push? [y/N] " ok
if [[ "$ok" != [yY] ]]; then
  echo "Not pushed. To undo: git tag -d $tag && git reset --hard origin/develop"
  exit 0
fi

# develop and main move together, without the tag even under push.followTags,
# so release.yml never starts on a tag main does not contain.
# A rerun after a failed push stops at "tag already exists", so name what is left.
git push --atomic --no-follow-tags origin HEAD:develop HEAD:main ||
  die "push failed; nothing was published. Finish with: git push --atomic --no-follow-tags origin HEAD:develop HEAD:main && git push origin $tag"
git push origin "$tag" || die "branches pushed, tag not; finish with: git push origin $tag"
