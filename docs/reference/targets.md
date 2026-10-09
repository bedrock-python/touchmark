# targets.yml

Which repositories a hub delivers to and which packs they get, at the root of the hub
repository. `touchmark schema targets` prints the JSON Schema.

```yaml
version: 1
defaults:
  provider: gh                     # needed only when hub.yml lists several providers
  packs: [agents]                  # every target gets these
  opt_in: required                 # the default: a target waits for its opt-in file
targets:
  - repo: acme/billing             # one repository, on the default provider
    packs: [python-service]
  - repo: corp:platform/api        # <provider>:<path>
  - repo: https://gitlab.example.com/platform/web   # a web URL: the provider at that url
  - provider: corp
    group: platform                # GitLab: with subgroups unless subgroups: false
    topics: [python]               # all of them must match
    packs: [python-service]
  - org: acme                      # org and group are synonyms: a namespace
    topics: [library]
    forks: false                   # the default; an explicit repo: entry is never skipped as a fork
    packs: [python-library]
  - org: acme
    match: [acme/svc-*]            # only the repositories whose path matches
    opt_in: assumed                # opted in by the hub, without an opt-in file
exclude:
  - acme/legacy-monolith
  - corp:platform/sandbox
  - corp:platform/legacy/**        # a subgroup, with every project beneath it
  - acme/*-archive
```

- **A target's packs**, in order: `defaults.packs`, the `packs` of every matching entry
  in file order, the `packs` of its opt-in file; duplicates dropped, each pack's
  `requires` before it.
- **A target's provider**, in order: the entry's `provider`, the `<provider>:` prefix,
  `defaults.provider`, the only provider. `check` fails when that leaves a choice.
- **Identity.** Output, `--only` and `operations.yml` name a target `<provider>:<path>`,
  also when `targets.yml` names it by its URL. Inside a run a target is its host and
  immutable repository id, so a repository two entries select is delivered to once, with
  the packs of both. A repository whose path changed gets a `renamed` warning.
- **Always skipped**, with the reason in the report: archived, disabled, empty, mirror,
  pending deletion, pull requests turned off, SHA-256 repositories, and, in a public hub,
  non-public ones.
- **`exclude`** wins over every entry.
- **The pre-v1 format**, a bare `repos:` list without `version`, is still read as
  `repo:` entries on the only provider.

`org` and `group` entries are resolved through the platform's API, so a local `status`
or `apply` cannot resolve them. When an `org` or `group` entry with `packs` might include
the target, `status` warns, and `apply` wants `--packs`; entries without `packs` add
nothing a local run could miss.

## Web URLs

`repo`, `org`, `group` and `exclude` also take the web URL of a repository or a
namespace, as a browser shows it: `https://github.com/acme/billing`,
`https://gitlab.example.com/platform/api`, with or without a trailing `/`, and for a
repository with or without `.git`.

- **The provider** is the one in `hub.yml` whose `url` the URL lies under: the same
  scheme, host and port, and the provider's path as a prefix when the instance lives
  under one (`https://example.com/gitlab`). A `github` provider without `url` is at
  `https://github.com`, a `gitlab` one at `https://gitlab.com`. A hub with `writer` at the
  top level of `hub.yml` instead of `providers` has the url of the CI's own platform
  (`GITHUB_SERVER_URL`, `CI_SERVER_URL`). Outside CI, the hub's `origin` remote tells it
  only on `github.com`, a `*.ghe.com` host or `gitlab.com`: a hub on any other instance
  that names targets by URL lists the instance under `providers`, or its local `check`,
  `status --hub` and `apply --hub` fail, and so does a run in another platform's CI (a
  target's own GitHub Actions job reading a hub on GitLab). `check` in CI warns about it.
- **Exactly one provider** must match. A URL under no provider's `url` fails `check`.
  When two providers share an instance, the entry's `provider` picks one, else
  `defaults.provider` when it is one of them; an `exclude` URL takes `defaults.provider`
  only, or is written as `<provider>:<path>`.
- **The rest of the path** is the target's path: with a provider `corp` at
  `https://gitlab.example.com`, `https://gitlab.example.com/platform/api` is
  `corp:platform/api`. On GitHub, Gitea and Forgejo a repository URL names `owner/name`,
  and so does an `exclude` URL without `**` (a page inside a repository, such as
  `/tree/main`, would exclude nothing); an organisation URL names one owner. A GitLab URL
  with a `/-/` segment (a file, a merge request) points inside a project and is refused.
  `.git` is dropped from a repository URL, but a pattern ending in `.git` is refused: it
  would widen (`acme/*.git` to `acme/*`).
