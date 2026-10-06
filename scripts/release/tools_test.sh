#!/usr/bin/env bash
# tools_test.sh tests the release's own scripts and the version wiring of
# release-please, without a registry or GitHub:
#   - check-version.sh, in scratch repositories: the tags it accepts, from
#     a clone of the default branch as publish.yml runs it, the ones it
#     refuses (not on the default branch, a manifest or an action.yml of
#     another version), the commit it reports, and where the floating image
#     tags go;
#   - build-image.sh, with a fake docker: the archives it takes, checked
#     against checksums.txt, the build it asks for, the digest it prints;
#   - every file with a version between x-release-please markers is in the
#     extra-files of release-please-config.json, and the other way round,
#     and action.yml has the one TM_VERSION line release-please rewrites.
# CI runs it (job release-snapshot); it also runs under Git Bash.
#
# Usage: scripts/release/tools_test.sh

set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
failures=0

fail() {
	echo "FAIL: $*"
	failures=$((failures + 1))
}

# refused PATTERN TEXT: the last run failed, and TEXT says PATTERN.
refused() {
	[ "$code" != 0 ] && grep -q -- "$1" <<<"$2"
}

# succeeded WANT: the last run succeeded and printed WANT.
succeeded() {
	[ "$code" = 0 ] && [ "$out" = "$1" ]
}

# --- check-version.sh ------------------------------------------------------

# A repository whose master has releases v0.1.0, v0.1.1, v0.2.0 and v0.10.0,
# each commit with the manifest and action.yml of its version.
repo=$tmp/repo
git init -q -b master "$repo"
g() { git -C "$repo" -c core.autocrlf=false -c user.name=t -c user.email=t@example.invalid "$@"; }
# release VERSION [MANIFEST [ACTION]]: a commit on the current branch whose
# manifest and action.yml say MANIFEST and ACTION (default VERSION), tagged
# vVERSION.
release() {
	printf '{\n  ".": "%s"\n}\n' "${2:-$1}" >"$repo/.release-please-manifest.json"
	sed "s/^        TM_VERSION: .*/        TM_VERSION: \"${3:-$1}\" # x-release-please-version/" "$root/action.yml" >"$repo/action.yml"
	g add -A
	g commit -q -m "chore(master): release ${1}"
	g tag "v$1"
}
release 0.1.0
release 0.1.1
release 0.2.0
release 0.10.0

# check TAG [VAR=VALUE...]: runs check-version.sh for TAG with master
# checked out, as publish.yml runs it; leaves $code and $out.
check() {
	local tag=$1
	shift
	code=0
	out=$(cd "$repo" && g checkout -q master 2>/dev/null; env RELEASE_BRANCH=master "$@" bash "$here/check-version.sh" "$tag" 2>&1) || code=$?
}

