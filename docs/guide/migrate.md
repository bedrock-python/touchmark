# Migrate from multi-gitter

A hub that rolled files out with [multi-gitter](https://github.com/lindell/multi-gitter)
on GitLab can move to touchmark without closing what is open: the merge requests
multi-gitter opened are taken over, gain a marker at the first write, and carry on.

## 1. Generate the configuration

```sh
touchmark migrate --from-multi-gitter multi-gitter.yml --id acme-eng --writer touchmark-writer
```

`migrate` reads the multi-gitter file (the keys `multi-gitter run --config` reads:
`platform`, `base-url`, `group`, `project`, `user`, `topic`, `include-subgroups`,
`skip-forks`, `skip-repo`, `repo-include`, `repo-exclude`, `branch`, `pr-title`,
`commit-message`, `labels`, `author-email` and others) and prints three files, one after
the other under `# ==> <name> <==` lines:

```yaml
# ==> hub.yml <==
version: 1
id: acme-eng
branch_aliases:
  - chore/sync-engineering-assets       # multi-gitter's branch: its merge requests stay there
providers:
  - id: gitlab
    type: gitlab
    url: "https://gitlab.example.com"
    writer: touchmark-writer
    known_authors:
      - group_42_bot_0123456789abcdef0123456789abcdef   # the account that opened them
commit:
  message: "chore: sync engineering assets"
pr:
  title: "chore: sync engineering assets"
  labels:
    - engineering-assets

# ==> targets.yml <==
version: 1
targets:
  - group: acme/platform
    forks: true
    topics: [python]
  - repo: acme/tools/cli
exclude:
  - platform/legacy

# ==> .touchmark/operations.yml <==
version: 1
adopt_unmarked:
  until: 2026-10-27                      # 30 days from the run
```

- **multi-gitter's bot.** With a read token for the provider
  (`TOUCHMARK_GITLAB_READ_TOKEN` or `TOUCHMARK_READ_TOKEN`, scope `read_api`), `migrate`
  finds the account that opened the open merge requests on multi-gitter's branch, in up to
  100 projects the file selects, and checks every login it names. Without one, or with
  `--bot LOGIN`, it prints the logins unchecked with a `TODO`. It prints the host and the
  variable it reads before the first request.
- **Topics.** multi-gitter takes a project with any one of its topics; a touchmark entry
  needs all of them, so `migrate` writes one entry per topic.
- **What it cannot carry over** is left as a `TODO` comment: the pull request body (put
  it in a file of the hub and set `pr.intro_file`), the packs each target gets, the
  writer if `--writer` is missing. The author name and email are dropped: the writer
  authors every commit. A `Draft:` title becomes `pr.draft: true`.
- **Self-managed instances** with a private CA: `--ca-file FILE`.

It exits 2 when the file is not a GitLab multi-gitter config or names something touchmark
cannot express, and 1 when an account it checked does not exist.

## 2. Build the hub

1. Create the hub from [the template](https://github.com/bedrock-python/engineering-assets-template)
   on the same GitLab instance ([A hub on GitLab](../getting-started/gitlab.md)).
2. Save the three files, resolve every `TODO`, move the files multi-gitter's script
   copied into packs under `packs/`, and assign them in `targets.yml`.
3. Decide how targets opt in. multi-gitter delivered without asking; touchmark waits
   for each repository's opt-in file unless `targets.yml` subscribes it.
   `defaults.opt_in: assumed` keeps multi-gitter's behaviour: every target gets its
   packs without the file, and a team opts out with `enabled: false` in it. Without it,
   add the opt-in file to every target first: the first `distribute` skips the others
   (`not-opted-in`) and takes none of their multi-gitter merge requests over.
4. Run `touchmark check`, open a merge request and read `plan`: it shows what
   `distribute` would do with each multi-gitter merge request, and the effect of the
   `adopt_unmarked` entry.

## 3. Switch over

Merge, and stop multi-gitter's pipeline. The first `distribute`:

- takes over each open merge request on the alias from a `known_authors` account: its
  branch is rewritable when every path it changes holds content the hub has shipped (or
  deletes the hub's content), whatever the commits' author;
- writes touchmark's commit and marker into it;
- opens new merge requests on `touchmark/<id>`.

Closed merge requests of multi-gitter, without a marker, never count as declines: their
content is unknown. Remove the `adopt_unmarked` entry once the move is over (it stops
acting after its date anyway), and revoke multi-gitter's token. A revoked group access
token's bot stays the author of its merge requests, and stays in `known_authors`.

## What changes for the targets

| multi-gitter | touchmark |
|---|---|
| every selected repository gets the change | repositories with `.engineering-assets.yml`, or those `targets.yml` subscribes (`opt_in: assumed`) |
| a file a team changed is overwritten, or the script decides | a changed file is the team's, never touched again |
| a closed merge request comes back on the next run | a declined merge request is remembered |
| one body for the whole run | a body per target: what changes there, what is sensitive |
