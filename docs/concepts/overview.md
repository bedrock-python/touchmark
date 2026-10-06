# How it works

A hub is a git repository. Its `packs/` directory holds the files to deliver; everything
else configures the hub. touchmark reads the hub, decides for every target what to
change, and turns that decision into one pull request per target.

```text
your-org/engineering-assets
├── packs/
│   ├── agents/AGENTS.md           → AGENTS.md in every target that gets "agents"
│   └── claude/.claude/settings.json
├── hub.yml                        hub id, providers, writer, settings, pack metadata
├── targets.yml                    which repositories, which packs
└── .touchmark/operations.yml      one-off overrides, reviewed like everything else
```

## The life of a change

1. **A pull request to the hub changes a pack.** The hub's CI runs `touchmark check`,
   which validates the configuration, the packs and the history, and
   `touchmark plan`, which reads every affected target with the read account and
   reports what each would receive: pull requests to open, update and close, the
   sensitive paths, the API writes it would cost. Nothing is written.
2. **A maintainer merges it.** On the default branch, `touchmark distribute` runs with
   the write account. For every target that has opted in (or that the hub subscribes)
   and has a difference, it builds one commit on the sync branch `touchmark/<id>` and
   opens or updates one pull request. A second run on the same inputs writes nothing.
3. **A person in each target reviews it.** They merge it, or edit files (which makes
   them the repository's own), or close it, which touchmark remembers as a decline.

`distribute` runs again on every merge to the hub and on a daily schedule, and
`touchmark doctor` checks the write account weekly.

## One decision, two commands

`plan` and `distribute` run the same pipeline; `plan` stops before writing, with the
read account. `distribute --dry-run` runs it with the write account and also stops
before writing, and its report is the same as `plan`'s.

| Phase | What happens |
|---|---|
| guards | git version, configuration, `check`, the write isolation probe, the hub's fingerprint; the hub's HEAD must be the tip of its default branch, or the run is `superseded` and writes nothing |
| resolve | `targets.yml` becomes a list of repositories: web URLs matched to providers, organisations and groups listed, `topics` and `match` filtered, `exclude` patterns applied, duplicates merged by repository id |
| inspect | per target, in parallel: archived, empty or mirror repositories skipped; the opt-in file read (an empty one assumed where `targets.yml` subscribes the target; `enabled: false` skips it); the target's tree fetched without blobs or a checkout; the decision made |
| sweep | sync pull requests of this hub in repositories that are no longer targets are found, to be closed |
| gate | the guards against mass actions: at most `limits.max_new_prs_per_run` new pull requests, and no mass close |
| execute | `distribute` only: closes, then updates, then new pull requests, under each provider's write budget |
| report | text, JSON or Markdown; in CI also the report files, the step summary and annotations; the exit code |

Each target ends with one **outcome**: `opened`, `updated`, `unchanged`, `closed`,
`declined`, `skipped`, `blocked`, `deferred` or `failed`, most with a reason. `deferred`
targets (a rate limit, the deadline, the rollout limit) are picked up by the next run:
touchmark keeps no state between runs and works everything out again from the platform.
See [Reports](../reference/output.md).

## What touchmark decides per file

For every path the selected packs have ever shipped, touchmark compares the target's
file with the hub's history. A file whose content is a version the hub shipped is the
hub's to update or delete; any other content is the repository's own and is left alone.
See [Ownership by provenance](ownership.md).

| State | The target file… | touchmark… |
|---|---|---|
| `missing` | does not exist | creates it |
| `current` | matches the current pack version | leaves it |
| `outdated` | matches an older pack version | updates it |
| `local` | matches no version the hub ever shipped | leaves it: it is the repository's own |
| `ignored` | is listed under `ignore` | leaves it |
| `retired` | is no longer shipped, and its content came from the hub | deletes it |
| `retired-local` | is no longer shipped, and its content is local | leaves it |
| `unsafe` | is a symlink, is not a regular file, or points outside the repository | leaves it |
| `orphaned` | was shipped only by a pack this repository no longer gets | leaves it, and lists it |

## Where it runs

| Command | Where | Account |
|---|---|---|
| `check` | every hub pull request | none |
| `plan` | every hub pull request | reader |
| `probe` | jobs any hub branch can start | none: it fails if it sees the write key |
| `distribute` | the hub's default branch: merges, a daily schedule | writer |
| `doctor` | the hub's default branch: a weekly schedule | writer |
| `setup` | a maintainer's machine, once | a maintainer's token |
| `status`, `apply` | a checkout of a target | none |

The [hub template](https://github.com/bedrock-python/engineering-assets-template) wires
all of this for GitHub Actions, GitLab CI, Gitea and Forgejo Actions. touchmark never
checks a target out in CI, and never runs anything from the hub or the targets but git.
