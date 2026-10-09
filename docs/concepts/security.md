# Security model

A hub can open pull requests in every repository it targets, and a merge into the hub
runs code in their CI: a sync pull request comes from a branch of the target repository
itself, so the target's workflows run on it with the target's secrets before anyone
reviews it. Treat the hub as the root of your supply chain.

Two things hold the line: **the review of the hub** and **the isolation of its write
account**. Everything else (sensitive paths, warnings, guards) helps the reviewer or
catches a misconfiguration; it is not a boundary. touchmark keeps the boundary, and fails
closed where it is missing.

## Three accounts

| Account | Who | Where | Reaches |
|---|---|---|---|
| reader | an account or App with read access only | `plan`, on hub pull requests | the targets |
| writer | a different account or App, with write access | `distribute` and `doctor`, from the hub's default branch only | the targets, never the hub |
| hub channel | the CI's own token: `GITHUB_TOKEN`, `CI_JOB_TOKEN`, Gitea's Actions token | the guards, the hub's visibility, `plan --comment` | the hub repository only |

- **Reader and writer are different accounts on every platform.** The read key reaches
  any branch of the hub; a GitLab role belongs to the user, not to the token; and a
  separate account has its own rate limits.
- **The writer has no access to the hub**, so a leaked write key cannot change the packs.
  `doctor` fails when the writer can push to the hub, and warns when it can see a
  private one.
- **`plan` holds no write credential**, and a test of the code's call graph keeps it from
  ever reaching one.

## The write key stays on the default branch

The write key must be visible only to jobs of the hub's default branch, which only
reviewed merges reach. `security.write_isolation` in `hub.yml` says how:

| Mode | The write key | touchmark checks |
|---|---|---|
| `platform` (default) | in the hub's CI: GitHub's environment `touchmark-distribute` limited to the default branch; GitLab's protected, masked, hidden variable scoped to that environment; on Bitbucket Premium a secured variable of the deployment `touchmark-distribute`, which only the default branch may deploy to | a probe in the jobs any branch can start; on GitHub also the environment's branch policy, through the hub channel; on Bitbucket the hub's statement in `security.reason`, since the API does not show the deployment permissions |
| `external` | released by an external secret store through the CI's OIDC token, bound to the default branch (GitHub `sub` = `repo:ORG/HUB:ref:refs/heads/main` or `…:environment:touchmark-distribute`; GitLab `project_path`, `ref`, `ref_type`, `ref_protected`) | the job's own context |
| `none` | anywhere; the risk is accepted | nothing; `security.reason` is required, and every report and `doctor` show it as a warning |

**The probe.** A guard only sees its own job, and an attacker who can push a branch writes
their own workflow instead of running yours. So the probe runs where an attacker works:

- **GitHub Actions:** a `probe` job without the environment evaluates
  `secrets.<write key> != ''` for every write secret, and `distribute` and `doctor`
  refuse to run unless it answered `false` (`TOUCHMARK_KEY_EXPOSED`). The `plan` job on
  pull requests runs the same expression. `touchmark check` fails when a workflow hands
  touchmark a write key the probe does not test.
- **GitLab CI:** `touchmark probe` runs in every merge request pipeline, in the
  environment with `action: prepare`, and fails if it sees the write token: that is what
  any branch would see.
- **Gitea and Forgejo Actions:** every branch sees every secret, so `platform` cannot
  hold and `distribute` refuses it there. Run the hub's CI on GitHub or GitLab, or use
  `external`, or `none` with every branch protected.
- **Bitbucket Pipelines:** `touchmark probe` runs in every pipeline, in a step without
  the deployment, and fails if it sees a write key: a repository or workspace variable
  reaches every branch. A deployment variable reaches any step that names the
  deployment, in any branch's `bitbucket-pipelines.yml`, unless the environment's
  deployment permissions (Premium) admit the default branch alone; Bitbucket's API does
  not show them, so under `platform` `distribute` and `doctor` refuse to run unless
  `security.reason` states them. Without Premium use `none` with a reason, or
  `external`. See [A hub on Bitbucket Cloud](../getting-started/bitbucket.md).

