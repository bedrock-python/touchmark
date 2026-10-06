# Contributing to touchmark

Thank you for your interest in contributing! This document covers everything you need to get started.

touchmark can write to every repository a hub targets, so changes are reviewed for safety
first: what a change lets a target, a hub branch or a log reader do matters as much as what
it adds. If you are unsure whether an idea fits, open an issue before the pull request.

## Development setup

You need Go 1.25 or newer (the `go` line of `go.mod`), git, [uv](https://docs.astral.sh/uv/)
for the documentation site, and Docker for the image and the live tests.

```bash
git clone https://github.com/bedrock-python/touchmark.git
cd touchmark
make install
uv tool install pre-commit
pre-commit install --hook-type pre-commit --hook-type commit-msg
```

`distribute` needs git 2.45 or newer. On an older git the tests that run it skip; the suite
in Docker below runs them with a recent one.

## Running checks

```bash
make fmt              # gofmt -w
make check            # gofmt, go vet (with and without -tags e2e), golangci-lint, govulncheck
make test-unit        # go test ./..., no Docker required
make test             # all tests with the race detector and the coverage gate
make test-e2e         # the live tests against Gitea, requires Docker
make build            # bin/touchmark
make snapshot         # the whole release into dist/ with goreleaser in Docker; publishes nothing
make docs-serve       # local docs preview
make docs-build       # the generated blocks of the docs, then the site into site/ with the Markdown copy of every page
make clean
```

`make check` runs golangci-lint and govulncheck at the versions the Makefile pins, the ones
CI runs: a golangci-lint of that version on `PATH` as it is, otherwise `go run`
builds it once. `make test` holds the coverage of the whole suite to the gate in the Makefile
(`COVERAGE_MIN`), which CI measures on Linux with a recent git.

The tests of a few heavy cases run on Linux only; set `TOUCHMARK_HEAVY_TESTS=1` to run them
on Windows or macOS. With git older than 2.45, the tests of `distribute` skip, and coverage
comes out below the gate. To run the whole suite with a recent git from any OS:

```bash
docker run --rm -v "$PWD:/src" -w /src golang:1.26 \
  sh -c 'git config --global --add safe.directory /src && go test -race -timeout 45m ./...'
```

In Git Bash on Windows, prefix it with `MSYS_NO_PATHCONV=1` and use `$(pwd -W)` for `$PWD`.

## Code style

- **gofmt** on every file — `make check` and pre-commit enforce it
- **golangci-lint** with the linters of `.golangci.yml` on top of `go vet`
- **Doc comments** on every exported package-level name: a full sentence that starts with
  the name. Methods that only satisfy an interface (`Error`, `String`) need none
- **No comments** unless the *why* is non-obvious (a workaround, a subtle invariant, a
  security reason); when there is one, a full sentence
- **Standard library first** — the module depends on a YAML parser and a JSON Schema
  validator and nothing else: touchmark holds write keys to many repositories, and every
  dependency is part of that trust. A new dependency needs an issue first. Tools
  (golangci-lint, govulncheck, goreleaser, actionlint) run pinned from their images or with
  `go run tool@version`, never from `go.mod`
- **git runs with an argument list**, never through a shell, and never with a credential on
  its command line
- **Deterministic output** — reports, schemas and messages sort what they list; tests pass
  on Linux, macOS and Windows
- **Messages point at the site** — an error, a flag's help, a schema description or a
  generated file links a page of the documentation site (`internal/docsurl`, whose test
  checks the page and its heading exist), never a design document or a milestone;
  `TestNoInternalReferences` in `scripts/docs` checks every string literal, the help, the
  schemas and the pages
- **Pinned actions** — the workflows shared with the other bedrock-python repositories
  (`docs.yml`, `release-please.yml`) stay byte-identical to theirs, tag pins included;
  touchmark's own workflows pin every action by commit with the version in a comment.
  Dependabot moves both

## Commit messages

[Conventional Commits](https://www.conventionalcommits.org/) are enforced by pre-commit:

| Prefix | Use for |
|--------|---------|
| `feat:` | New feature or behaviour |
| `fix:` | Bug fix |
| `docs:` | Documentation only |
| `test:` | Test additions or changes |
| `refactor:` | Code restructure, no behaviour change |
| `perf:` | Performance improvement |
| `chore:` | Build, tooling, CI |

Breaking changes: add `!` after the type (`feat!:`) or include a `BREAKING CHANGE:` footer.

`CHANGELOG.md` is written from these by release-please: `feat`, `fix`, `perf` and `revert`
get a section of their own, the other types are left out.

## Pull requests

1. Fork the repository
2. Create a branch from `master`: `git checkout -b feat/my-feature`
3. Make your changes with tests
4. Run `make check && make test-unit` locally
5. Open a PR against `master`

Pull requests are squash-merged, so the title becomes the commit on `master` and the line
in the changelog: write it as a Conventional Commit.

## Live platforms

Gitea, Forgejo and GitLab run in Docker through `scripts/e2e/`, against the real
platforms' APIs and a real git:

```bash
make test-e2e                      # one Gitea
bash scripts/e2e/gitea.sh all      # every supported Gitea and Forgejo, about 35 minutes
bash scripts/e2e/gitlab.sh all     # every supported GitLab with a runner; about 6 GB of memory
```

CI runs the Gitea and Forgejo tests on every pull request; a maintainer adds the label
`e2e-gitlab` to run the GitLab ones. A change to the image, the Action or what the hub
template's CI files rely on also gets a run of the template's pipelines (`--template`).
GitHub's tests run against a fake of its API; the live ones need a sandbox organisation.
[End-to-end tests](https://bedrock-python.github.io/touchmark/project/e2e/) has the
details.

## The agents page

`docs/agents.md` is the whole tool on one page, written for a coding assistant: the
commands and their flags, the configuration files, the rules that break a hub when they
are broken, the mistakes models make, and a map of which page to fetch for the rest.
People hand it to an assistant instead of the site, which is what makes a stale one worse
than none — it teaches a model a command line that no longer exists.

It is part of the public interface, so it changes in the same pull request the interface
does: a command, flag, configuration key, environment variable or exit code added, renamed
or removed, a changed default, a new rule a hub has to obey. A new docs page means a new
row in the documentation map. The review check is mechanical — if the diff changes the
public surface and `docs/agents.md` is untouched, the pull request is not finished.

The page carries its own weight only if it stays fetchable as text. Every page of the site
is written a second time as raw Markdown next to its HTML by `scripts/emit_markdown.py`,
which the Docs workflow runs after the build; the **Copy page** control above each page
reads those files. A page whose Markdown would not read as the page declines both with
`copy_page: false` in its front matter.

## Releasing (maintainers only)

Releases are cut by [release-please](https://github.com/googleapis/release-please) from the
Conventional Commits on `master`: it keeps a release pull request open with the next
version, the generated changelog section and the version the Action runs (`action.yml`);
merging that PR tags the release `vX.Y.Z` and creates the GitHub release, and the publish
workflow takes it from there: goreleaser builds the binaries for Linux, macOS and Windows on
amd64 and arm64 and uploads them to that release with their checksums, signature and SBOMs;
the image is built for both architectures, pushed to GHCR by digest with its SBOM and build
provenance, tagged `X.Y.Z`, `X.Y` and `latest`, and signed with cosign.
[Releases](https://bedrock-python.github.io/touchmark/project/release/) has every step and
how to verify a release.

The release pull request is opened with the workflow's own token, which starts no workflow,
so the required check never reports on it: close the pull request and reopen it to run CI.

The first push creates the `ghcr.io/bedrock-python/touchmark` package private; make it
public in the organisation's package settings once, or every `docker pull`, every GitLab
and Gitea job and the Action are denied.

- `feat:` bumps the minor version, `fix:` the patch version, `feat!:` / a `BREAKING CHANGE:`
  footer bumps the major version (before 1.0 a breaking change bumps the minor version —
  `bump-minor-pre-major` is on).
- To force a specific version, add a `Release-As: x.y.z` footer to a commit — this is how
  1.0.0 is cut.
- Tags are `vX.Y.Z`, without the component prefix the other bedrock-python repositories
  use: Go modules and the Action resolve bare `v` tags only.
- release-please writes each release above the first version heading of `CHANGELOG.md`.
  Before the first release there is none, so the first release pull request moves the file's
  header below the 0.1.0 section; move it back to the top in that pull request just before
  merging it. Later releases leave it in place.
