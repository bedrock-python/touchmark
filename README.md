# touchmark

Keep shared files in sync across many repositories through pull requests, without overwriting what a team has made its own.

[![Release](https://img.shields.io/github/v/release/bedrock-python/touchmark?color=blue)](https://github.com/bedrock-python/touchmark/releases)
[![Go](https://img.shields.io/github/go-mod/go-version/bedrock-python/touchmark)](go.mod)
[![License](https://img.shields.io/github/license/bedrock-python/touchmark)](LICENSE)
[![CI](https://github.com/bedrock-python/touchmark/actions/workflows/ci.yml/badge.svg?branch=master)](https://github.com/bedrock-python/touchmark/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/bedrock-python/touchmark/graph/badge.svg)](https://codecov.io/gh/bedrock-python/touchmark)
[![Docs](https://img.shields.io/badge/docs-online-blue)](https://bedrock-python.github.io/touchmark/)

touchmark is the engine behind **engineering-assets hubs**. A hub is a repository your
organisation creates from [the hub template](https://github.com/bedrock-python/engineering-assets-template):
it holds **packs** of shared files (agent instructions, review prompts, merge request
templates, shared CI files) and a list of **targets**. Whenever a pack changes, touchmark
opens a pull request (a merge request on GitLab) in every target that has opted in, on
GitHub, GitLab, Gitea and Forgejo. It manages a file only while it can prove the hub
shipped that exact content, so a line a team changes stays changed.

Three ways in, one version number:

<!-- x-release-please-start-version -->

| | | |
|---|---|---|
| **GitHub Action** | `uses: bedrock-python/touchmark@<commit> # v0.2.0` | The hub's workflows on GitHub. Runs the release's image by digest, after checking its build provenance. [The Action](https://bedrock-python.github.io/touchmark/reference/action/) |
| **Container image** | `ghcr.io/bedrock-python/touchmark:0.2.0` | GitLab CI, Gitea and Forgejo Actions, `docker run`: touchmark and git on Alpine, as a non-root user. [Run it in CI](https://bedrock-python.github.io/touchmark/guide/ci/) |
| **Binary** | [Releases](https://github.com/bedrock-python/touchmark/releases) | Linux, macOS and Windows on amd64 and arm64, one static binary; or `go install github.com/bedrock-python/touchmark/cmd/touchmark@latest` with Go 1.26 or newer. [Installation](https://bedrock-python.github.io/touchmark/getting-started/installation/) |

<!-- x-release-please-end -->

> [!TIP]
> **Setting up a hub with an AI assistant?** Hand it
> **[one page](https://bedrock-python.github.io/touchmark/agents/)** instead of the
> whole site: every command and configuration file, the rules that break a hub when they
> are broken, the mistakes models make, and a map of which page to fetch for the rest.
> Every docs page is also served as raw Markdown at its own URL, and a **Copy page** button
> at the top of each one hands it straight to a chat window.

## Features

- **Ownership from history** — a file is managed only while its content is, byte for byte,
  a version the hub has shipped at that path. Change one line and the file is the
  repository's own: touchmark never overwrites or deletes it again
- **No state in your repositories** — no lock files, markers or generated headers. Ownership
  comes from the hub's git history, the memory of declined changes from the pull requests
- **Review, never merge** — every change arrives as a pull request a person merges; there
  is no auto-merge option
- **Opt-in** — a repository receives nothing until it adds `.engineering-assets.yml`, which
  can add packs and `ignore` paths; a hub that owns the decision subscribes repositories
  itself (`opt_in: assumed`), and `enabled: false` in the file still opts one out
- **Targets as you name them** — a repository, an organisation or a group by path or by
  web URL, narrowed by topics and path globs (`acme/svc-*`), minus `exclude` globs
  (`corp:platform/legacy/**`)
- **Four platforms, one hub** — GitHub (github.com, GHE.com, Enterprise Server), GitLab
  (gitlab.com and self-managed), Gitea and Forgejo; one hub can deliver to several at once
- **Declines remembered** — content in a pull request closed without merging is not
  proposed again; reopen it, or tick a box in its description, to change your mind
- **A plan before delivery** — `plan` in a hub pull request reports what every target would
  receive and how many API writes it costs, and changes nothing
- **Separate read and write accounts** — the writer delivers from the hub's default branch
  only; `setup` arranges that on GitHub and GitLab, `probe` and `doctor` check it
- **Runs nothing from your repositories** — git with an argument list and no shell; targets
  are never checked out, so their hooks, filters, LFS and submodules never run
- **Checkout line endings do not matter** — a file's identity is its committed git blob id,
  so a Windows checkout with `core.autocrlf=true` behaves like a Linux one
- **Built for CI** — text, JSON and Markdown reports, step summaries and annotations, one
  comment on the hub pull request kept up to date (GitHub, Gitea, Forgejo), exit codes a
  pipeline can read
- **Signed and attested releases** — cosign signatures, SBOMs and GitHub build provenance
  for the binaries and the image

## Installation

<!-- x-release-please-start-version -->
```bash
# The binary for your platform, from the release
v=0.2.0
curl -fsSLO "https://github.com/bedrock-python/touchmark/releases/download/v$v/touchmark_${v}_linux_amd64.tar.gz"
tar -xzf "touchmark_${v}_linux_amd64.tar.gz" touchmark

# With Go
go install github.com/bedrock-python/touchmark/cmd/touchmark@latest

# The image
docker pull ghcr.io/bedrock-python/touchmark:0.2.0
```
<!-- x-release-please-end -->

**Requirements:** git 2.31+ for `check`, `status`, `apply` and `manifest`, git 2.45+ for
`plan` and `distribute`, which read the targets (the image brings its own); Go 1.26+ to
build from source. To check an archive or the image before you
run it, see [Verifying what you run](SECURITY.md#verifying-what-you-run).

## Quick start

Most people never run touchmark by hand: they create a hub from
[the template](https://github.com/bedrock-python/engineering-assets-template), and the hub's
CI runs it. Getting started walks through that on [GitHub](https://bedrock-python.github.io/touchmark/getting-started/github/),
[GitLab](https://bedrock-python.github.io/touchmark/getting-started/gitlab/) and
[Gitea or Forgejo](https://bedrock-python.github.io/touchmark/getting-started/gitea-forgejo/).

To see what it does, make a hub with one pack and one target:

```bash
mkdir -p hub/packs/agents && cd hub && git init -q
cat > hub.yml <<'EOF'
version: 1
id: acme-eng
EOF
cat > targets.yml <<'EOF'
version: 1
targets:
  - repo: acme/billing
    packs: [agents]
EOF
cat > packs/agents/AGENTS.md <<'EOF'
# Agent instructions

Run `make check` before you commit. Never commit secrets or generated files.
EOF
git add -A && git commit -qm "feat: the agents pack"
touchmark check --hub .
```

Then a target that opts in, and the pack applied to it:

```bash
cd .. && git init -q billing && cd billing
touch .engineering-assets.yml          # the repository's consent
touchmark apply --hub ../hub --repo acme/billing
```

```text
touchmark apply · hub acme-eng @ afd206a · target acme/billing
packs: agents (targets.yml)

  done  create  missing  AGENTS.md  agents

missing 1
applied 1 of 1 change
```

Change the file, and it is no longer the hub's:

```bash
echo "Ask before you add a dependency." >> AGENTS.md
touchmark status --hub ../hub --repo acme/billing
```

```text
touchmark status · hub acme-eng @ afd206a · target acme/billing
packs: agents (targets.yml)

  keep  local  AGENTS.md  agents  run touchmark apply --adopt AGENTS.md to take it back, or add it to ignore

local 1
in sync
```

In the hub's CI, `plan` and `distribute` make the same decisions for every target through
the platform's API, and each change arrives as a pull request. On GitHub the hub's workflow
runs the Action; its plan step on a hub pull request:

<!-- x-release-please-start-version -->
```yaml
- uses: bedrock-python/touchmark@<commit> # v0.2.0
  with:
    command: plan
    strict: true
    comment: true
  env:
    GITHUB_TOKEN: ${{ github.token }}
    TOUCHMARK_READ_APP_ID: ${{ vars.TOUCHMARK_READ_APP_ID }}
    TOUCHMARK_READ_APP_KEY: ${{ secrets.TOUCHMARK_READ_APP_KEY }}
```
<!-- x-release-please-end -->

## How it decides

For every path the selected packs have ever shipped, touchmark compares the target's file
with the hub's history:

| State | The target's file… | touchmark… |
|---|---|---|
| `missing` | does not exist | creates it |
| `current` | matches the current pack version | leaves it |
| `outdated` | matches an older pack version | updates it |
| `local` | matches no version the hub ever shipped | leaves it: it is the repository's own |
| `ignored` | is listed under `ignore` | leaves it |
| `retired` | is no longer shipped, and its content came from the hub | deletes it |
| `retired-local` | is no longer shipped, and its content is local | leaves it |
| `unsafe` | is a symlink, not a regular file, or points outside the repository | leaves it |
| `orphaned` | was shipped only by a pack this repository no longer gets | leaves it, and lists it |

Content under 64 bytes never counts as evidence, so an empty `.gitkeep` or a `{}` a team
wrote is never mistaken for a shipped file. The hub's full history is the ownership record:
touchmark refuses a shallow or partial clone of it.
[Ownership by provenance](https://bedrock-python.github.io/touchmark/concepts/ownership/) has
the rest.

## Commands

| Where | Command | What it does |
|---|---|---|
| The hub | `check` | validates `hub.yml`, `targets.yml` and every pack |
| | `plan` | reports what `distribute` would do in every target; changes nothing |
| | `distribute` | opens, updates and closes the sync pull request of every target |
| | `doctor` | checks the write account against every target, and with `--hub-token` where the hub keeps its write key |
| | `probe` | fails when the job can see the write key |
| | `setup github\|gitlab` | sets up the hub's platform: the reader and the writer, the write key kept to the default branch |
| | `migrate` | prints a hub's configuration for one that replaces a multi-gitter setup on GitLab |
| | `manifest` | prints the ownership manifest built from the hub's history |
| A target checkout | `status` | lists every managed path and its state; changes nothing |
| | `apply` | applies the packs to the working tree (`--dry-run`, `--adopt GLOB`) |
| Anywhere | `schema NAME` | prints the JSON Schema of a configuration file or a report |
| | `version` | prints the version |

Exit codes: `0` success, `1` an action failed, `2` a usage, configuration or safety error with
nothing written, `3` with `--strict` a blocked or deferred target, an incomplete resolve or a
check that warns (and from `setup`, a step left to you). [Commands](https://bedrock-python.github.io/touchmark/reference/commands/) has
every flag.

## Configuration

| File | Lives in | What it holds |
|---|---|---|
| `hub.yml` | the hub | the hub's id, the platforms it delivers to with their writers, commit and pull request settings, security settings, pack descriptions, `requires` and `formerly` |
| `targets.yml` | the hub | which repositories get which packs: by repository (its path or web URL), organisation or group, filtered by topic and path pattern, minus `exclude` patterns; and whether the hub subscribes them without an opt-in file (`opt_in: assumed`) |
| `.touchmark/operations.yml` | the hub | one-off overrides of a safeguard, merged through review: rebuild a paused branch, propose declined content again, allow a mass close |
| `.engineering-assets.yml` | each target | the repository's consent, or with `enabled: false` its refusal; packs to add and paths to `ignore` |

Credentials come from the environment, never from flags or files:
`TOUCHMARK_[<ID>_]READ_TOKEN` (or `…_READ_APP_ID` with `…_READ_APP_KEY` for a GitHub App),
the same for `WRITE`, and an optional `…_SIGNING_KEY`. `touchmark schema hub` (`targets`,
`opt-in`, `operations`) prints the JSON Schema an editor completes from. The
[Reference](https://bedrock-python.github.io/touchmark/reference/hub/) lists every key and
[Environment variables](https://bedrock-python.github.io/touchmark/reference/environment/)
every name.

## Security model

A hub can open pull requests in every repository it targets, and a sync pull request runs
the target's CI before anyone reviews it. touchmark keeps that boundary, and fails closed
where it is missing:

- **Three accounts** — a reader plans on hub pull requests; a writer, a different account,
  delivers from the hub's default branch only; the CI's own token reaches only the hub. The
  writer has no access to the hub, so a leaked write key cannot change the packs
- **The write key stays on the default branch** — a probe in the jobs any hub branch can
  start fails if it sees the key, and `distribute` refuses to run unless the platform keeps
  the key to the default branch or `hub.yml` accepts the risk with a reason
- **Overrides go through review** — rebuilding a branch someone pushed to, proposing
  declined content again and mass closes come only from `.touchmark/operations.yml` on the
  default branch
- **It touches only its own pull requests** — in the target itself, by the writer or a known
  author, with the hub's fingerprint in the marker; a branch someone else's pull request
  uses is never moved
- **Tokens stay out of sight** — never in a URL, an argument or `.git/config`, masked in
  every output; a secret found in a pull request body, comment, commit or branch name stops
  the operation

[Security model](https://bedrock-python.github.io/touchmark/concepts/security/) explains each
control, and the [threat model](https://bedrock-python.github.io/touchmark/project/threat-model/)
maps every threat to its control, its test and the risk that remains. Report a vulnerability
privately: see [SECURITY.md](SECURITY.md).

## Documentation

[bedrock-python.github.io/touchmark](https://bedrock-python.github.io/touchmark/)

- [Getting started](https://bedrock-python.github.io/touchmark/getting-started/installation/) — install, a hub on GitHub, GitLab, Gitea or Forgejo, opting a repository in
- [Concepts](https://bedrock-python.github.io/touchmark/concepts/overview/) — how it works: ownership by provenance, packs and opt-in, delivery and the sync branch, the memory of declined pull requests, the security model
- [How-to guides](https://bedrock-python.github.io/touchmark/guide/setup/) — set up a hub's platform, write packs, run it in CI, deliver to several platforms, one-off operations, check the write account, use it on your machine, migrate from multi-gitter, troubleshooting, FAQ
- [Reference](https://bedrock-python.github.io/touchmark/reference/commands/) — every command and flag, every key of every configuration file, environment variables, exit codes, reports, the GitHub Action
- [Project](https://bedrock-python.github.io/touchmark/project/threat-model/) — the threat model, how releases are built and verified, the end-to-end tests against live platforms
- [For AI agents](https://bedrock-python.github.io/touchmark/agents/) — every command, the
  configuration, the rules that break a hub when broken and a map of the rest, on one page
  to hand to a coding assistant

## Development

```bash
make install          # go mod download + uv sync --group docs
make check            # gofmt + go vet + golangci-lint + govulncheck
make test-unit        # go test ./..., no Docker required
make test             # all tests with the race detector and the coverage gate
make test-e2e         # the live tests against Gitea, in Docker
make build            # bin/touchmark
make snapshot         # a release build in dist/ with goreleaser; publishes nothing
make docs-serve       # local docs preview
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache 2.0 — see [LICENSE](LICENSE).
