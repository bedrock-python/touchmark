# Troubleshoot

Start from the report: every target has an outcome and a reason, and the run has
warnings. `--format json` or the `touchmark-report.json` artifact has them all; see
[Reports](../reference/output.md).

## The run stops before any target

These exit with code 2 and write nothing.

| Message | Cause | Fix |
|---|---|---|
| `the hub's fingerprint is unknown` | a run outside CI | pass `--hub-fp HOST/ID`, the hub's host and repository id |
| `hub.yml declares no providers, no CI environment names one…` | a local run of a hub with the single-provider shorthand on a self-managed instance | set `platform` and `base_url` in `hub.yml`, or list the instance under `providers` |
| `git X is too old: plan needs git 2.45.0 or newer` (or `distribute`) | the runner's or your git | use the image, or a newer git |
| `hub checkout is shallow: full history is required` | `actions/checkout` without `fetch-depth: 0`, or `GIT_DEPTH` not `"0"` | fetch the whole history |
| `hub checkout is a partial clone` | a clone with `--filter` | clone without it |
| `id: still the template placeholder "change-me"` | a new hub | set `id` in `hub.yml` |
| `targets.yml: targets[N].repo: <url> is not under the url of any provider` | a target written as a web URL whose host or path no provider in `hub.yml` has | add the provider, fix its `url`, or write the target as `<provider>:<path>` |
| `… is under the url of providers …` | two providers share the instance of a URL | name one with `provider:` on the entry, or write the target as `<provider>:<path>` |
| `targets.yml: its web URLs are matched with the providers' urls, which are unknown` | a hub without `providers` that names targets by URL, run outside its own CI: an `origin` remote names the provider only on `github.com`, a `*.ghe.com` host or `gitlab.com` | list the instance under `providers` in `hub.yml`; on one of those hosts, adding the `origin` remote is enough |
| `… looks like a secret …; remove it from the file and revoke it` | a token pasted into a configuration file | remove it, and revoke the token: it is in the history |
| `TOUCHMARK_… is visible in this job, which any branch of the hub can run` | the probe saw a write key | keep write keys only in the protected environment or variable |
| a write key visible outside the environment, or the probe not configured | GitHub: `TOUCHMARK_KEY_EXPOSED` is `true`, or unset | remove the repository or organisation secret; pass the probe job's output |
| `--worktree: distribute ships only committed packs` | `distribute --worktree` | commit; `plan --worktree` previews |
| an operation flag in CI | `--recreate`, `--forget-declines`, `--allow-mass-close`, `--adopt-unmarked`, `--allow-stale` with `CI` set | use [`.touchmark/operations.yml`](operations.md) |
| the run is not on the default branch | `distribute` on a branch or a pull request event | let it run on the default branch only |
| the writer does not match | the write key belongs to another account than `hub.yml`'s `writer` | fix `writer`, or the key |

A run whose hub commit is no longer the tip of the default branch ends `superseded`, exit
0: a newer run will deliver the newer packs.

## A target is blocked

| Reason | What happened | Fix |
|---|---|---|
| `edited` | someone pushed to the sync branch, merged another branch into it, or rebased it onto other commits | tick *Rebuild this branch* in the pull request (their commits are dropped), or add a `recreate` entry with the branch's head |
| `branch-taken` | the sync branch exists and is not this hub's: another hub with the same `id`, or a branch someone created | rename one hub's `branch`, or delete the foreign branch |
| `branch-in-use` | someone else's open pull request uses the sync branch | close or move that pull request |
| `opt-in-invalid` | the opt-in file is not valid, or names a pack the hub does not have | fix `.engineering-assets.yml` in the target |
| `marker-invalid` | touchmark's pull request has a marker it cannot read, or none | add a `recreate` entry: touchmark takes the pull request back and writes a new marker |
| `rules:<rule>` | a branch rule of the target blocks the push, such as a protected `touchmark/*` pattern | exempt the sync branch, or the writer |
| `permission:<what>` | the writer lacks a permission, such as `permission:workflows` for a change under `.github/workflows/` | grant it to the writer App or account |
| `cannot-sign` | the target requires signed commits and touchmark cannot sign there | set `TOUCHMARK_<ID>_SIGNING_KEY` for the writer; on GitHub, use Apps |
| `archived` | the target is archived and has touchmark's open pull request | unarchive it, or leave it |
| `mass-close` | the run would close more than max(5, `max_close_fraction` × open pull requests) | check `targets.yml`; if intended, add `allow_mass_close` |