- **A namespace URL in `exclude`** names one repository at that path, not what lies
  beneath it: browsers show groups and projects alike, so to leave out a group write its
  URL with `/**` (`https://gitlab.example.com/platform/legacy/**`). `plan` and
  `distribute` warn about an `exclude` entry that excludes nothing while targets lie
  beneath it.
- **Refused:** credentials (`user:password@`), a query (`?tab=readme`), a fragment
  (`#readme`), and any scheme but `https`; `http` is accepted for `localhost`,
  `127.0.0.1` and `[::1]` only, as in `hub.yml`.

touchmark rewrites every URL as `<provider>:<path>` as soon as it reads `targets.yml`:
reports, `--only` and `operations.yml` keep that form.

## Patterns

`exclude` entries and the `match` patterns of `org` and `group` entries are glob
patterns over a repository's full path:

| Pattern | Matches |
|---|---|
| `*` | any characters within one path segment: `acme/legacy-*` |
| `**` | as a whole segment, any number of segments, none included: `corp:platform/legacy/**` is every project of the subgroup and of the groups beneath it |
| `?` | one character: `acme/svc-?` |

Paths compare without regard to case. A pattern covers a whole repository path, so it
has at least two segments; `**` inside a longer segment (`svc-**`) is `*`. An entry
without glob characters names one repository, as before: `acme/legacy` does not exclude
`acme/legacy-api`, nor anything beneath `acme/legacy`. `[` is not a glob character and
is refused; in a URL `?` would start a query, so a URL pattern takes `*` and `**` only.

In YAML a value that starts with `*` reads as an alias, so quote such a pattern:
`- "**/legacy"`, `match: ["*/svc-*"]`.

- **`exclude`**: `[<provider>:]<pattern>`, or a URL. The provider is found as for an
  entry: the prefix, else `defaults.provider`, else the only provider. A pattern that
  covers no `repo:` entry is fine: it is there for the repositories of `org` and `group`
  entries. One that covers a `repo:` entry is a warning: the exclude wins. So is one of
  more than `owner/name` without `**` on GitHub, Gitea or Forgejo, which have no nested
  namespaces: it can cover no repository.
- **`match`**, on `org` and `group` entries only: patterns over the full path, without
  a provider (`acme/svc-*`, `platform/**/api`). A repository of the namespace is selected
  when its path matches at least one of them. `check` warns about a pattern that no
  repository of the namespace can match: outside it, or deeper than `subgroups: false`
  allows, or than a GitHub, Gitea or Forgejo organisation holds (`owner/name`). An empty
  list, a URL, a provider prefix, and `match` on a `repo:` entry are errors.

## Opt-in

A repository receives nothing until it has [the opt-in file](opt-in.md), unless the hub
subscribes it: `opt_in` on an entry, or in `defaults` for every entry that sets none.

- **`required`** (the default): a repository the entry selects is skipped
  (`not-opted-in`) until it adds the opt-in file.
- **`assumed`**: a repository the entry selects counts as opted in without the file, as
  if it had an empty one. It gets the packs of `defaults` and of its entries, and ignores
  nothing. Its sync pull request says that the hub subscribed it, how to choose packs or
  ignore files (add the opt-in file), and how to opt out (`enabled: false` in it).

**One entry is enough.** A repository is subscribed when at least one entry that selects
it has `assumed`, its own or through `defaults.opt_in`, whatever the other entries say.
Entries add up, as their packs do: a `repo:` entry that gives one repository of a
subscribed organisation more packs must not unsubscribe it in passing. To let a
repository of that organisation opt in by itself, keep it out of the `assumed` entry
with `match`. `check` warns about a `repo:` entry that says `opt_in: required` while an
`assumed` entry may select the same repository.

**The repository decides last.** Its opt-in file always wins: with one, its `packs` and
`ignore` apply, as for any target; with `enabled: false` in it, the repository is opted
out whatever `targets.yml` says (`skipped:opted-out`), and its open sync pull request is
closed as `opted-out`. Deleting the opt-in file of a subscribed repository returns it to
the hub's subscription; it does not opt it out.

