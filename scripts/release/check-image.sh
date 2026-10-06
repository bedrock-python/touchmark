#!/usr/bin/env bash
# check-image.sh checks a touchmark image against what the threat model
# (docs/project/threat-model.md) and the hub template's CI ask of it. CI
# runs it on the image it builds from the Dockerfile; docs/project/release.md
# runs it on a release candidate.
#
# Usage: scripts/release/check-image.sh IMAGE [VERSION]
#
#   IMAGE    an image the local docker has or can pull
#   VERSION  the version `touchmark version` must print (default: any)
#
# It checks that:
#   - the default user is not root, and its uid is 65532;
#   - git is 2.45 or newer, can reach https (git-remote-https), and git-lfs
#     is absent;
#   - the system git config trusts every directory (safe.directory), so a
#     hub checkout another user owns is readable;
#   - sh, grep, sleep and tail exist, which GitLab's Docker executor and the
#     Gitea and Forgejo runners need in a job container;
#   - the entrypoint is touchmark, and `touchmark version` runs, also under
#     an arbitrary uid as the GitHub Action runs it (scripts/action/run.sh);
#   - the image is at most 64 MB.
# It prints the facts it checked.

set -euo pipefail
export MSYS_NO_PATHCONV=1

image=${1:?usage: check-image.sh IMAGE [VERSION]}
want_version=${2:-}
max_bytes=$((64 * 1024 * 1024))

fail() {
	echo "check-image.sh: $*" >&2
	exit 1
}

inspect() {
	docker image inspect --format "$1" "$image"
}

docker image inspect "$image" >/dev/null 2>&1 || docker pull -q "$image" >/dev/null

user=$(inspect '{{.Config.User}}')
[ "$user" = "65532:65532" ] || fail "the default user is \"$user\", want 65532:65532"
entrypoint=$(inspect '{{json .Config.Entrypoint}}')
[ "$entrypoint" = '["/usr/local/bin/touchmark"]' ] || fail "the entrypoint is $entrypoint"
size=$(inspect '{{.Size}}')
[ "$size" -le "$max_bytes" ] || fail "the image takes $size bytes, more than $max_bytes"

# One shell in the image answers the rest, as the image's default user.
facts=$(docker run --rm --entrypoint sh "$image" -c '
	set -eu
	echo "uid $(id -u)"
	echo "$(git version)"
	if [ -x "$(git --exec-path)/git-remote-https" ]; then echo "https yes"; else echo "https no"; fi
	if command -v git-lfs >/dev/null 2>&1 || git lfs version >/dev/null 2>&1; then echo "lfs yes"; else echo "lfs no"; fi
	echo "safe $(git config --system --get-all safe.directory | tr "\n" " ")"
	for p in sh grep sleep tail; do command -v "$p" >/dev/null || { echo "missing $p"; exit 1; }; done
	echo "tools sh grep sleep tail"
	cat /etc/alpine-release | sed "s/^/alpine /"
')
printf '%s\n' "$facts"

grep -qx 'uid 65532' <<<"$facts" || fail "the default uid is not 65532"
git_line=$(grep '^git version ' <<<"$facts") || fail "no git"
rest=${git_line#git version }
major=${rest%%.*}
rest=${rest#*.}
minor=${rest%%.*}
if ! { [ "$major" -gt 2 ] || { [ "$major" -eq 2 ] && [ "$minor" -ge 45 ]; }; }; then
	fail "$git_line is older than 2.45"
fi
grep -qx 'https yes' <<<"$facts" || fail "git has no https helper"
grep -qx 'lfs no' <<<"$facts" || fail "git-lfs is installed"
grep -q '^safe \* $' <<<"$facts" || fail "the system git config does not set safe.directory = *"

out=$(docker run --rm "$image" version)
echo "$out"
case $out in
"touchmark ${want_version:-}"*) ;;
*) fail "touchmark version prints \"$out\", want touchmark $want_version" ;;
esac
# The GitHub Action runs the image as the runner's uid, which has no entry
# in the image's /etc/passwd.
docker run --rm --user 1001:127 --env HOME=/tmp "$image" version >/dev/null ||
	fail "touchmark does not run as an arbitrary uid"

echo "size $size bytes"
echo "check-image.sh: $image ok"
