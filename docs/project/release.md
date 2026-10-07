# Releases

How touchmark is released, what to set up before the first release, and how anyone checks what a release contains. As in the other bedrock-python repositories, [Release Please](https://github.com/googleapis/release-please) cuts the release and a publish workflow builds it. The configuration lives in [`release-please-config.json`](https://github.com/bedrock-python/touchmark/blob/master/release-please-config.json), [`.release-please-manifest.json`](https://github.com/bedrock-python/touchmark/blob/master/.release-please-manifest.json), [`.github/workflows/release-please.yml`](https://github.com/bedrock-python/touchmark/blob/master/.github/workflows/release-please.yml), [`.github/workflows/publish.yml`](https://github.com/bedrock-python/touchmark/blob/master/.github/workflows/publish.yml), [`.goreleaser.yaml`](https://github.com/bedrock-python/touchmark/blob/master/.goreleaser.yaml), [`Dockerfile`](https://github.com/bedrock-python/touchmark/blob/master/Dockerfile), [`action.yml`](https://github.com/bedrock-python/touchmark/blob/master/action.yml) and [`scripts/release/`](https://github.com/bedrock-python/touchmark/tree/master/scripts/release).

## What a release contains

| Artifact | Where | Signed | Attested |
|---|---|---|---|
| `touchmark_X.Y.Z_{linux,darwin,windows}_{amd64,arm64}.{tar.gz,zip}` | the GitHub release | through `checksums.txt` | build provenance |
| an SPDX SBOM of each archive, `….sbom.json` | the GitHub release | through `checksums.txt` | build provenance |
| `checksums.txt` and its cosign bundle `checksums.txt.sigstore.json` | the GitHub release | cosign, keyless | |
| `ghcr.io/bedrock-python/touchmark`, linux/amd64 and linux/arm64, tags `X.Y.Z`, `X.Y`, `latest` | GHCR | cosign, keyless | build provenance, in GHCR and on GitHub; BuildKit's SBOM and SLSA provenance in the index |
| the Action, `bedrock-python/touchmark@vX.Y.Z` | the tag | | runs only an image whose attestation checks out |

The archives and the image carry the same binary, byte for byte: goreleaser builds it once per platform with `-trimpath`, CGO off and the version, commit and commit time stamped into `touchmark version`, and the image is built from the release's Linux archives, after their `checksums.txt` signature checks out.

## The flow

```
master ── a conventional commit lands (feat:, fix:, …)
  └─ Release Please (release-please.yml): opens or updates the pull request
     "chore(master): release X.Y.Z": CHANGELOG.md, .release-please-manifest.json,
     and the version in action.yml and README.md
merge it ── Release Please: the tag vX.Y.Z and its GitHub release, with the notes
  └─ Publish (publish.yml), on workflow_run of Release Please:
       check   with master's copy of scripts/release/check-version.sh, never the
               tag's: the tag is vX.Y.Z, its commit is on master, and the manifest
               and action.yml there carry its version
       assets  goreleaser: archives and SBOMs into the release; checksums.txt,
               signed with cosign; a build provenance attestation of every file
       image   built from the release's Linux archives (scripts/release/build-image.sh),
               pushed by digest, untagged, with BuildKit's SBOM and provenance;
               checked (scripts/release/check-image.sh)
       scan    trivy, read-only: a CRITICAL or HIGH vulnerability with a fix turns
               it red; it holds nothing back
       attest  a build provenance attestation of the digest, in GHCR too
       tags    cosign signs the digest; X.Y.Z, X.Y and latest name it
       verify  amd64 and arm64: signature, attestations, tags, SBOM, provenance,
               the version inside, the release files against checksums.txt, the
               Action of the tag, plan and distribute against a live Gitea
```

Every job after `check` checks out the tag's commit by its id and stops if the checkout is anything else. Only `assets`, `attest` and `tags` can request the OIDC token that signs and attests; the scanner runs in a job of its own with read access alone. No image tag names the image until it passed its checks and is attested and signed. The git tag, and with it the Action at `vX.Y.Z`, exist from the moment Release Please creates the release: until `tags` is done, the Action at the new tag finds no image and fails. A failure in `verify` is a red release run to read, not an unpublished release.

**The vulnerability scan.** The scan that blocks is CI's: the job `release-snapshot` builds the image a release would publish and scans it on every pull request, the release pull request included, and every night, so a new vulnerability in the base image shows up before a release does. The scan in `publish.yml` only reports: by then the tag and the release exist, the same base image is in every earlier release, and holding the image back would leave the Action at the tag broken for good.

**Versions.** `feat:` bumps the minor version, `fix:` the patch version; before 1.0 a breaking change (`feat!:`, a `BREAKING CHANGE:` footer) bumps the minor version too (`bump-minor-pre-major`). A `Release-As: x.y.z` footer on a commit forces a version. The manifest starts at `0.0.0`, so the first release, from the commit `feat: initial release`, is `0.1.0`.

**Tags.** A release tag is `vX.Y.Z`, without the component prefix of the org's libraries (`pg-partsmith-v1.7.2`): the Go module proxy and `uses: bedrock-python/touchmark@vX.Y.Z` need bare semver tags (`include-component-in-tag: false`). The release is named `vX.Y.Z` too.

**Version in files.** Release Please writes the new version wherever a file marks it: the `TM_VERSION` line of `action.yml`, which ends in an `x-release-please-version` comment, and the blocks of `README.md` between `x-release-please-start-version` and `x-release-please-end` HTML comments. Every such file must be in the `extra-files` of `release-please-config.json`; `scripts/release/tools_test.sh` fails when one is missing.

**The Action and its image.** `action.yml` at `vX.Y.Z` carries the version `X.Y.Z`, not a digest: the tag is created before the image exists. Its step ([`scripts/action/run.sh`](https://github.com/bedrock-python/touchmark/blob/master/scripts/action/run.sh)) resolves `ghcr.io/bedrock-python/touchmark:X.Y.Z` to a digest once, then:

```sh
gh attestation verify oci://ghcr.io/bedrock-python/touchmark@sha256:<digest> --bundle-from-oci \
  --repo bedrock-python/touchmark \
  --signer-workflow github.com/bedrock-python/touchmark/.github/workflows/publish.yml \
  --source-ref refs/heads/master --deny-self-hosted-runners
```

pulls that digest, checks that the image is labelled `vX.Y.Z`, and runs it by that digest. An image the publish workflow on master did not build fails the attestation; an older release's image under the new tag fails the label. `--bundle-from-oci` reads the attestation that `publish.yml` pushed to GHCR next to the image, so the check needs no GitHub API and works from GitHub Enterprise Server runners too; `gh` still wants a token to start, and the Action gives it the job's. On `master` between releases, `action.yml` names the latest release; before the first one it says `0.0.0`, which the Action refuses.

**Not immutable.** Release Please creates the release and goreleaser adds the files afterwards, so the release cannot be immutable, as in the org's other repositories. The signatures and attestations above carry the guarantees instead.

## Before the first release

Do these once, after the repository exists on GitHub and `master` is pushed. The release needs no secret: the workflows' `GITHUB_TOKEN` opens the release pull request, creates the release, pushes the image and requests the signing certificates.

### Repository settings

touchmark's settings are those of the org's other repositories, and the org's script applies them: `scripts/setup_repo.py` of [python-library-template](https://github.com/bedrock-python/python-library-template). It is idempotent and needs `gh` with admin rights on the repository and Python 3.11 or newer. Run it once the first CI run on `master` has reported `All checks passed`: the ruleset requires that check, and the script leaves the rule out while the check has never reported, since a required check that never reports blocks every merge. CI's job `template` checks out `bedrock-python/engineering-assets-template`; until that repository is public, the job says so in a notice and checks nothing, so the first run can pass without it.

```sh
git clone --depth 1 https://github.com/bedrock-python/python-library-template.git ../python-library-template
python ../python-library-template/scripts/setup_repo.py bedrock-python/touchmark \
  --topics go,cli,github-actions,gitlab,gitea,forgejo,pull-requests
# Two of its defaults are for Python libraries: it creates the environment
# pypi whenever publish.yml exists, and adds the topic python.
gh api -X DELETE repos/bedrock-python/touchmark/environments/pypi
gh repo edit bedrock-python/touchmark --remove-topic python
```

The first push ran `docs.yml` before Pages existed, so its `deploy` job failed. Once the script has run, open Actions → Docs and re-run that run's failed jobs (`docs.yml` is the org's and has no *Run workflow*). Until then the README's Docs badge and every link to the site, the agents page included, answer 404.

What it sets:

| Setting | Value |
|---|---|
| Ruleset `master-rules` on the default branch | no deletion and no force push; every change through a pull request, with no approval required, merged as a merge commit or squashed; the required check `All checks passed` (the sentinel job of `ci.yml`), not strict; nobody on the bypass list. Classic branch protection is removed. |
| Merge options | squash and merge commits on, rebase merging off; branches deleted on merge; no wiki, no projects |
| Actions | `GITHUB_TOKEN` read-only by default; **Allow GitHub Actions to create and approve pull requests** on, so that Release Please can open its pull request |
| Pages | the environment `github-pages`, Pages built by GitHub Actions (`build_type: workflow`, the org's `docs.yml`) |
| Homepage | `https://bedrock-python.github.io/touchmark/` |
| Security | secret scanning with push protection, Dependabot alerts and security updates, private vulnerability reporting (`SECURITY.md` relies on it) |
| Topics | the `--topics` above, `go` first, and `python`, which the second command removes; topics already set are kept |

Then, as on pg-partsmith, three settings the script leaves alone: in *Settings → Environments → github-pages*, **Deployment branches and tags** set to selected branches, `master` only; in *Settings → Pages*, **Enforce HTTPS**; and Codecov, which the script prints as its last step: enable the repository at [codecov.io](https://app.codecov.io/gh/bedrock-python) and add its upload token as the Actions secret `CODECOV_TOKEN`. Without the token the upload fails quietly (`continue-on-error`, as in the org); the coverage gate of `make test` in the job still holds.

Run the org's script rather than a Go port of it: the script is where the org's settings are written down, a port would drift from it, and no repository of the org keeps a copy (a new library deletes its own before the first commit). The two commands after it go away if the script creates `pypi` only for a `publish.yml` that uses that environment and takes the language topic as an option; that change belongs in python-library-template.

touchmark adds nothing stricter to them on purpose: no required approval (a maintainer merges, as in the org), the sentinel as the only required check (it covers every job), no environment that approves a release (merging the release pull request is the approval), no release immutability (goreleaser uploads to the release after Release Please creates it) and no required SHA pinning (below).

### Also for touchmark

1. **Actions** (Settings → Actions → General): all actions allowed, as in the org's repositories, and **Require actions to be pinned to a full-length commit SHA** off: `docs.yml` and `release-please.yml` are the org's, byte for byte, and take their actions by tag. touchmark's own workflows pin every action by commit, with the version in a comment; Dependabot moves both.
2. **The label `e2e-gitlab`** (Issues → Labels): a maintainer adds it to a pull request to run the GitLab tests (`e2e-gitlab.yml`).
3. **Tags** (Settings → Rules → Rulesets): a tag ruleset for `v*` that restricts updates and deletions and blocks force pushes. Restrict creations only with the GitHub Actions app on the ruleset's bypass list (GitHub accepts it there in an organization's repository): Release Please creates the tags with `GITHUB_TOKEN`. Either way, a tag pushed by hand is not published unless it is `vX.Y.Z`, its commit is on `master`, and both the manifest and `action.yml` at that commit carry its version: `publish.yml` checks this with `master`'s copy of `check-version.sh`, never the tag's, and then builds the tag's commit by its id. The org's other repositories have no tag ruleset; touchmark adds it because the Go module proxy and every hub that pins the Action keep the first commit a tag named.
4. **Packages** (organisation settings): allow public packages.
5. **The first release pull request.** `CHANGELOG.md` has no release yet, so Release Please writes `# Changelog`, the 0.1.0 section, and then the file's own header, demoted to `## Changelog`. Before merging, edit `CHANGELOG.md` in the pull request: the header back on top, the 0.1.0 section below it. Later releases go in above the previous one.
6. **The package.** The first Publish run creates `ghcr.io/bedrock-python/touchmark` private, and its `verify` jobs fail at the anonymous steps. In the package's settings: check that it is linked to the repository (the index's `org.opencontainers.image.source`) and that the repository's Actions have write access, then **Change visibility → Public**, and re-run the failed jobs. Hubs and the Action pull anonymously.

PyPI is not part of a release yet: see [Adding PyPI later](#adding-pypi-later).

## Release, step by step

1. **Merge into master** with conventional pull request titles (squash merges keep them as the commit subject). Release Please opens or updates its pull request on every push.
2. **Review the release pull request**: the new section of `CHANGELOG.md` (reword it if needed), the version in `.release-please-manifest.json`, `action.yml` and `README.md`. A pull request opened with `GITHUB_TOKEN` starts no workflow: **close and reopen it** to run CI, as in the org's other repositories, or the required check never reports.
3. **Merge it.** Release Please tags the merge commit `vX.Y.Z` and creates the release; Publish starts when that Release Please run completes (Actions → Publish).
4. **Check the result** with [Verify a release](#verify-a-release). Hubs get the new version from Dependabot (the Action) and Renovate (the image).
5. **Pin the template and the hubs you maintain** to the release ([below](#after-a-release-the-template-and-the-hubs)).

When something fails:

- **check:** the tag is not on `master`, or the manifest or `action.yml` disagree with it; nothing was published. Fix `master` and let the next release pull request carry the fix.
- **assets, image, attest or tags:** re-run the failed jobs, or Actions → Publish → *Run workflow* on `master` with the tag. goreleaser replaces the files of the release; the image is rebuilt, pushed by digest again, and the tags move to the new digest.
- **Publish skipped or cancelled for a release** (the Release Please run on the merge commit failed and a later run created the release, or the run was cancelled): the tag and the release exist with no files and no image. Run Actions → Publish → *Run workflow* on `master` with the tag.
- **scan:** the image is out, with a known vulnerability that has a fix, usually in the base image. Re-running scans the same image again. Bump the base image's digest in the `Dockerfile` (Dependabot's `docker` pull request, or by hand), merge it as `fix(image): …` so that it cuts a release, and release the next patch. Say in the release notes of the affected release which vulnerability its image carries and which release fixes it.
- **verify:** the release is out; read what failed. A bad release is never fixed by moving or deleting its tag: the Go module proxy keeps the first commit of a tag forever. Merge a `fix:` and release the next patch.

## After a release: the template and the hubs

The hub template (`bedrock-python/engineering-assets-template`) and the bedrock-python hub name touchmark by commit and image digest. Until the first release they carry placeholders marked `TODO(release)`: `bedrock-python/touchmark@0000… # v0.1.0` in `.github/workflows`, and `ghcr.io/bedrock-python/touchmark:0.1.0@sha256:0000…` in `.gitlab-ci.yml` and `.gitea/workflows`. A hub created from a template with placeholders fails at the Action's step or at the image pull.

1. In each of them, replace every placeholder with the release: the tag's commit (`git rev-parse vX.Y.Z^{commit}`, with `# vX.Y.Z` after it) and the image `ghcr.io/bedrock-python/touchmark:X.Y.Z@<digest>`, the digest from `docker buildx imagetools inspect ghcr.io/bedrock-python/touchmark:X.Y.Z --format '{{ .Manifest.Digest }}'`, and delete the `TODO(release)` lines. Afterwards Dependabot moves the Action and Renovate the image.
2. Run `bash scripts/validate.sh --release` there: it fails while a placeholder is left.
3. Run the template's pipelines against the published image ([e2e.md](e2e.md), "The hub template"): `scripts/e2e/gitea.sh` and `scripts/e2e/gitlab.sh` with `--template <template> --touchmark-image ghcr.io/bedrock-python/touchmark@<digest> --run TestTemplate`, and the GitHub dry run `TestTemplateWorkflow` with `TOUCHMARK_E2E_TEMPLATE` in the Docker run of the suite.
4. Merge the change through a pull request in each repository.

Once, when the template is first published:

- *Settings → General*: tick **Template repository**.
- *Settings → Actions → General*: **Disable actions**. The template's workflows are a hub's: they deliver from the default branch and check every pull request of a hub, and in the template itself, with the placeholder `id` and no keys, every run would fail (`check` rejects the placeholder by design). touchmark's own CI checks the template instead (`ci.yml`, job `template`). Dependabot's version updates still open pull requests there.
- A GitLab mirror `gitlab.com/bedrock-python/engineering-assets-template`: create it, and turn **CI/CD** off in its *Settings → General → Visibility, project features, permissions*, for the same reason. Keep it in sync from outside the template: a workflow placed in the template is copied into every hub created from it. Offer it in the template's README and in [GitLab](../getting-started/gitlab.md) only once it exists; until then both give the template's URL on GitHub to import.

## Try a release locally

Nothing here publishes anything: a registry on localhost stands in for GHCR. It needs Docker with buildx and bash (Git Bash on Windows, where `MSYS_NO_PATHCONV=1` keeps docker's paths intact and `$(cygpath -m "$PWD")` stands for `$PWD`), and is what the CI job `release-snapshot` runs on every pull request.

```sh
export MSYS_NO_PATHCONV=1
gr=goreleaser/goreleaser:v2.18.2@sha256:7077423cf5ef643ff56a34b58f93c1364e927e5c3dfa470eeabc44cab1a9c72b

# The configuration, then every file of a release in dist/, stamped v0.0.1-ci.
# Keyless signing needs the publish workflow's identity, so --skip=sign.
docker run --rm -v "$PWD:/src" -w /src $gr check
docker run --rm -v "$PWD:/src" -w /src -e GORELEASER_CURRENT_TAG=v0.0.1-ci $gr release --snapshot --clean --skip=sign

# The image, as publish.yml builds it: from dist/'s Linux archives, pushed by
# digest to a registry on localhost, by a builder that shares the host's network.
docker run -d --name touchmark-e2e-registry -p 127.0.0.1:5000:5000 registry:3.1.2
docker buildx create --name touchmark-e2e-builder --driver docker-container --driver-opt network=host --bootstrap
BUILDX_BUILDER=touchmark-e2e-builder bash scripts/release/build-image.sh dist localhost:5000/touchmark v0.0.1-ci
# digest=sha256:<digest>
bash scripts/release/check-image.sh localhost:5000/touchmark@sha256:<digest> v0.0.1-ci
bash scripts/release/image-e2e.sh localhost:5000/touchmark@sha256:<digest>   # a live Gitea, a few minutes

docker rm -f touchmark-e2e-registry && docker buildx rm touchmark-e2e-builder
```

The scripts of the release and of the Action have tests of their own: `scripts/release/tools_test.sh` (`check-version.sh`, `build-image.sh` with a fake docker, the version markers against `extra-files`) and `scripts/action/run_test.sh` (the Action's step with a fake docker and gh: the inputs, the resolve, the attestation, the label, the environment). The CI job `release-snapshot` also runs the Action from a copy that names the local image, with a stand-in `gh`, since a snapshot has no attestation.

## Verify a release

The signatures are keyless, made with cosign 3 (verify with cosign 3 too: it stores and finds signatures as Sigstore bundles), and the attestations are GitHub's build provenance. Both name the workflow that made them and the ref it ran on:

```
identity  https://github.com/bedrock-python/touchmark/.github/workflows/publish.yml@refs/heads/master
issuer    https://token.actions.githubusercontent.com
```

**The archives.** Download what you need with `checksums.txt` and `checksums.txt.sigstore.json` from the release, then:

```sh
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity https://github.com/bedrock-python/touchmark/.github/workflows/publish.yml@refs/heads/master \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
sha256sum --check --ignore-missing checksums.txt
gh attestation verify touchmark_X.Y.Z_linux_amd64.tar.gz --repo bedrock-python/touchmark \
  --signer-workflow bedrock-python/touchmark/.github/workflows/publish.yml --source-ref refs/heads/master
```

**The image.** Take the digest from `docker buildx imagetools inspect ghcr.io/bedrock-python/touchmark:X.Y.Z --format '{{ .Manifest.Digest }}'`:

```sh
cosign verify ghcr.io/bedrock-python/touchmark@sha256:<digest> \
  --certificate-identity https://github.com/bedrock-python/touchmark/.github/workflows/publish.yml@refs/heads/master \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/bedrock-python/touchmark@sha256:<digest> --repo bedrock-python/touchmark \
  --signer-workflow bedrock-python/touchmark/.github/workflows/publish.yml --source-ref refs/heads/master
docker buildx imagetools inspect ghcr.io/bedrock-python/touchmark@sha256:<digest> --format '{{ json .SBOM }}'
```

Add `--bundle-from-oci` to the `gh attestation verify` of the image for the check the Action makes, from the attestation stored in GHCR.

## Adding PyPI later

The org's libraries publish to PyPI; touchmark does not yet. To let Python projects run `uvx touchmark`, add to `publish.yml`, after `assets`:

1. a job that downloads the release's archives, checks them against the signed `checksums.txt` (as the `image` job does), and wraps each binary into a wheel with [go-to-wheel](https://pypi.org/project/go-to-wheel/), pinned by hash: 8 platform tags (manylinux and musllinux for amd64 and arm64, macOS, Windows), each wheel's binary byte for byte the archive's;
2. a job `pypi` in an environment `pypi` with `id-token: write`, which uploads the wheels with `pypa/gh-action-pypi-publish` (trusted publishing; `skip-existing` so that a re-run completes a failed upload), and, if the wheels should be on the GitHub release too, uploads them there with their own build provenance attestation.

On pypi.org, add a pending trusted publisher: project `touchmark`, owner `bedrock-python`, repository `touchmark`, workflow `publish.yml`, environment `pypi`; and add the badge and the install line to the README.
