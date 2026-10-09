# touchmark for AI agents

> One page holding everything a coding assistant needs to set up, configure and drive
> touchmark correctly, plus a map of where the rest of the documentation keeps the
> details it leaves out. Give an agent this page rather than the whole site.

| | |
|---|---|
| Program | `touchmark`, one static Go binary; the same binary in the image `ghcr.io/bedrock-python/touchmark` and the GitHub Action `bedrock-python/touchmark` |
| Requires | git 2.31+ for `check`, `status`, `apply`, `manifest`; git 2.45+ for `plan` and `distribute`, which read the targets (the image brings its own) |
| Install | archives on [GitHub releases](https://github.com/bedrock-python/touchmark/releases) · `go install github.com/bedrock-python/touchmark/cmd/touchmark@latest` (Go 1.26+) · `docker run ghcr.io/bedrock-python/touchmark:<version>` |
| Platforms | GitHub (github.com, GHE.com, GHES 3.19+), GitLab 17.0+, Gitea 1.26+, Forgejo 15+; as providers a hub delivers to: Bitbucket Cloud, Azure DevOps Services |
| Hub template | <https://github.com/bedrock-python/engineering-assets-template> |
| Source | <https://github.com/bedrock-python/touchmark> |

## How to read this page

Every page of this site is also served as raw Markdown at its own URL with `.md` in
place of the trailing slash — this page is `/agents.md`, the ownership concept is
`/concepts/ownership.md` — so anything the map below points at can be fetched as plain
text rather than scraped out of HTML. The **Copy page** control at the top of a page does
the same thing for a human with a chat window open. The reference pages are Markdown
too: the help of every command and the fields of every configuration file there are
generated from the code, so they are the exact spelling.

Top to bottom before changing a hub. [Rules that hold or break the setup](#rules-that-hold-or-break-the-setup)
is the section correctness lives in — those are the things touchmark refuses at run time,
or worse, cannot see at all. Every command, flag, key and variable used below exists; if
you need something not listed here, fetch the page the
[documentation map](#documentation-map) points at, or run `touchmark <command> --help`
and `touchmark schema <name>`, rather than guessing a flag that sounds plausible.

## Scope

**It does** deliver the files in `packs/` of a hub repository to many target
repositories as pull requests (merge requests on GitLab), one sync pull request per
target, and keeps them current: opens, updates and closes those pull requests as the
packs change. It decides per file whether it may touch it, from the hub's git history
alone. It checks a hub (`check`), reports on every hub pull request what each target
would receive (`plan`), checks the write account (`doctor`), sets up the hub's platform
on GitHub and GitLab (`setup`), and applies packs to a local checkout (`status`, `apply`).

**It does not** merge anything, ever. It does not render templates or variables: a pack
file is copied byte for byte. It does not merge a team's edits into a pack's new version:
an edited file is the team's and is never touched again. It keeps no state in targets —
no lock file, no header, no marker in their files. It never checks a target out in CI,
and never runs a script, hook, filter or LFS from the hub or a target. It delivers
nothing to a repository without the opt-in file, unless `targets.yml` subscribes it
(`opt_in: assumed`). There is no pull mode (targets do not
fetch from the hub), no remote pack sources, and no `setup` for Bitbucket or Azure DevOps (a
hub on Bitbucket Cloud runs in Bitbucket Pipelines, one in Azure Repos in Azure Pipelines, both
set up by hand).

## Mental model

A **hub** is a repository made from the template. `packs/<pack>/` mirrors a target's
layout (`packs/agents/AGENTS.md` becomes `AGENTS.md`); everything else in the hub
configures it: `hub.yml`, `targets.yml`, `.touchmark/operations.yml`, the CI files.

* **Provenance.** touchmark reads every commit of the hub that touched `packs/` and
  builds a **manifest**: every version (blob id) each pack ever shipped at each path. A
  target file whose content equals one of those versions is the hub's; any other content
  is the repository's own. Identity is git's blob id: in CI the committed blob; in a
  checkout, the blob in the index for a file whose bytes on disk are that blob, and for
  any other file both its bytes on disk and what `git hash-object` makes of them after
  the target's filters. So CRLF checkouts of LF blobs behave like LF ones, and a file
  committed with CRLF is `local` against a pack's LF version. git keeps that CRLF on
  later commits even with `core.autocrlf=true`, so an edit to the pack's text that
  `status` calls `current` is `local` once committed; `apply --adopt` writes LF.
* **States** of a path in a target: `missing` (created), `current` (left), `outdated`
  (updated), `local` (left: the team's), `ignored` (left), `retired` (no longer shipped,
  hub content: deleted), `retired-local` (left), `unsafe` (symlink, not a regular file,
  outside the repository: left), `orphaned` (shipped only by a pack the target no longer
  gets: left and listed). Content under 64 bytes never counts as evidence.
* **Selection.** A target's packs, in order: `defaults.packs` of `targets.yml`, the
  `packs` of every entry that matches it, the `packs` of its opt-in file; duplicates
  dropped, each pack's `requires` placed before it. When two packs ship one path, the
  later wins. The hub sets the minimum; a target can add packs and `ignore` paths, never
  remove a pack the hub assigns.
* **Opt-in.** A target receives nothing until `.engineering-assets.yml` exists at its
  root. An empty file is consent. An entry of `targets.yml` with `opt_in: assumed`
  subscribes the repositories it selects instead: they count as opted in without the
  file, as if it were empty; one such entry is enough. The file still wins:
  `enabled: false` in it opts any repository out (`skipped:opted-out`, its open sync pull
  request closed), and deleting it returns a subscribed repository to the hub's
  subscription rather than opting it out.
* **Targets** are named `[<provider>:]<path>` or by web URL (`https://host/owner/name`),
  whose provider is the one in `hub.yml` at that url; output and `--only` always use
  `<provider>:<path>`. `exclude` entries and the `match` filter of `org`/`group` entries
  are globs: `*` within a segment, `**` across segments, `?` one character, case
  ignored; YAML needs a glob that starts with `*` quoted (`"**/legacy"`).
* **Accounts.** A **reader** (read-only) runs `plan` on hub pull requests. A **writer**,
  a different account, runs `distribute` and `doctor` from the hub's default branch only.
  The **hub channel**, the CI's own token (`GITHUB_TOKEN`, `CI_JOB_TOKEN`), reaches the hub
  repository alone. The writer has no access to the hub.
* **Fingerprint.** The hub is known by its host and immutable repository id
  (`github.com/712345678`), which CI provides. `id` in `hub.yml` names the sync branch
  `touchmark/<id>` and is for people.
* **The sync branch** in each target is one touchmark commit on top of the target's
  default branch. If anyone else pushes to it (other than a clean *Update branch* merge of
  the base), touchmark pauses it (`blocked:edited`) and never drops their commits unless
  told to.
* **The marker**, an HTML comment at the end of the sync pull request's body, records the
  hub's fingerprint and a **content key**: the hash of every `(path, from, mode, to)` the
  pull request carries.
* **Memory.** A sync pull request a person closes without merging is a **decline**: the
  same content is not proposed again. New content, a change to `packs` or `ignore` in the
  opt-in file, or a declined path becoming `local` lifts it. A bot's close is not a
  decline: the content comes back after 30 days.
* **One-off operations** that override a safeguard (rebuild a paused branch, propose
  declined content again, allow a mass close) live in `.touchmark/operations.yml` and
  go through review; their command-line flags work only outside CI.
* `plan` and `distribute --dry-run` produce the same report: what each target would get.
  `distribute` writes. A second run on the same inputs writes nothing.

The life of a change: a pull request to the hub changes a pack → `check` and `plan`
(reader) report what each target would receive → merge → `distribute` (writer, default
branch) opens or updates one pull request per opted-in target → a person in each target
merges it, edits files (they become `local`), or closes it (a decline).

## Wiring

The smallest hub that works, and how to try it on your machine before any CI:

```yaml
# hub.yml
version: 1
id: acme-eng                       # not the template's change-me: check rejects it
writer: acme-assets-write[bot]     # the writer account; on GitLab, Gitea and Forgejo its username
packs:
  agents:
    description: AGENTS.md and a project profile
```

```yaml
# targets.yml
version: 1
defaults:
  packs: [agents]
targets:
  - repo: acme/billing
  - org: acme                      # on GitLab: group: acme/platform
    topics: [python]
```

```text
packs/agents/AGENTS.md             each file at least 64 bytes, committed
packs/agents/.agents/project.md
```

```yaml
# .engineering-assets.yml at the root of each target; an empty file is enough
version: 1
ignore:
  - .agents/project.md             # optional: paths this repository keeps for itself
```

A target can also ask for packs the hub does not assign it, with `packs:` in the same
file, as long as the hub defines them ([opt-in file](reference/opt-in.md)).

```sh
git -C ~/src/engineering-assets add -A                              # new pack files too
git -C ~/src/engineering-assets commit -m "feat: agents pack"      # only committed packs ship
touchmark check --hub ~/src/engineering-assets
cd ~/src/billing && touchmark status --hub ~/src/engineering-assets
touchmark apply --hub ~/src/engineering-assets --dry-run
```

In CI, copy the template's workflow (`.github/workflows/engineering-assets.yml`,
`.gitlab-ci.yml` or `.gitea/workflows/`) rather than writing one. Its load-bearing parts
on GitHub Actions:

```yaml
jobs:
  probe:                                   # no environment: sees what any branch could see
    runs-on: ubuntu-latest
    outputs:
      exposed: ${{ steps.probe.outputs.exposed }}
    steps:
      - id: probe
        run: echo "exposed=$EXPOSED" >> "$GITHUB_OUTPUT"
        env:
          EXPOSED: ${{ secrets.TOUCHMARK_WRITE_APP_KEY != '' || secrets.TOUCHMARK_WRITE_TOKEN != '' || secrets.TOUCHMARK_SIGNING_KEY != '' }}

  distribute:
    needs: probe
    runs-on: ubuntu-latest
    environment: touchmark-distribute     # holds the write key; default branch only
    permissions:
      contents: read
      actions: read                       # touchmark reads the environment's branch policy
    steps:
      - uses: actions/checkout@<commit> # v7.0.1
        with:
          fetch-depth: 0                  # the hub's whole history is its ownership record
          persist-credentials: false
      - uses: bedrock-python/touchmark@<commit> # vX.Y.Z
        with:
          command: distribute
        env:
          TOUCHMARK_KEY_EXPOSED: ${{ needs.probe.outputs.exposed }}
          TOUCHMARK_WRITE_APP_ID: ${{ vars.TOUCHMARK_WRITE_APP_ID }}
          TOUCHMARK_WRITE_APP_KEY: ${{ secrets.TOUCHMARK_WRITE_APP_KEY }}
          GITHUB_TOKEN: ${{ github.token }}
```

`touchmark setup github --hub .` or `touchmark setup gitlab --hub . --group <group>`,
run once by a maintainer with `TOUCHMARK_HUB_TOKEN` set, creates the reader and the
writer, stores their keys where only the default branch sees the write key, and protects
the hub. See [Set up a hub's platform](guide/setup.md).

## Commands

| Command | Runs | Credential | What it does |
|---|---|---|---|
| `check [--hub DIR] [--worktree]` | hub, every pull request | none | validates `hub.yml`, `targets.yml`, `operations.yml`, every pack, the history, and that the GitHub workflow's probe tests every write secret |
| `plan [--strict] [--all] [--comment] [--assume-opt-in] [--only REF]...` | hub pull request | reader | resolves targets, reads their opt-in files and branches, reports what `distribute` would do and the API writes it would cost; writes nothing (`--comment` keeps one comment in the hub pull request through the hub channel) |
| `distribute [--dry-run] [--only REF]... [--deadline D] [--strict] [--report FILE]` | hub default branch | writer | opens, updates and closes one sync pull request per opted-in target; `--dry-run` writes nothing |
| `doctor [--strict] [--only REF]... [--hub-token]` | hub default branch, weekly | writer | checks the writer against every target: access, token expiry, App permissions, branch rules, signing, the hub hidden from the writer, foreign markers; `--hub-token` (local only) also where the hub keeps its write key |
| `probe` | jobs any hub branch can start | — | exits 2 when the job can see a write credential or signing key |
| `setup github|gitlab [--dry-run] ...` | a maintainer's machine | `TOUCHMARK_HUB_TOKEN` | creates the reader and writer, stores the keys so only the default branch sees the write key, protects the hub; changes only what differs; exit 3 when a step is left to you |
| `migrate --from-multi-gitter FILE [--id ID] [--writer LOGIN]` | anywhere | reader, optional | prints `hub.yml`, `targets.yml` and `operations.yml` for a hub that replaces a multi-gitter setup on GitLab |
| `status [--hub DIR] [--dir DIR] [--repo REF] [--packs a,b] [--assume-opt-in]` | a target checkout | none | lists every managed path and its state; changes nothing |
| `apply [status flags] [--adopt GLOB]... [--dry-run]` | a target checkout | none | applies the packs to the working tree; `--adopt` takes `local` files back |
| `manifest [--hub DIR]` | hub | none | prints the ownership manifest as JSON |
| `schema hub|targets|opt-in|operations|report|doctor|setup` | anywhere | none | prints a JSON Schema |
| `version` | anywhere | none | prints the version, Go toolchain, platform, commit and its time |

Flags every hub command shares: `--hub DIR` (default `$TOUCHMARK_HUB`), `--format
text|json` (`plan`, `distribute`, `doctor` also `markdown`), `--worktree` (read packs and
configs from the hub's working tree instead of its last commit; `check`, `plan`,
`status`, `apply`; `distribute` refuses it). `--hub-fp HOST/ID` gives `plan`,
`distribute` and `doctor` the hub's fingerprint outside CI. `--only [PROVIDER:]PATH`
(repeatable or comma-separated) limits `plan`, `distribute` and `doctor` to some
targets, and turns the sweep of stale pull requests off.

Operation flags of `distribute`, refused in CI (exit 2): `--recreate TARGET@HEAD`,
`--forget-declines TARGET#PR`, `--allow-mass-close MAX`, `--adopt-unmarked`,
`--allow-stale`.

In CI, `plan` and `distribute` leave `touchmark-report.{json,md}` in the working
directory (`distribute` also streams `touchmark-report.jsonl`), and `doctor` leaves
`touchmark-doctor.{json,md}`; on GitHub Actions they write the step summary and up to
ten `::error` and ten `::warning` annotations.

## Configuration

`hub.yml` (in the hub; `touchmark schema hub`). Only `id` is required:

| Key | Default | Meaning |
|---|---|---|
| `version` | `1` | format version |
| `id` | — | slug, 3–40 characters; names the branch `touchmark/<id>`; `change-me` fails `check` |
| `writer`, `sign` | — , `auto` | single-provider shorthand: the writer account, commit signing (`auto`, `always`) |
| `providers[]` | from CI | `id`, `type` (`github`, `gitlab`, `gitea`, `forgejo`, `bitbucket`, `azure-devops`), `url` (required for gitea, forgejo and azure-devops: `https://dev.azure.com/<organization>`), `api_url`, `ca_file`, `writer`, `known_authors`, `automation_accounts`, `sign`, `limits`; replaces the shorthand |
| `branch`, `branch_aliases` | `touchmark/<id>`, none | the sync branch, and former names whose pull requests stay the hub's |
| `previous_fingerprints` | none | the hub's fingerprints before it moved |
| `opt_in_file` | `.engineering-assets.yml` | the opt-in file's path in targets |
| `commit.message` | `chore: sync engineering assets` | must not start with `Draft:` or `WIP:` |
| `pr.title`, `pr.labels`, `pr.draft`, `pr.intro_file`, `pr.link_hub` | `chore: sync engineering assets`, `[engineering-assets]`, `false`, none, `auto` | applied at creation; after that title, labels and draft belong to people; the body is touchmark's |
| `limits.max_new_prs_per_run`, `limits.max_close_fraction` | `100`, `0.1` | the rest is deferred to the next run; a run that would close more than max(5, fraction × open) closes none |
| `memory.auto_close_cooldown` | `30d` | how long content a bot closed waits before it is proposed again |
| `security.write_isolation` | `platform` | `platform` (CI keeps the key to the default branch; probed), `external` (an OIDC secret store), `none` (needs `security.reason`) |
| `security.private_targets_in_public_hub` | `skip` | `deliver` puts their names into public CI logs |
| `sensitive_paths` | none | patterns added to the built-in list highlighted under ⚠ in pull requests |
| `packs.<pack>.description`, `.requires`, `.formerly` | none | metadata; `requires` adds and orders dependencies; `formerly` keeps a renamed pack's history |

`targets.yml` (in the hub; `touchmark schema targets`): `defaults.provider`,
`defaults.packs`, `defaults.opt_in`; `targets[]` entries with exactly one of `repo`
(`owner/name`, `provider:path`, or a web URL), `org` or `group` (a namespace, by path or
web URL; with `topics` that must all match, `subgroups` default `true`, `forks` default
`false`, `match` a list of globs over the full path of which one must match), each with
optional `provider`, `packs` and `opt_in` (`required`, the default, or `assumed`);
`exclude[]` (repositories, globs or web URLs; it wins over every entry). A URL resolves to
the one provider whose `url` it lies under (`github` defaults to `https://github.com`,
`gitlab` to `https://gitlab.com`); none fails `check`, and so do several unless the
entry's `provider` or `defaults.provider` picks one of them, credentials, a query, a
fragment, and any scheme but `https` (`http` for loopback only). On GitHub, Gitea and
Forgejo a repository URL, and an `exclude` URL without `**`, names `owner/name`; on Azure
DevOps it is `https://dev.azure.com/<organization>/<project>/_git/<repository>`, naming
`<project>/<repository>`, and an `org` URL is the organization itself.

`.engineering-assets.yml` (in each target; `touchmark schema opt-in`): `version`,
`enabled` (default `true`; `false` opts out whatever the hub says), `packs` (added to
the hub's), `ignore` (paths or globs, `**` for any depth, at most 1000; a plain path
covers everything under it). At most 64 KiB.

`.touchmark/operations.yml` (in the hub; `touchmark schema operations`):
`recreate[]` (`target`, `head`: acts while the branch head is that commit),
`forget_declines[]` (`target`, `pr`: acts once; on Bitbucket Cloud and Azure DevOps while present), `allow_mass_close` (`max`, `until`),
`adopt_unmarked` (`until`). Dates are `YYYY-MM-DD`, UTC, inclusive.

Environment, one set per provider; `<ID>` is the provider id upper-cased with `-` as
`_`, and with one provider the names without `<ID>_` work too. touchmark removes each
from its environment once read:

| Variable | For |
|---|---|
| `TOUCHMARK_<ID>_READ_TOKEN`, or `TOUCHMARK_<ID>_READ_APP_ID` + `TOUCHMARK_<ID>_READ_APP_KEY` | `plan`, `migrate` |
| `TOUCHMARK_<ID>_WRITE_TOKEN`, or `TOUCHMARK_<ID>_WRITE_APP_ID` + `TOUCHMARK_<ID>_WRITE_APP_KEY` | `distribute`, `doctor` |
| `TOUCHMARK_<ID>_SIGNING_KEY` | an ssh ed25519 key to sign commits, optional |
| `TOUCHMARK_KEY_EXPOSED` | the GitHub probe job's answer; `distribute` and `doctor` need `false` there |
| `TOUCHMARK_HUB_TOKEN` | a maintainer's token for `setup` and `doctor --hub-token` |
| `TOUCHMARK_HUB` | the default of `--hub` |
| `GITHUB_TOKEN`, `CI_JOB_TOKEN`, `GITEA_TOKEN` | the hub channel: the hub repository only |

## Rules that hold or break the setup

1. **The hub is a full clone.** The history is the ownership record: on a shallow clone
   every managed file would look `local`. touchmark refuses a shallow clone and a partial
   clone (`--filter`). In CI: `fetch-depth: 0` (GitHub, Gitea), `GIT_DEPTH: "0"` (GitLab).
2. **Reader and writer are two different accounts** on every platform: two GitHub Apps,
   two GitLab service accounts (or group access tokens), two Gitea or Forgejo bot users,
   two Azure DevOps users with personal access tokens (Code read; Code read & write).
   The read key reaches any branch of the hub; the write key must not.
3. **The write key is visible to the default branch only.** GitHub: secrets in the
   environment `touchmark-distribute` with deployment branches *Selected* = the default
   branch, never repository or organisation secrets. GitLab: a variable that is
   protected, masked and hidden, environment scope `touchmark-distribute`, with only the
   default branch protected. The probe fails the run otherwise, and `distribute` refuses
   to start.
4. **Every write secret a workflow hands to touchmark is in the probe's expression.**
   `check` fails when a workflow passes a `TOUCHMARK_*_WRITE_*` or `*_SIGNING_KEY` the
   probe does not test.
5. **The writer has no access to the hub.** Install the Apps on targets only; keep the
   hub outside the GitLab groups the accounts belong to. `doctor` fails when the writer
   can push to the hub, and warns when it can see a private one.
6. **Nothing reaches a repository without its consent.** `.engineering-assets.yml` at
   the root, possibly empty, or an entry with `opt_in: assumed` when the hub owns the
   decision; `enabled: false` in the file overrides the hub. Never ship that file in a
   pack: `check` rejects it.
7. **Only committed packs ship.** `distribute` reads the hub's HEAD commit and refuses
   `--worktree`; it runs only when HEAD is the tip of the default branch (otherwise the
   run is `superseded` and writes nothing).
8. **Every pack file is at least 64 bytes, a regular file (mode 100644 or 100755).**
   `check` rejects smaller files, symlinks, submodules, a top-level `.lfsconfig`, and
   paths that collide by case.
9. **An edited file is the repository's for good.** touchmark never updates or deletes a
   `local` file. To get updates back: restore the pack's version, or `touchmark apply
   --adopt <glob>` and commit. Deleting a managed file is not opting out: the next run
   restores it. Opting out of a path is `ignore`.
10. **`hub.yml` names the writer.** `distribute` exits 2 when the write key belongs to
    another account. On GitHub `writer` is the App's bot, `<slug>[bot]`.
11. **Credentials come from the environment, under exactly these names.** Never action
    inputs, never configuration files: `check` fails on a line that looks like a token.
12. **Overrides go through review.** In CI the operation flags exit 2; add the entry to
    `.touchmark/operations.yml` and merge it. Each entry limits itself, so a forgotten
    one does nothing.
13. **A pack the hub stops assigning leaves its files in place** (`orphaned`). Renaming a
    pack without listing the old name under `formerly` makes its files look `local`.
14. **Gitea and Forgejo Actions cannot keep a secret to one branch.** With
    `write_isolation: platform`, `distribute` refuses to run there: run the hub's CI on
    GitHub or GitLab, or set `external`, or `none` with a `reason` and protect every
    branch.
15. **Outside CI, `plan`, `distribute` and `doctor` need `--hub-fp HOST/ID`**, and
    `hub.yml` must name the platform unless the hub's origin is on github.com, a
    `*.ghe.com` host or gitlab.com: a `providers` list, or `platform` (and `base_url`)
    at the top level.
16. **A public hub skips non-public targets** and prints only how many, because its CI
    logs are public. `security.private_targets_in_public_hub: deliver` changes that.
17. **Pin touchmark** by commit (the Action) and by digest (the image), and take updates
    through the reviewed Dependabot or Renovate pull request.

## Common mistakes

```yaml
# WRONG — a shallow checkout: every managed file looks local, and touchmark refuses it
- uses: actions/checkout@<commit>

# RIGHT
- uses: actions/checkout@<commit> # v7.0.1
  with:
    fetch-depth: 0
    persist-credentials: false
```

```yaml
# WRONG — a credential as an input, and the write key as a repository secret
- uses: bedrock-python/touchmark@<commit> # vX.Y.Z
  with:
    command: distribute
    token: ${{ secrets.WRITE_TOKEN }}

# RIGHT — the environment holds the key; the names are the ones touchmark reads
distribute:
  needs: probe
  environment: touchmark-distribute
  steps:
    - uses: bedrock-python/touchmark@<commit> # vX.Y.Z
      with:
        command: distribute
      env:
        TOUCHMARK_KEY_EXPOSED: ${{ needs.probe.outputs.exposed }}
        TOUCHMARK_WRITE_APP_ID: ${{ vars.TOUCHMARK_WRITE_APP_ID }}
        TOUCHMARK_WRITE_APP_KEY: ${{ secrets.TOUCHMARK_WRITE_APP_KEY }}
        GITHUB_TOKEN: ${{ github.token }}
```

```yaml
# WRONG — topics on a repo entry, a pull request title that makes a draft
targets:
  - repo: acme/billing
    topics: [python]
pr:
  title: "WIP: sync engineering assets"

# RIGHT — topics select within an org or group; drafts are pr.draft
targets:
  - org: acme
    topics: [python]
pr:
  draft: true
```

```sh
# WRONG — opting a target out of one file by deleting it: the next run restores it
git rm .agents/project.md
```

```yaml
# RIGHT — add the path to `ignore` in the target's .engineering-assets.yml, and keep
# the rest of the file as it is (rewriting `packs` drops packs and lifts every decline)
version: 1
ignore:
  - .agents/project.md
```

```sh
# WRONG — an override in CI: exit 2
touchmark distribute --recreate acme/api@4b1d9e0c8f5a3b2e1d0c9b8a7f6e5d4c3b2a1f0e

# RIGHT — the same, through review, in .touchmark/operations.yml
# recreate:
#   - target: acme/api
#     head: 4b1d9e0c8f5a3b2e1d0c9b8a7f6e5d4c3b2a1f0e
```

A target is `[PROVIDER:]PATH`. The prefix is the `id` of an entry of `providers` in
`hub.yml`, needed only when the hub has more than one; a hub with one provider, the
single-provider shorthand (`writer` at the top level) included, writes the path alone.

```sh
# WRONG — plan from a laptop without the fingerprint: exit 2
touchmark plan --hub .

# RIGHT — the hub's host and repository id
touchmark plan --hub . --hub-fp github.com/712345678
```

## Errors and exit codes

| Code | Meaning |
|---|---|
| `0` | success, including a `superseded` run; `probe`: no write key visible |
| `1` | an action failed: a `failed` target, a provider unavailable, the sweep failed, the mass-close guard fired; `doctor`: a check failed; `setup`: a step failed; `migrate`: an account it checked does not exist; or an unexpected git or I/O error |
| `2` | usage, configuration or guard error, nothing written: a bad flag or file, `check` errors, an unknown fingerprint, git too old, a shallow hub, the probe sees a write key, an operation flag in CI, `distribute` off the default branch, the writer does not match `hub.yml` |
| `3` | with `--strict`: a target `blocked` or `deferred`, `targets.yml` not fully resolved, the sweep off; `doctor --strict`: a check that warns or is unknown; `setup` (always): a step is left to you, run it again once done |

Each target ends with one outcome, and a reason for most:

| Outcome | Reasons | What to do |
|---|---|---|
| `opened`, `updated`, `unchanged`, `closed` | `updated`: `content`, `rebase`, `recreate`, `body`, `title`, `base-renamed`; `closed`: `no-diff`, `opted-out`, `target-dropped`, `duplicate` | nothing |
| `declined` | the declined pull request | nothing; see [memory](concepts/memory.md) to propose again |
| `skipped` | `not-opted-in`, `opted-out` (the opt-in file says `enabled: false`), `archived`, `disabled`, `empty`, `mirror`, `pending-deletion`, `prs-disabled`, `sha256`, `unsafe-opt-in`, `private-in-public-hub`, `superseded` | add the opt-in file (or `opt_in: assumed` in the hub), or nothing |
| `blocked` | `edited` (someone pushed to the sync branch), `branch-taken`, `branch-in-use`, `opt-in-invalid`, `marker-invalid`, `rules:<rule>`, `permission:<what>`, `cannot-sign`, `archived`, `mass-close` | tick *Rebuild this branch* (not on Bitbucket Cloud) or add a `recreate` entry; fix the opt-in file; grant the permission; add a signing key; `allow_mass_close` |
| `deferred` | `rate-limit`, `deadline`, `rollout-limit`, `provider-down`, `interrupted`, `cooldown` | nothing: the next run continues |
| `failed` | `transient`, `auth`, `access`, `git`, `integrity`, `race`, `secret-exposure`, `internal` | read the report's reason; `auth` and `access` are the credential |

Frequent messages: *the hub's fingerprint is unknown* (pass `--hub-fp`); *hub.yml declares
no providers* (add `providers`, or run from CI); *git X is too old: plan needs
git 2.45.0* (`distribute` too; use the image); *hub checkout is shallow* (`fetch-depth: 0`); *TOUCHMARK_…
is visible in this job* (move the key into the protected environment). See
[Troubleshoot](guide/troubleshooting.md).

## Documentation map

Fetch a page when the task is the one named beside it.

| Page | Read it when |
|---|---|
| [Installation](getting-started/installation.md) | choosing between the Action, the image and a binary; verifying a download |
| [A hub on GitHub](getting-started/github.md) | creating a hub with GitHub Apps and the `touchmark-distribute` environment |
| [A hub on GitLab](getting-started/gitlab.md) | creating a hub with service accounts and protected variables |
| [A hub on Gitea or Forgejo](getting-started/gitea-forgejo.md) | delivering to or hosting on Gitea and Forgejo |
| [Opt a repository in](getting-started/opt-in.md) | writing a target's `.engineering-assets.yml`; a hub that subscribes repositories (`opt_in: assumed`) and how they opt out |
| [How it works](concepts/overview.md) | the flow from a hub pull request to a sync pull request |
| [Ownership by provenance](concepts/ownership.md) | the file states, `local`, `--adopt`, CRLF, `orphaned` |
| [Packs and opt-in](concepts/packs.md) | pack selection order, `requires`, `formerly`, seed files, consent by file or by the hub |
| [Delivery and the sync branch](concepts/delivery.md) | the branch, the commit, signing, pull request fields, stale pull requests |
| [Memory of declined pull requests](concepts/memory.md) | why a pull request did not come back, and how to bring it back |
| [Security model](concepts/security.md) | the accounts, the probe, write isolation, what a target can and cannot do |
| [Set up a hub's platform](guide/setup.md) | `touchmark setup github` and `setup gitlab`, every step and flag |
| [Write packs](guide/packs.md) | pack layout, size, reserved files, sensitive paths |
| [Run it in CI](guide/ci.md) | the Action's inputs, the image on GitLab, Gitea and Forgejo, reports and summaries |
| [Deliver to several platforms](guide/providers.md) | `providers`, provider ids in `targets.yml`, per-provider variables |
| [One-off operations](guide/operations.md) | `recreate`, `forget_declines`, `allow_mass_close`, `adopt_unmarked` |
| [Check the write account](guide/doctor.md) | reading `doctor`, `--hub-token` |
| [Use it on your machine](guide/local.md) | `status`, `apply`, `--adopt`, `--worktree`, a local `plan` |
| [Migrate from multi-gitter](guide/migrate.md) | replacing a multi-gitter rollout on GitLab |
| [Troubleshoot](guide/troubleshooting.md) | a blocked or failed target, an error message |
| [FAQ](guide/faq.md) | the questions teams ask about sync pull requests |
| [Commands](reference/commands.md) | an exact flag — the help of every command |
| [hub.yml](reference/hub.md), [targets.yml](reference/targets.md), [opt-in file](reference/opt-in.md), [operations.yml](reference/operations.md) | every key, type and default |
| [Environment variables](reference/environment.md) | every variable touchmark reads |
| [Exit codes](reference/exit-codes.md) | scripting around touchmark |
| [Reports](reference/output.md) | parsing `--format json`, the CI artifacts |
| [The GitHub Action](reference/action.md) | the Action's inputs and what it runs |
| [Glossary](reference/glossary.md) | a term used here without explanation |
| [Threat model](project/threat-model.md) | each threat, its control, its test, the risk that remains |
| [Changelog](changelog.md) | what changed between versions |
