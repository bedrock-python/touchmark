#!/usr/bin/env bash
# image-e2e.sh runs plan and distribute from a touchmark image against a
# live Gitea or Forgejo: the binary, the git and the user of the image, end
# to end. The unit and e2e suites run the engine in process with the git of
# golang:1.26; this checks the artifact a hub actually runs. docs/project/release.md
# runs it on a release candidate before the tag.
#
# Usage: scripts/release/image-e2e.sh IMAGE [FORGE_IMAGE]
#
#   IMAGE        the touchmark image to test (a local tag or NAME@sha256:...)
#   FORGE_IMAGE  default docker.gitea.com/gitea:1.27.3
#
# It starts and seeds the forge with scripts/e2e/gitea.sh --keep (running
# only its quick TestVisibility), then, in the forge's network namespace:
#   1. creates a repository in acme with an opt-in file, and a hub in a
#      temporary directory that ships one pack to it;
#   2. plan, with the reader's token, as the image's user, the hub mounted
#      read-only: one target would get a pull request;
#   3. distribute, with the writer's token: the pull request exists on the
#      forge, by the writer, on touchmark/<id>, with the pack's file;
#   4. distribute again, as an arbitrary uid with HOME=/tmp, the way the
#      GitHub Action runs the image: nothing to write.
# Everything it created is removed on exit, the forge included. Tokens reach
# containers through the environment, never a command line.

set -euo pipefail
export MSYS_NO_PATHCONV=1

image=${1:?usage: image-e2e.sh IMAGE [FORGE_IMAGE]}
forge_image=${2:-docker.gitea.com/gitea:1.27.3}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
url=http://localhost:3000

die() {
	echo "image-e2e.sh: $*" >&2
	exit 1
}
log() {
	echo "image-e2e.sh: $*" >&2
}
host_path() {
	if command -v cygpath >/dev/null 2>&1; then cygpath -m -- "$1"; else printf '%s\n' "$1"; fi
}