**Withdrawing a subscription** (dropping `assumed` from an entry or from `defaults`)
makes the repositories it subscribed without an opt-in file `not-opted-in` again: their
open sync pull requests are closed as `opted-out`, with a comment that says the hub no
longer subscribes the repository and that the opt-in file brings the changes back.

**Declines work as usual.** The first sync pull request of a subscribed repository is a
proposal: closing it without merging is a decline, and the same content is not proposed
again. An opt-in file that changes `packs` or `ignore` lifts the decline, like any edit
of those keys; an empty one does not.

In reports, a subscribed target without the opt-in file has `opt_in_assumed: true` and
`opt_in_assumed_by: targets.yml`, and the text and Markdown outputs count such targets.
`plan --assume-opt-in` is another thing: it takes an empty opt-in file for every target
that has none, for that report only (`opt_in_assumed_by: --assume-opt-in`).

A local `status` or `apply` knows the target by its path only. A `repo:` entry with
`assumed` that names it opts it in (`opted in by targets.yml`). An `org` or `group` entry
with `assumed` cannot be resolved locally: the target counts as not opted in, and
`status` warns that `plan` has the answer. An opt-in file in the checkout settles it
either way.

## Keys

<!-- generated: schema targets -->

| Key | Type | Description |
|---|---|---|
| `version` | `1` | Format version. Only 1 exists; when absent, 1 is assumed with a warning. |
| `defaults` | object | Applies to every target. |
| `defaults.provider` | string | Provider of entries that name none. Needed when hub.yml has several providers. |
| `defaults.packs` | list of strings, each up to 64 characters | Packs every target gets, before the packs of its entries and its opt-in file. |
| `defaults.opt_in` | one of `required`, `assumed` | The opt_in of every entry that sets none: required (the default) or assumed. |
| `targets` | list of objects | Repositories, organisations and groups to deliver to, in delivery order. |
| `targets[].repo` | string, up to 512 characters | One repository: owner/name or group/sub/project, optionally prefixed with a provider id and a colon, or its web URL, such as https://gitlab.example.com/platform/api, whose provider is the one hub.yml lists at that url. An entry has exactly one of `repo`, `org` and `group`. |
| `targets[].provider` | string | Id of a provider in hub.yml. |
| `targets[].packs` | list of strings, each up to 64 characters | — |
| `targets[].opt_in` | one of `required`, `assumed` | required (the default): a repository the entry selects gets nothing until it adds the opt-in file. assumed: it counts as opted in without one, with the packs targets.yml gives it, until its opt-in file says enabled: false. One entry with assumed is enough to subscribe a repository. |
| `targets[].org` | string, up to 512 characters | Every repository in a GitHub organisation, a GitLab, Gitea or Forgejo namespace, a Bitbucket workspace, a Bitbucket Data Center project (by its key) or an Azure DevOps organization (the provider's url; a project is selected with match) (a synonym of group), named by its path or its web URL. An entry has exactly one of `repo`, `org` and `group`. |
| `targets[].topics` | list of strings | Only repositories with all of these topics (repository labels on Bitbucket Data Center). Bitbucket Cloud and Azure DevOps have no topics: check refuses them on their entries; use match. Only with `org` or `group`. |
| `targets[].subgroups` | boolean | Include repositories in nested groups. Default: true. Only with `org` or `group`. |
| `targets[].forks` | boolean | Include forks. Default: false. Only with `org` or `group`. |
| `targets[].match` | list of strings, each up to 512 characters | Only repositories whose full path, without the provider, matches at least one of these patterns: \* within one segment, \*\* any number of segments, ? one character, compared ignoring case, such as acme/svc-\* or platform/\*\*/api. Quote a pattern that starts with \*, which YAML reads as an alias. Only with `org` or `group`. |
| `targets[].group` | string, up to 512 characters | Every repository in a GitLab group, with its subgroups unless subgroups is false (a synonym of org), named by its path or its web URL. An entry has exactly one of `repo`, `org` and `group`. |
| `exclude` | list of strings, each up to 512 characters | Repositories never delivered to, even when an entry selects them: a repository as repo names one, or a pattern of repository paths in which \* matches within one segment, \*\* any number of segments and ? one character (corp:platform/legacy/\*\* covers a subgroup at any depth), compared ignoring case. A web URL works too, with \* and \*\* but not ?. Quote a pattern that starts with \*, which YAML reads as an alias. |
| `repos` | list of strings, each up to 512 characters | Deprecated. The pre-v1 format: a bare list of repository paths, read as repo: entries. |

<!-- end generated -->