`plan` runs with the reader, which cannot see what the writer may bypass: `plan` can
predict a rule block that `distribute` will not hit. `doctor` checks with the writer.
`distribute --dry-run` and `distribute` read, with the writer, the protected branches
that keep it from pushing to the sync branch (GitLab's protected branches; on Gitea and
Forgejo, the protection of a sync branch that exists already) and block the target
before any write; other rules show at the first push, which they refuse before anything
is written.

## A target is deferred or failed

`deferred` targets need nothing: the next run continues them.

| Reason | Meaning |
|---|---|
| `rate-limit` | the provider rate-limited the run three times, or for longer than the target's time |
| `deadline` | `--deadline` passed before the target started |
| `rollout-limit` | `limits.max_new_prs_per_run` new pull requests were opened already |
| `provider-down` | three authentication failures in a row, or a GitLab IP ban breaker |
| `interrupted` | the run was cancelled (SIGINT, SIGTERM) |
| `cooldown` | a bot closed the pull request; it comes back after `memory.auto_close_cooldown` |

| Failed reason | Look at |
|---|---|
| `auth` | the token or App key: expired, revoked, wrong provider |
| `access` | the writer's role on the target, or the App's installation |
| `transient` | a 5xx or a network timeout after three tries; the next run retries |
| `git` | a fetch or push that failed; an object larger than touchmark reads (a 1 MiB commit, an 8 MiB blob) |
| `race` | the target changed while touchmark wrote; the next run decides again |
| `integrity` | the commit touchmark built did not match its decision; please report it |
| `secret-exposure` | a secret was about to be written into a pull request, comment, commit or branch name; nothing was written |
| `internal` | a bug; please report it with the report file |

## Nothing happens to a repository

- It has no `.engineering-assets.yml` at its root (`skipped:not-opted-in`), and no entry
  that selects it has `opt_in: assumed`.
- Its `.engineering-assets.yml` says `enabled: false` (`skipped:opted-out`).
- An `exclude` entry covers it: a pattern such as `acme/*` or `platform/**` covers more
  than the repositories it names. `check` warns when one covers a `repo:` entry.
- It is archived, disabled, empty, a mirror, pending deletion, has pull requests turned
  off, or uses SHA-256 objects (`skipped` with that reason).
- The hub is public and the repository is not (`private-in-public-hub`, only counted).
- An `org` or `group` entry did not select it: `topics` must all match, its full path must
  match one of the `match` patterns (`acme/svc-*` leaves out `acme/sub/svc-a`), forks are
  left out unless `forks: true`, nested groups need `subgroups` (on by default).
- It declined this content before (`declined`): see [memory](../concepts/memory.md).
- It already has everything (`unchanged`).

## A file did not change in a repository

It is `local` (the repository changed it), `ignored`, `unsafe`, or `orphaned` (its pack is
no longer assigned). `touchmark status` in a checkout shows which. To take a `local` file
back: `touchmark apply --adopt <path>`. A file committed with CRLF line endings is `local`
against a pack's LF version even when the text is the same: see
[Identity is git's](../concepts/ownership.md#identity-is-gits).

## Reporting a bug

Open an issue at
[github.com/bedrock-python/touchmark/issues](https://github.com/bedrock-python/touchmark/issues)
with `touchmark version`, the platform and its version, and the report with private names
removed. Report a vulnerability privately: see
[SECURITY.md](https://github.com/bedrock-python/touchmark/blob/master/SECURITY.md).