work=$(mktemp -d)
forge=
net=
envfile=
cleanup() {
	local status=$?
	trap - EXIT INT TERM
	if [ -n "$forge" ]; then docker rm -f -v "$forge" >/dev/null 2>&1 || true; fi
	if [ -n "$net" ]; then docker network rm "$net" >/dev/null 2>&1 || true; fi
	if [ -n "$envfile" ]; then rm -f -- "$envfile"; fi
	rm -rf "$work"
	exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

docker image inspect "$image" >/dev/null 2>&1 || docker pull -q "$image" >/dev/null

log "starting $forge_image with scripts/e2e/gitea.sh --keep"
if ! bash "$root/scripts/e2e/gitea.sh" --keep --run '^TestVisibility$' "$forge_image" >"$work/harness.log" 2>&1; then
	tail -n 40 "$work/harness.log" >&2
	forge=$(sed -n 's/^  forge      \([^ ,]*\).*/\1/p' "$work/harness.log")
	net=$(sed -n 's/^  network    \(.*\)$/\1/p' "$work/harness.log")
	envfile=$(sed -n 's/^  env file   \([^ ]*\) .*/\1/p' "$work/harness.log")
	die "the harness failed"
fi
forge=$(sed -n 's/^  forge      \([^ ,]*\).*/\1/p' "$work/harness.log")
net=$(sed -n 's/^  network    \(.*\)$/\1/p' "$work/harness.log")
envfile=$(sed -n 's/^  env file   \([^ ]*\) .*/\1/p' "$work/harness.log")
[ -n "$forge" ] && [ -n "$net" ] && [ -f "$envfile" ] || die "cannot find the kept forge in the harness's output"

# The tokens, from the harness's env file, into this shell only.
token() {
	sed -n "s/^TOUCHMARK_E2E_$1_TOKEN=//p" "$envfile"
}
admin_token=$(token ADMIN)
reader_token=$(token READER)
writer_token=$(token WRITER)
writer=$(sed -n 's/^TOUCHMARK_E2E_WRITER_LOGIN=//p' "$envfile")
org=$(sed -n 's/^TOUCHMARK_E2E_ORG=//p' "$envfile")
flavor=$(sed -n 's/^TOUCHMARK_E2E_FLAVOR=//p' "$envfile")

# api METHOD PATH [JSON]: the forge's API as the admin, through curl in the
# forge container; the token goes in on standard input.
api() {
	local -a data=()
	[ $# -ge 3 ] && data=(-H 'Content-Type: application/json' -d "$3")
	printf 'Authorization: token %s\n' "$admin_token" |
		docker exec -i "$forge" curl -sS -f -H @- -X "$1" ${data[@]+"${data[@]}"} "$url/api/v1$2"
}

suffix=$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')
target=image-e2e-$suffix
hub_id='image-e2e'
log "creating $org/$target with an opt-in file"
api POST "/orgs/$org/repos" "{\"name\":\"$target\",\"auto_init\":true,\"default_branch\":\"main\"}" >/dev/null
api POST "/repos/$org/$target/contents/.engineering-assets.yml" \
	"{\"content\":\"$(printf 'version: 1\n' | base64 | tr -d '\n')\",\"message\":\"opt in\"}" >/dev/null

pack_file='This file is shipped by the image-e2e hub of touchmark.
It is long enough to prove where it came from.
'
hub=$work/hub
mkdir -p "$hub/packs/base"
printf 'version: 1\nid: %s\nproviders:\n  - id: forge\n    type: %s\n    url: %s\n    writer: %s\n' \
	"$hub_id" "$flavor" "$url" "$writer" >"$hub/hub.yml"
printf 'version: 1\ndefaults:\n  packs: [base]\ntargets:\n  - repo: %s/%s\n' "$org" "$target" >"$hub/targets.yml"
printf '%s' "$pack_file" >"$hub/packs/base/SHARED.md"
# Under Git Bash, git for Windows needs a native path: MSYS_NO_PATHCONV is
# on for docker's sake.
hubgit=$(host_path "$hub")
git -C "$hubgit" init -q -b master
git -C "$hubgit" -c core.autocrlf=false add -A
git -C "$hubgit" -c core.autocrlf=false -c user.name=image-e2e -c user.email=image-e2e@example.invalid commit -q -m hub

# run [DOCKER OPTIONS...] -- TOUCHMARK ARGS...: the image in the forge's
# network namespace, the hub read-only, the tokens by name.
run() {
	local -a opts=()
	while [ "$1" != -- ]; do
		opts+=("$1")
		shift
	done
	shift
	TOUCHMARK_FORGE_READ_TOKEN=$reader_token TOUCHMARK_FORGE_WRITE_TOKEN=$writer_token \
		docker run --rm --network "container:$forge" -v "$(host_path "$hub"):/hub:ro" -w /tmp \
		"${opts[@]}" "$image" "$@"
}
fp=localhost:3000/4242

log "plan (the reader, the image's user)"
out=$(run -e TOUCHMARK_FORGE_READ_TOKEN -- plan --hub /hub --hub-fp "$fp" --format json)
grep -q '"outcome": *"opened"' <<<"$out" || die "plan does not open a pull request: $out"

log "distribute (the writer, the image's user)"
out=$(run -e TOUCHMARK_FORGE_WRITE_TOKEN -- distribute --hub /hub --hub-fp "$fp" --format json)
grep -q '"outcome": *"opened"' <<<"$out" || die "distribute did not open a pull request: $out"

pulls=$(api GET "/repos/$org/$target/pulls?state=open")
grep -q "\"ref\":\"touchmark/$hub_id\"" <<<"$pulls" || die "no open pull request from touchmark/$hub_id: $pulls"
grep -q "\"login\":\"$writer\"" <<<"$pulls" || die "the pull request is not the writer's: $pulls"
shipped=$(api GET "/repos/$org/$target/raw/SHARED.md?ref=touchmark/$hub_id")
[ "$shipped" = "${pack_file%$'\n'}" ] || [ "$shipped" = "$pack_file" ] || die "SHARED.md on the sync branch differs from the pack"

log "distribute again (an arbitrary uid, HOME=/tmp, as the GitHub Action runs it)"
out=$(run -e TOUCHMARK_FORGE_WRITE_TOKEN --user 1001:1001 -e HOME=/tmp -- distribute --hub /hub --hub-fp "$fp" --format json)
grep -q '"outcome": *"unchanged"' <<<"$out" || die "a second distribute is not unchanged: $out"
grep -q '"ops": *\[\]' <<<"$out" || die "a second distribute wrote: $out"
for t in "$reader_token" "$writer_token" "$admin_token"; do
	if grep -qF -- "$t" <<<"$out"; then die "a token reached the output"; fi
done

version=$(docker run --rm "$image" version)
log "ok: $version, $(docker run --rm --entrypoint git "$image" version), against $forge_image"