**The guards** of `distribute` (and of `doctor` in CI), each exit 2 with nothing written:
the run is not on the default branch, or is a pull request event; on GitLab the ref is
not protected or the job is not in the environment `touchmark-distribute`; on Bitbucket
the step has no `deployment: touchmark-distribute`, the default branch could not be read,
or `platform` comes without the statement in `security.reason`; the probe saw
a write key or did not run; an operation flag was given in CI; git is older than 2.45;
the write key belongs to another account than `hub.yml`'s `writer`. `plan` in CI exits 2
when it sees any write key. A run on a commit that is no longer the tip of the default
branch is `superseded` and writes nothing.

## Overrides go through review

Rebuilding a branch someone pushed to, proposing declined content again, mass closes and
taking over pull requests without a marker come only from `.touchmark/operations.yml` on
the default branch. Their command-line flags fail in CI. The workflows take no inputs.

## touchmark touches only its own pull requests

A pull request is touchmark's only if it is in the target repository itself, on a sync
branch, opened by the writer or a `known_authors` account, with the hub's fingerprint in
its marker. A fork that copies the marker never qualifies, and a branch someone else's
pull request uses is never moved or deleted. A branch someone pushed to is paused, never
silently rewritten.

## It runs nothing from the hub or the targets

touchmark runs only git, with an argument list and no shell. Targets are never checked
out: no hooks, filters, LFS or submodules. git runs with hooks off, fsmonitor off, only
the https protocol, no redirects, object checks on, and an environment reduced to an
allowlist (`PATH`, temporary directories, proxies, CA files).

## Tokens stay out of sight

- git gets a token only as an `Authorization` header in its environment, bound to the
  provider's URL: never in a URL, an argument or `.git/config`.
- The HTTP client sends a credential only to its own provider's host, and the hub channel
  only to the hub repository.
- touchmark removes every `TOUCHMARK_*` credential from its environment once read; no git
  it runs sees them, nor `GITHUB_TOKEN` or `CI_JOB_TOKEN`.
- Every token, App JWT and installation token is masked in all output, in its raw,
  Basic, URL-encoded and Bearer forms; on GitHub Actions each is registered with
  `::add-mask::` before first use.
- A secret found in a pull request body, comment, commit or branch name stops the
  operation (`failed:secret-exposure`), and `check` fails on a configuration line that
  looks like a token.

## Untrusted input is bounded

Everything read from a target is data. The opt-in file is at most 64 KiB with a strict
schema; the marker at most 16 KiB and 64 KiB unpacked; API responses at most 32 MiB; a
commit read from a target at most 1 MiB and a blob 8 MiB. Of what a target contains,
only the *Rebuild this branch* and *Propose this content again* tick boxes act.

## Public hubs

The CI logs of a public hub are public. touchmark reads the hub's visibility from the CI
and skips non-public targets there (`skipped:private-in-public-hub`), printing only how
many, in every output. A hub whose visibility the CI does not report counts as public.
`security.private_targets_in_public_hub: deliver` delivers to them anyway; their names
then appear in the logs.

## What remains

- **A merge into the hub is code in every target's CI.** Protect the default branch,
  require review, keep CODEOWNERS on `packs/`, `hub.yml`, `targets.yml`, `.touchmark/`
  and the CI files, and read the ⚠ section of every sync pull request.
- **The read key** can read every target, and anyone who can push a branch to the hub can
  use it. Keep hub write access tight.
- **The hub's maintainers hold the write account** on every platform: they can change the
  variables, the environments and the workflows. Choose them accordingly.
- **A leaked write key** can open pull requests in, push branches to and run the CI of
  every target until it is revoked. On GitHub, touchmark mints a token per target that
  lives for minutes.
- **GitLab:** a sync pipeline runs as the writer, and its job token carries the writer's
  access to other projects. Keep the CI/CD job token allowlist on in targets, don't add
  sync targets to each other's allowlists, and use a separate writer per group of targets
  you trust differently.
- **No auto-merge.** A person merges every pull request.
- **Releases are signed and attested.** Pin touchmark in your hub and upgrade it through
  review; see [Releases](../project/release.md#verify-a-release).

The [threat model](../project/threat-model.md) maps each threat to the control that stops
it, the test that shows it, and the risk that remains. Report a vulnerability privately:
see [SECURITY.md](https://github.com/bedrock-python/touchmark/blob/master/SECURITY.md).
