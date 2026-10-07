# Installation

Most people never install touchmark by hand: they create a hub from
[the template](https://github.com/bedrock-python/engineering-assets-template), and the
hub's CI runs it. Install it on your machine to try a pack on a checkout, to run
`setup` once, or to plan from a laptop.

| Where | How |
|---|---|
| GitHub Actions | `uses: bedrock-python/touchmark@<commit> # vX.Y.Z`, see [Run it in CI](../guide/ci.md#github-actions) |
| GitLab CI | the image `ghcr.io/bedrock-python/touchmark:X.Y.Z@sha256:<digest>`, see [Run it in CI](../guide/ci.md#gitlab-ci) |
| Gitea and Forgejo Actions | the image as the job's `container:`, pinned by digest |
| Your machine | an archive from [Releases](https://github.com/bedrock-python/touchmark/releases), or `go install` |

Every release has archives for Linux, macOS and Windows on amd64 and arm64, and the image
for linux/amd64 and linux/arm64. All of them carry the same binary, and the version is
the same everywhere: the tag `vX.Y.Z`, the image tags `X.Y.Z`, `X.Y` and `latest`, and
`touchmark version`.

Pin a version, and let Dependabot (the Action) or Renovate (the image) propose the next
one in a pull request you review.

## From an archive

```sh
v=X.Y.Z
base=https://github.com/bedrock-python/touchmark/releases/download/v$v
curl -fsSLO "$base/touchmark_${v}_linux_amd64.tar.gz"
curl -fsSLO "$base/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
gh attestation verify "touchmark_${v}_linux_amd64.tar.gz" --repo bedrock-python/touchmark
tar -xzf "touchmark_${v}_linux_amd64.tar.gz" touchmark
./touchmark version
```

`gh attestation verify` checks the archive's build provenance: that GitHub Actions built
it in `bedrock-python/touchmark`. [Releases](../project/release.md#verify-a-release) has
the stricter checks, down to the workflow that signed.

On Windows, take `touchmark_X.Y.Z_windows_amd64.zip` and put `touchmark.exe` on your
`PATH`. On macOS, `darwin_arm64` or `darwin_amd64`.

## With Go

```sh
go install github.com/bedrock-python/touchmark/cmd/touchmark@latest
```

Go 1.26 or newer. A binary built this way reports the module version in
`touchmark version`.

## The image

`ghcr.io/bedrock-python/touchmark` holds touchmark and git (2.45 or newer, no git-lfs)
on Alpine, and runs as the non-root user `touchmark` (uid 65532). Its entrypoint is
`touchmark`. It has a shell because the CIs that run it need one (GitLab's Docker
executor runs the job script with `sh`), but touchmark never starts a shell: it runs git
with an argument list. Its system git config trusts every directory (`safe.directory`),
so a hub checkout that belongs to another user, as in most CI jobs, can be read.

```sh
docker run --rm ghcr.io/bedrock-python/touchmark:X.Y.Z version
```

On your machine, in a checkout of a target, with the hub next to it; `--user` lets
`apply` write files you own:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -v "$HOME/src/engineering-assets:/hub:ro" -w /work \
  ghcr.io/bedrock-python/touchmark:X.Y.Z status --hub /hub --repo acme/billing
```

## Requirements

- **git 2.31 or newer** for `check`, `status`, `apply` and `manifest`; **git 2.45 or
  newer** for `plan` and `distribute`, which read every target into a repository of their
  own with lazy fetches off, `check-attr --source` and `--attr-source`. An offline `plan`
  (a hub pull request without read credentials) reads no target and runs on 2.31. The
  image brings its own git.
- **A full clone of the hub.** touchmark reads the hub's whole history and refuses a
  shallow or partial clone.
- **Network access** to the platforms in `hub.yml`, for `plan`, `distribute`, `doctor`,
  `setup` and `migrate`. `check`, `status`, `apply` and `manifest` work offline.

Next: create a hub [on GitHub](github.md), [on GitLab](gitlab.md) or
[on Gitea or Forgejo](gitea-forgejo.md).