# expect_outputs TAG LATEST LATEST_MINOR
expect_outputs() {
	check "$1"
	local v=${1#v} commit want
	commit=$(g rev-parse "refs/tags/$1^{commit}")
	want="tag=$1
commit=$commit
version=$v
minor=${v%.*}
latest=$2
latest_minor=$3"
	succeeded "$want" || fail "check-version.sh $1: exit $code, got:
$out
want:
$want"
}
expect_outputs v0.10.0 true true
expect_outputs v0.2.0 false true
expect_outputs v0.1.1 false true
expect_outputs v0.1.0 false false

# expect_refused TAG PATTERN
expect_refused() {
	check "$1"
	refused "$2" "$out" || fail "check-version.sh $1: exit $code, want a refusal with \"$2\": $out"
}
g tag not-a-version v0.2.0
g tag v0.3 v0.2.0
g tag v0.3.0-rc.1 v0.2.0
g tag v01.0.0 v0.2.0
expect_refused not-a-version "is not a release tag"
expect_refused v0.3 "is not a release tag"
expect_refused v0.3.0-rc.1 "is not a release tag"
expect_refused v01.0.0 "is not a release tag"
expect_refused v9.9.9 "no tag v9.9.9"

# A tag on a branch that never reached master.
g checkout -q -b side master
release 0.11.0
g checkout -q master
expect_refused v0.11.0 "is not on master"
code=0
out=$(cd "$repo" && g checkout -q --detach v0.10.0 && bash "$here/check-version.sh" v0.10.0 2>&1) || code=$?
refused "no origin/master" "$out" || fail "check-version.sh without origin/master: exit $code: $out"

# A tag whose manifest or action.yml names another version.
g checkout -q master
release 0.12.0 0.11.9
expect_refused v0.12.0 '.release-please-manifest.json at v0.12.0 says "0.11.9"'
g checkout -q master
release 0.13.0 0.13.0 0.0.0
expect_refused v0.13.0 'action.yml at v0.13.0 runs version "0.0.0"'

# Wherever HEAD is, the tag's own commit is checked: from a branch that
# never reached master, a tag on master passes and reports its commit.
code=0
out=$(cd "$repo" && g checkout -q side && RELEASE_BRANCH=master bash "$here/check-version.sh" v0.2.0 2>&1) || code=$?
if [ "$code" != 0 ] || ! grep -qx "commit=$(g rev-parse 'refs/tags/v0.2.0^{commit}')" <<<"$out"; then
	fail "check-version.sh with HEAD on another branch: exit $code: $out"
fi
g checkout -q master

# --- build-image.sh --------------------------------------------------------

mkdir -p "$tmp/bin" "$tmp/dist"
cat >"$tmp/bin/docker" <<'EOF'
#!/usr/bin/env bash
# The fake docker: records the build's arguments, keeps a copy of the build
# context's binaries, and writes buildx's metadata file.
printf '%s\n' "$@" >"$FAKE/args"
ctx= meta=
prev=
for a in "$@"; do
	[ "$prev" = build ] && ctx=$a
	[ "$prev" = --metadata-file ] && meta=$a
	prev=$a
done
if command -v cygpath >/dev/null 2>&1; then ctx=$(cygpath -u "$ctx"); meta=$(cygpath -u "$meta"); fi
cp -r "$ctx" "$FAKE/context"
printf '{\n  "buildx.build.ref": "x",\n  "containerimage.digest": "sha256:%s"\n}\n' "$(printf "%064d" 0 | tr 0 d)" >"$meta"
EOF
chmod +x "$tmp/bin/docker"
for arch in amd64 arm64; do
	mkdir -p "$tmp/src/$arch"
	printf 'binary %s\n' "$arch" >"$tmp/src/$arch/touchmark"
	(cd "$tmp/src/$arch" && tar -czf "$tmp/dist/touchmark_0.1.0_linux_${arch}.tar.gz" touchmark)
done
printf 'darwin\n' >"$tmp/dist/touchmark_0.1.0_darwin_arm64.tar.gz"
(cd "$tmp/dist" && sha256sum -- *.tar.gz | sed 's/ \*/  /') >"$tmp/checksums.txt"
mv "$tmp/checksums.txt" "$tmp/dist/checksums.txt"

# build [DIR] [TAG]: runs build-image.sh; leaves $code and $out (stdout).
build() {
	rm -rf "$tmp/fake"
	mkdir -p "$tmp/fake"
	code=0
	out=$(PATH="$tmp/bin:$PATH" FAKE=$tmp/fake bash "$here/build-image.sh" "${1:-$tmp/dist}" registry.example.invalid/touchmark "${2:-v0.1.0}" 2>"$tmp/build.err") || code=$?
}
build
succeeded "digest=sha256:$(printf "%064d" 0 | tr 0 d)" || fail "build-image.sh: exit $code, out \"$out\": $(cat "$tmp/build.err")"
for arch in amd64 arm64; do
	[ "$(cat "$tmp/fake/context/linux/$arch/touchmark" 2>/dev/null)" = "binary $arch" ] ||
		fail "build-image.sh: the context lacks the $arch binary of its archive"
done
cmp -s "$tmp/fake/context/Dockerfile" "$root/Dockerfile" || fail "build-image.sh: the context lacks the Dockerfile"
for want in buildx build "--platform" "linux/amd64,linux/arm64" "BINARY=prebuilt" "VERSION=v0.1.0" \
	"COMMIT=$(git -C "$root" rev-parse HEAD)" "type=provenance,mode=max" \
	"type=image,name=registry.example.invalid/touchmark,push-by-digest=true,name-canonical=true,push=true" \
	"index:org.opencontainers.image.source=https://github.com/bedrock-python/touchmark"; do
	grep -qxF -- "$want" "$tmp/fake/args" || fail "build-image.sh: the build lacks $want"
done
grep -qx 'type=sbom,generator=docker.io/docker/buildkit-syft-scanner:[^@]*@sha256:[0-9a-f]\{64\}' "$tmp/fake/args" ||
	fail "build-image.sh: the SBOM scanner is not pinned by digest"

# What it refuses.
cp -r "$tmp/dist" "$tmp/tampered"
printf 'other\n' >"$tmp/src/amd64/touchmark"
(cd "$tmp/src/amd64" && tar -czf "$tmp/tampered/touchmark_0.1.0_linux_amd64.tar.gz" touchmark)
build "$tmp/tampered"
if ! refused "checksums.txt says" "$(cat "$tmp/build.err")" || [ -e "$tmp/fake/args" ]; then
	fail "build-image.sh built from an archive that does not match checksums.txt"
fi
cp -r "$tmp/dist" "$tmp/missing"
rm "$tmp/missing/touchmark_0.1.0_linux_arm64.tar.gz"
build "$tmp/missing"
refused "want one touchmark_\*_linux_arm64.tar.gz" "$(cat "$tmp/build.err")" || fail "build-image.sh built without the arm64 archive"
cp -r "$tmp/dist" "$tmp/unlisted"
grep -v linux_arm64 "$tmp/dist/checksums.txt" >"$tmp/unlisted/checksums.txt"
build "$tmp/unlisted"
refused "does not list" "$(cat "$tmp/build.err")" || fail "build-image.sh built from an archive checksums.txt does not list"
build "$tmp/dist" 0.1.0
refused "is not a version tag" "$(cat "$tmp/build.err")" || fail "build-image.sh took the tag 0.1.0"

# --- release-please's version files ------------------------------------------

listed=$(sed -n 's/^ *"path": *"\([^"]*\)".*$/\1/p' "$root/release-please-config.json" | LC_ALL=C sort)
marked=$(cd "$root" && git ls-files --cached --others --exclude-standard | while IFS= read -r f; do
	case $f in scripts/release/*) continue ;; esac
	if [ -f "$f" ] && grep -qE '(#|<!--)[[:space:]]*x-release-please-(start-)?version' "$f"; then echo "$f"; fi
done | LC_ALL=C sort)
[ "$listed" = "$marked" ] || fail "release-please-config.json extra-files: $(tr '\n' ' ' <<<"$listed"); files with version markers: $(tr '\n' ' ' <<<"$marked")"
lines=$(grep -c 'x-release-please-version' "$root/action.yml" || true)
if [ "$lines" != 1 ] || ! grep -qE '^        TM_VERSION: "[0-9]+\.[0-9]+\.[0-9]+" # x-release-please-version$' "$root/action.yml"; then
	fail "action.yml: want one TM_VERSION line with an x-release-please-version comment"
fi
manifest=$(sed -n 's/^ *"\." *: *"\([^"]*\)".*$/\1/p' "$root/.release-please-manifest.json")
grep -qx "        TM_VERSION: \"$manifest\" # x-release-please-version" "$root/action.yml" ||
	fail "action.yml's TM_VERSION is not the manifest's $manifest"

if [ "$failures" -gt 0 ]; then
	echo "tools_test.sh: $failures failure(s)"
	exit 1
fi
echo "tools_test.sh: ok"
