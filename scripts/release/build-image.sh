#!/usr/bin/env bash
# build-image.sh builds the touchmark image for linux/amd64 and linux/arm64
# from the Linux archives of a release, and pushes it by digest, untagged,
# with BuildKit's SBOM and SLSA provenance attached. The publish workflow
# runs it on the archives of the GitHub release, ci.yml on those of a
# goreleaser snapshot, pushed to a registry on the runner.
#
# Usage: scripts/release/build-image.sh DIR IMAGE TAG
#
#   DIR    holds checksums.txt and the archives
#          touchmark_<version>_linux_{amd64,arm64}.tar.gz (goreleaser's dist/,
#          or the files of the GitHub release)
#   IMAGE  the image name, without a tag: ghcr.io/bedrock-python/touchmark
#   TAG    the release tag, vX.Y.Z: the image's version label
#
# The archives are checked against checksums.txt, and their binaries become
# the image's (the Dockerfile's stage prebuilt): the image holds the release
# archives' binary byte for byte. The labels name TAG, the commit at HEAD and
# that commit's time, as goreleaser stamps them into the binary. The SBOM
# scanner is pinned by digest: BuildKit's default is a floating tag, run
# inside the build that cosign then signs.
#
# It prints digest=sha256:<64 hex>, the index's digest, on stdout; the
# build's log goes to stderr. It needs docker buildx with a builder that can
# build both platforms and push to IMAGE's registry.

set -euo pipefail
export MSYS_NO_PATHCONV=1

dir=${1:?usage: build-image.sh DIR IMAGE TAG}
image=${2:?usage: build-image.sh DIR IMAGE TAG}
tag=${3:?usage: build-image.sh DIR IMAGE TAG}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
scanner=docker.io/docker/buildkit-syft-scanner:1.12.0@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9
description="Keep shared files in sync across many repositories through pull requests"

fail() {
	echo "build-image.sh: $*" >&2
	exit 1
}
# host_path: a path docker can read; under Git Bash, docker is a Windows
# program.
host_path() {
	if command -v cygpath >/dev/null 2>&1; then cygpath -m -- "$1"; else printf '%s\n' "$1"; fi
}

[[ $image =~ ^[a-z0-9][a-z0-9.:/_-]*/[a-z0-9._-]+$ ]] || fail "$image is not an image name without a tag"
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail "$tag is not a version tag vX.Y.Z"
[ -f "$dir/checksums.txt" ] || fail "no checksums.txt in $dir"

ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
for arch in amd64 arm64; do
	shopt -s nullglob
	archives=("$dir"/touchmark_*_linux_"$arch".tar.gz)
	shopt -u nullglob
	[ "${#archives[@]}" = 1 ] || fail "want one touchmark_*_linux_$arch.tar.gz in $dir, found ${#archives[@]}"
	name=$(basename "${archives[0]}")
	want=$(awk -v f="$name" '$2 == f { print $1 }' "$dir/checksums.txt")
	[[ $want =~ ^[0-9a-f]{64}$ ]] || fail "checksums.txt does not list $name"
	got=$(sha256sum "${archives[0]}" | cut -d' ' -f1)
	[ "$got" = "$want" ] || fail "$name is $got, checksums.txt says $want"
	mkdir -p "$ctx/linux/$arch"
	tar -xzf "${archives[0]}" -C "$ctx/linux/$arch" touchmark
	echo "build-image.sh: linux/$arch from $name ($got)" >&2
done
cp "$root/Dockerfile" "$ctx/Dockerfile"

commit=$(git -C "$(host_path "$root")" rev-parse HEAD)
date=$(TZ=UTC git -C "$(host_path "$root")" log -1 --format=%cd --date=format-local:%Y-%m-%dT%H:%M:%SZ HEAD)
meta=$ctx.metadata.json
trap 'rm -rf "$ctx" "$meta"' EXIT

# GHCR shows the index's annotations on the package page and links the
# package to the repository by its source.
docker buildx build "$(host_path "$ctx")" \
	--platform linux/amd64,linux/arm64 \
	--build-arg BINARY=prebuilt \
	--build-arg VERSION="$tag" \
	--build-arg COMMIT="$commit" \
	--build-arg DATE="$date" \
	--annotation "index:org.opencontainers.image.source=https://github.com/bedrock-python/touchmark" \
	--annotation "index:org.opencontainers.image.description=$description" \
	--annotation "index:org.opencontainers.image.licenses=Apache-2.0" \
	--annotation "index:org.opencontainers.image.version=$tag" \
	--annotation "index:org.opencontainers.image.revision=$commit" \
	--attest "type=sbom,generator=$scanner" \
	--attest type=provenance,mode=max \
	--output "type=image,name=$image,push-by-digest=true,name-canonical=true,push=true" \
	--metadata-file "$(host_path "$meta")" >&2

digest=$(grep -o '"containerimage.digest": *"sha256:[0-9a-f]\{64\}"' "$meta" | grep -o 'sha256:[0-9a-f]\{64\}' || true)
[[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || fail "no image digest in buildx's metadata: $(cat "$meta")"
echo "digest=$digest"
