# Opt a repository in

Nothing happens to a repository until it has `.engineering-assets.yml` at its root. The
file can be empty: its presence is the consent. It can also ask for more packs and keep
some paths for itself:

```yaml
# .engineering-assets.yml
version: 1
packs: [claude]                 # in addition to what the hub assigns
ignore:
  - .claude/settings.json       # we keep our own
  - docs/guidelines/**
```

Commit it to the default branch. The next `distribute` run opens a sync pull request with
everything the hub's packs carry for this repository; a team that wants to see it first
runs [`touchmark status`](../guide/local.md) in a checkout.

## What the file can do

- **Add packs.** `packs` adds to the packs the hub assigns in `targets.yml`. It cannot
  remove one: the hub sets the minimum.
- **Keep paths.** `ignore` lists paths or globs touchmark must never create, update or
  delete here. A path without glob characters covers everything beneath it; `**` matches
  any number of directories; at most 1000 patterns.
- **Change its mind.** Editing `packs` or `ignore` lifts the memory of declined pull
  requests: content the team declined by closing a sync pull request is proposed again.
  Comments and formatting don't count.
- **Opt out.** Deleting the file stops delivery; the open sync pull request is closed
  with the reason `opted-out`. Files already merged stay.

The file is read through the platform's API from the default branch: at most 64 KiB, a
regular file, strict YAML. An invalid file blocks the target (`blocked:opt-in-invalid`)
until it is fixed; a symlink or a directory in its place skips it (`unsafe-opt-in`).

## Which packs a repository gets

The hub lists repositories in `targets.yml`:

```yaml
# targets.yml in the hub
version: 1
defaults:
  packs: [agents]
targets:
  - repo: your-org/billing
    packs: [python-service]
  - org: your-org               # on GitLab: group: your-org/platform
    topics: [python-library]
    packs: [python-library]
  - repo: corp:platform/api     # a repository on another provider from hub.yml
exclude:
  - your-org/legacy-monolith
```

A repository's packs, in order: `defaults.packs`, then the `packs` of every entry that
matches it (in file order), then the `packs` of its opt-in file. Duplicates are dropped,
and each pack's `requires` from `hub.yml` comes before it. When two packs ship the same
path, the later one wins. See [Packs and opt-in](../concepts/packs.md).

## What the team sees

A sync pull request from the hub's writer on the branch `touchmark/<id>`:

- a table of every change, by file and pack;
- a **⚠ Sensitive paths** section listing changes to workflows, CI configuration, agent
  settings, skills and subagents, CODEOWNERS and executable files, which is never cut;
- a folded list of the files the repository has made its own (`local`), with how to take
  them back;
- the tick boxes *Rebuild this branch* and *Propose this content again* when they apply;
- a marker at the end, an HTML comment that records what the pull request carries.

The team reviews it like any other pull request. It can edit the title, the labels and
the draft state: touchmark leaves them alone after creation. It should comment rather
than edit the description, which touchmark rewrites when the proposed changes change.

## What a team can do next

| To… | Do this |
|---|---|
| take the changes | merge the pull request |
| keep its own version of a file | edit the file in the repository (it becomes `local`), or add it to `ignore` |
| decline the whole change | close the pull request without merging; the same content will not come back |
| get declined content after all | reopen the pull request, or tick *Propose this content again* |
| take a diverged file back under the hub | `touchmark apply --adopt <path>` in a checkout, and open a pull request |
| push a fix to the sync branch | don't: touchmark pauses the branch; *Update branch* is fine |

See the [FAQ](../guide/faq.md) for the questions teams ask.
