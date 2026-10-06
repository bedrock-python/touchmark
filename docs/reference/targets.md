# targets.yml

Which repositories a hub delivers to and which packs they get, at the root of the hub
repository. `touchmark schema targets` prints the JSON Schema.

```yaml
version: 1
defaults:
  provider: gh                     # needed only when hub.yml lists several providers
  packs: [agents]                  # every target gets these
targets:
  - repo: acme/billing             # one repository, on the default provider
    packs: [python-service]
  - repo: corp:platform/api        # <provider>:<path>
  - provider: corp
    group: platform                # GitLab: with subgroups unless subgroups: false
    topics: [python]               # all of them must match
    packs: [python-service]
  - org: acme                      # org and group are synonyms: a namespace
    topics: [library]
    forks: false                   # the default; an explicit repo: entry is never skipped as a fork
    packs: [python-library]
exclude:
  - acme/legacy-monolith
  - corp:platform/sandbox
```

- **A target's packs**, in order: `defaults.packs`, the `packs` of every matching entry
  in file order, the `packs` of its opt-in file; duplicates dropped, each pack's
  `requires` before it.
- **A target's provider**, in order: the entry's `provider`, the `<provider>:` prefix,
  `defaults.provider`, the only provider. `check` fails when that leaves a choice.
- **Identity.** Output, `--only` and `operations.yml` name a target `<provider>:<path>`.
  Inside a run a target is its host and immutable repository id, so a repository two
  entries select is delivered to once, with the packs of both. A repository whose path
  changed gets a `renamed` warning.
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

## Keys

<!-- generated: schema targets -->

| Key | Type | Description |
|---|---|---|
| `version` | `1` | Format version. Only 1 exists; when absent, 1 is assumed with a warning. |
| `defaults` | object | Applies to every target. |
| `defaults.provider` | string | Provider of entries that name none. Needed when hub.yml has several providers. |
| `defaults.packs` | list of strings, each up to 64 characters | Packs every target gets, before the packs of its entries and its opt-in file. |
| `targets` | list of objects | Repositories, organisations and groups to deliver to, in delivery order. |
| `targets[].repo` | string, up to 512 characters | One repository: owner/name or group/sub/project, optionally prefixed with a provider id and a colon. An entry has exactly one of `repo`, `org` and `group`. |
| `targets[].provider` | string | Id of a provider in hub.yml. |
| `targets[].packs` | list of strings, each up to 64 characters | — |
| `targets[].org` | string, up to 512 characters | Every repository in a GitHub organisation or a GitLab, Gitea or Forgejo namespace (a synonym of group). An entry has exactly one of `repo`, `org` and `group`. |
| `targets[].topics` | list of strings | Only repositories with all of these topics. Only with `org` or `group`. |
| `targets[].subgroups` | boolean | Include repositories in nested groups. Default: true. Only with `org` or `group`. |
| `targets[].forks` | boolean | Include forks. Default: false. Only with `org` or `group`. |
| `targets[].group` | string, up to 512 characters | Every repository in a GitLab group, with its subgroups unless subgroups is false (a synonym of org). An entry has exactly one of `repo`, `org` and `group`. |
| `exclude` | list of strings, each up to 512 characters | Repositories never delivered to, even when an entry selects them. |
| `repos` | list of strings, each up to 512 characters | Deprecated. The pre-v1 format: a bare list of repository paths, read as repo: entries. |

<!-- end generated -->
