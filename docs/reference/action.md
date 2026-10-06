# The GitHub Action

`bedrock-python/touchmark` runs touchmark on GitHub Actions, from the image of the
Action's own release.

```yaml
- uses: bedrock-python/touchmark@<commit> # vX.Y.Z
  with:
    command: distribute
  env:
    GITHUB_TOKEN: ${{ github.token }}
    TOUCHMARK_KEY_EXPOSED: ${{ needs.probe.outputs.exposed }}
    TOUCHMARK_WRITE_APP_ID: ${{ vars.TOUCHMARK_WRITE_APP_ID }}
    TOUCHMARK_WRITE_APP_KEY: ${{ secrets.TOUCHMARK_WRITE_APP_KEY }}
```

Pin it by commit, with the version in a comment, and let Dependabot move it. Credentials
are never inputs: pass them in `env` under the names touchmark reads (see
[Environment variables](environment.md)).

## Inputs

Each input is one flag; there is no free-form argument.

<!-- generated: action-inputs -->

| Input | Default | Description |
|---|---|---|
| `command` | required | The command to run: check, plan, distribute, doctor or version. |
| `hub` | `.` | The hub checkout, relative to the workspace (--hub). Check it out with fetch-depth 0. |
| `strict` | `false` | plan, distribute, doctor: exit 3 on a blocked or deferred target, an incomplete resolve or a check that warns (--strict). true or false. |
| `all` | `false` | plan: in a hub pull request, plan every target, not only those of the packs it changes (--all). true or false. |
| `comment` | `false` | plan: keep the report in one comment of the hub pull request, through GITHUB_TOKEN (--comment). true or false. |
| `assume-opt-in` | `false` | plan: report as if every target without an opt-in file had opted in (--assume-opt-in); enabled: false still opts a target out. true or false. |
| `dry-run` | `false` | distribute: check everything with the write account and write nothing (--dry-run). true or false. |
| `only` | — | plan, distribute, doctor: only these targets, comma-separated [PROVIDER:]PATH (--only). |
| `deadline` | — | distribute: start no target after this duration, such as 50m (--deadline). Default: 5h30m on GitHub Actions. |
| `format` | — | The output format: text, json or markdown (--format; check takes text or json). |
| `report` | — | distribute, doctor: also write the JSON report to this file, relative to the workspace (--report). |

<!-- end generated -->

## What it runs

The Action is a composite action. Its step:

1. resolves `ghcr.io/bedrock-python/touchmark:<version>`, the version of the Action's
   release, to a digest;
2. checks that the digest has a build provenance attestation made by this repository's
   publish workflow (`gh attestation verify`, reading the attestation from the registry);
3. pulls the image by that digest, checks that it is labelled with the version, and runs
   it by that digest.

A version tag that names an image the publish workflow did not build fails the check;
nothing runs from a tag alone. An image that cannot be resolved, verified or pulled exits
2, like touchmark's own usage errors.

It runs the image with `docker run` as the runner's own user, not as a container action:
GitHub runs a container action as the image's user, which must be root to write the
workspace, and the image runs as a non-root user. The container gets the workspace, the
event payload (read-only) and the step summary, mounted at the same paths; the
environment variables `CI`, every `GITHUB_*` and `TOUCHMARK_*` of the step, and the
runner's proxy settings; nothing else: no Docker socket, no capabilities.

## Requirements

- A **Linux runner** with Docker, `docker buildx` and the GitHub CLI (`gh`), as GitHub's
  hosted Linux runners have. On other runners, install touchmark from the release
  archives and run it directly.
- **HTTPS access** to `ghcr.io`, and to the Sigstore trust roots `gh` fetches to check the
  image's attestation (`tuf-repo-cdn.sigstore.dev` and `tuf-repo.github.com`): a
  self-hosted runner behind an allowlist needs all three.
- A **full checkout** of the hub (`fetch-depth: 0`) in the workspace, at the `hub` input
  (default `.`).
- The job's `GITHUB_TOKEN` in the step's `env`, for the hub channel.

See [Run it in CI](../guide/ci.md) for the jobs of the template's workflow, and
[Releases](../project/release.md) for how the image and its attestations are made.
