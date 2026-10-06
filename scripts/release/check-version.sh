#!/usr/bin/env bash
# check-version.sh checks the release tag the publish workflow is about to
# publish, and prints what the later jobs need, as KEY=VALUE lines for
# GITHUB_OUTPUT. It runs in a full clone of the default branch (the tags
# fetched): the publish workflow runs the copy of the default branch, never
# the copy of the tag it checks, since a tag can name any commit.
#
# Usage: scripts/release/check-version.sh TAG
#
# Environment:
#   RELEASE_BRANCH  the ref the tag must be on (default: origin/master)
#
# It checks that:
#   - TAG is vX.Y.Z and exists in the clone;
#   - the tag's commit is on RELEASE_BRANCH: a release is cut from the
#     default branch, never from a tag pushed on another branch and
#     dispatched by hand;
#   - .release-please-manifest.json and the TM_VERSION line of action.yml
#     at that commit both say X.Y.Z: the Action at the tag runs the image of
#     the tag's own version, the one this run builds.
# It reads the tag's files with git show and runs nothing from them.
#
# It prints tag=vX.Y.Z, commit=<the tag's commit>, version=X.Y.Z, minor=X.Y,
# and whether the image's floating tags move to this release: latest=true
# when no release tag above X.Y.Z exists, latest_minor=true when no
# vX.Y.<higher> exists. A run dispatched again for an older release leaves
# them where they are.

set -euo pipefail

tag=${1:?usage: check-version.sh TAG}
branch=${RELEASE_BRANCH:-origin/master}
re='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'

fail() {
	echo "check-version.sh: $*" >&2
	exit 1
}

[[ $tag =~ $re ]] || fail "$tag is not a release tag vX.Y.Z"
version=${tag#v}
minor=${version%.*}
commit=$(git rev-parse --verify --quiet "refs/tags/$tag^{commit}") || fail "no tag $tag"
git rev-parse --verify --quiet "$branch^{commit}" >/dev/null || fail "no $branch in this clone"
git merge-base --is-ancestor "$commit" "$branch" || fail "$tag ($commit) is not on $branch"

manifest=$(git show "$commit:.release-please-manifest.json" | sed -n 's/^ *"\." *: *"\([^"]*\)".*$/\1/p')
[ "$manifest" = "$version" ] || fail ".release-please-manifest.json at $tag says \"$manifest\", not $version"
pins=$(git show "$commit:action.yml" | grep -c 'x-release-please-version' || true)
[ "$pins" = 1 ] || fail "action.yml at $tag has $pins x-release-please-version lines, want 1"
action=$(git show "$commit:action.yml" | sed -n 's/^ *TM_VERSION: *"\([^"]*\)" *# x-release-please-version$/\1/p')
[ "$action" = "$version" ] || fail "action.yml at $tag runs version \"$action\", not $version"

# key prints a key of a version that sorts as the versions do.
key() {
	local a b c
	IFS=. read -r a b c <<<"${1#v}"
	printf '%010d%010d%010d' "$((10#$a))" "$((10#$b))" "$((10#$c))"
}
me=$(key "$tag")
latest=true
latest_minor=true
while IFS= read -r t; do
	[[ $t =~ $re ]] || continue
	k=$(key "$t")
	if [[ $k > $me ]]; then
		latest=false
		if [[ $t == "v$minor."* ]]; then latest_minor=false; fi
	fi
done < <(git tag --list 'v*')

echo "tag=$tag"
echo "commit=$commit"
echo "version=$version"
echo "minor=$minor"
echo "latest=$latest"
echo "latest_minor=$latest_minor"
