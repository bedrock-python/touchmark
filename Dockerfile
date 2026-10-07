# syntax=docker/dockerfile:1.27.1@sha256:4edf897a3ffa55b89f906fc8cc78afdb3f1834cc9c7083565e611a8a7d5fe99e
#
# The touchmark image, ghcr.io/bedrock-python/touchmark: touchmark and git
# (2.45 or newer, without git-lfs) on Alpine, running as the non-root user
# touchmark (uid 65532). docs/project/release.md explains how a release builds,
# signs and attests it; README.md how to use it.
#
# Why Alpine and not a shell-less (distroless) base: the engine itself never
# starts a shell (it runs git with an argv), but the hub template's CI jobs
# run touchmark inside this image, and every CI that does so needs a few
# programs in it:
#   - GitLab's Docker executor runs the job script with sh or bash and needs
#     grep (docs.gitlab.com/ci/docker/using_docker_images/, "Image
#     requirements");
#   - a Gitea runner keeps a job container alive with /bin/sleep, a Forgejo
#     runner with tail -f /dev/null, and both run `run:` steps with a shell;
#   - git needs its https helper (libcurl) and a CA bundle.
# Alpine gives busybox (sh, grep, sleep, tail) and git with nothing else; no
# git-lfs, no perl, no node.
#
# Build it from source for the host (what CI and scripts/release/check-image.sh
# use):
#
#   docker buildx build --load -t touchmark:dev .
#
# A release builds it with scripts/release/build-image.sh, which passes
# BINARY=prebuilt: the image then holds the very binary of the release
# archives, which the script takes from them and puts at
# $TARGETPLATFORM/touchmark in the build context, and this file never
# compiles anything.
#
# The bases are pinned by digest in the FROM lines, where Dependabot's docker
# ecosystem finds and moves them (.github/dependabot.yml). The git
# package comes from Alpine's repository at build time, so a later build of
# the same commit can carry a newer git of the same Alpine branch.

# BINARY names the stage the binary comes from: source (compile it here) or
# prebuilt (the release archives' binary).
ARG BINARY=source

# source: compile touchmark for the target platform on the build platform,
# with the flags .goreleaser.yaml uses (and -buildvcs=false: the build
# context has no .git).
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine3.24@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS source
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=
ARG DATE=
WORKDIR /src
ENV CGO_ENABLED=0 GOFLAGS=-mod=readonly GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY schemas ./schemas
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
	GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -buildvcs=false \
	-ldflags="-s -w -X github.com/bedrock-python/touchmark/internal/cli.Version=${VERSION} -X github.com/bedrock-python/touchmark/internal/cli.Commit=${COMMIT} -X github.com/bedrock-python/touchmark/internal/cli.Date=${DATE}" \
	-o /out/touchmark ./cmd/touchmark

# prebuilt: the release archive's binary for this platform.
FROM scratch AS prebuilt
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/touchmark /out/touchmark

FROM ${BINARY} AS binary

FROM alpine:3.24.2@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ARG VERSION=dev
ARG COMMIT=
ARG DATE=

# git with its https helper, and nothing else: no git-lfs, so that no target
# can make git run it (docs/concepts/security.md). The checks fail the build
# on an Alpine whose git is older than distribute needs (2.45) or that pulls
# in git-lfs.
RUN <<EOF
set -eu
apk add --no-cache git
v=$(git version)
rest=${v#git version }
major=${rest%%.*}
rest=${rest#*.}
minor=${rest%%.*}
if ! { [ "$major" -gt 2 ] || { [ "$major" -eq 2 ] && [ "$minor" -ge 45 ]; }; }; then
	echo "$v is older than 2.45" >&2
	exit 1
fi
if command -v git-lfs >/dev/null || git lfs version >/dev/null 2>&1; then
	echo "git-lfs is installed" >&2
	exit 1
fi
addgroup -S -g 65532 touchmark
adduser -S -D -u 65532 -G touchmark -h /home/touchmark -s /sbin/nologin touchmark
EOF

# The hub checkout of a CI job usually belongs to another user than the job:
# GitLab's helper clones as root, and a Gitea or Forgejo job container sees
# the runner's files. git refuses to read a repository another user owns
# ("detected dubious ownership"), so the system config trusts every
# directory. It reaches only the hub and the local commands: touchmark runs
# git in a target's clone, which it creates itself, with GIT_CONFIG_NOSYSTEM,
# so no system setting applies there.
COPY --chmod=0644 <<EOF /etc/gitconfig
[safe]
	directory = *
EOF

COPY --from=binary --chmod=0755 /out/touchmark /usr/local/bin/touchmark

LABEL org.opencontainers.image.title="touchmark" \
	org.opencontainers.image.description="Keep shared files in sync across many repositories through pull requests" \
	org.opencontainers.image.url="https://bedrock-python.github.io/touchmark/" \
	org.opencontainers.image.source="https://github.com/bedrock-python/touchmark" \
	org.opencontainers.image.documentation="https://bedrock-python.github.io/touchmark/guide/ci/" \
	org.opencontainers.image.licenses="Apache-2.0" \
	org.opencontainers.image.version="${VERSION}" \
	org.opencontainers.image.revision="${COMMIT}" \
	org.opencontainers.image.created="${DATE}"

USER 65532:65532
ENTRYPOINT ["/usr/local/bin/touchmark"]
CMD ["--help"]
