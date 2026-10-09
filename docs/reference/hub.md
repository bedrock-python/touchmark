# hub.yml

The settings of a hub, at the root of the hub repository. Only `id` is required; every
other key has a default. `touchmark schema hub` prints the JSON Schema, for editors.

```yaml
version: 1
id: acme-eng                        # required; 3–40 characters, lowercase words joined by hyphens
# branch: touchmark/acme-eng        # default: touchmark/<id>
branch_aliases: [chore/sync-engineering-assets]   # former branch names: their pull requests stay the hub's
previous_fingerprints: []           # the hub's fingerprints before a move, like gitlab.example.com/1234
opt_in_file: .engineering-assets.yml
commit:
  message: "chore: sync engineering assets"
pr:
  title: "chore: sync engineering assets"
  labels: [engineering-assets]
  draft: false
  intro_file: .touchmark/pr-intro.md
  link_hub: auto                    # auto: no links to a private hub in public targets
limits:
  max_new_prs_per_run: 100
  max_close_fraction: 0.1           # a run that would close more than max(5, 10%) closes none
memory:
  auto_close_cooldown: 30d          # a bot's close: proposed again after 30d, then 60d; the third counts
security:
  write_isolation: platform         # platform | external | none
  # reason: "..."                   # required with none, and with platform on Bitbucket Pipelines
  private_targets_in_public_hub: skip   # skip | deliver
providers:
  - id: gh                          # TOUCHMARK_GH_* variables; gh:owner/name in targets.yml
    type: github                    # github | gitlab | gitea | forgejo
    url: https://github.com
    writer: acme-assets-write[bot]
    sign: auto                      # auto | always
  - id: corp
    type: gitlab
    url: https://gitlab.example.com
    ca_file: certs/corp-ca.pem
    writer: touchmark-writer
    known_authors: [group_42_bot_8f3c]
    automation_accounts: [stale-bot]
    limits: { writes_per_minute: 120 }
sensitive_paths:
  - deploy/**
packs:
  claude:
    description: Claude Code skills, a reviewer agent and settings
    requires: [agents]
  python-library:
    formerly: [python-lib]
```

## Rules `check` adds

The schema cannot say everything. `touchmark check` also requires:

- `id` is not the template's placeholder `change-me`;
- the single-provider shorthand (`writer`, `sign`, `platform`, `base_url`) and
  `providers` are not both used; provider ids are unique; `gitea` and `forgejo`
  providers have a `url`;
- every pack in `requires` exists, `requires` has no cycle, and a `formerly` name is
  neither a current pack nor claimed by two packs;
- `commit.message` and `pr.title` are not blank and do not start with `Draft:` or `WIP:`
  (set `pr.draft` instead); `commit.message` holds no git scissors line;
- `security.reason` is set when `write_isolation` is `none` (and `distribute` and
  `doctor` on Bitbucket Pipelines need it under `platform` too: see
  [A hub on Bitbucket Cloud](../getting-started/bitbucket.md));
- no line of the file looks like a token (`ghp_`, `github_pat_`, `ghs_`, `glpat-`,
  `-----BEGIN`).

In a hub pull request, `providers` and `ca_file` are read from the default branch, and
`sensitive_paths` is the union of the default branch's and the pull request's.

## Keys

<!-- generated: schema hub -->

| Key | Type | Description |
|---|---|---|
| `version` | `1` | Format version. Only 1 exists; when absent, 1 is assumed with a warning. |
| `id` | string, 3 to 40 characters, required | Short slug naming this hub, used in the sync branch name and in pull request markers. The template's placeholder "change-me" fails `touchmark check`. |
| `branch` | string | Sync branch in every target. Default: touchmark/&lt;id&gt;. |
| `branch_aliases` | list of strings | Former sync branch names. Open pull requests on them are still recognised as this hub's own. |
| `previous_fingerprints` | list of strings | Former fingerprints of this hub (host/repository-id: a numeric id, or a Bitbucket repository UUID with or without braces), after the hub moved. |
| `opt_in_file` | string | Path of the opt-in file in targets. Default: .engineering-assets.yml. |
| `commit` | object | Commit settings. The commit author is always the provider's writer account. |
| `commit.message` | string | Commit message. Default: "chore: sync engineering assets". Must not start with Draft: or WIP:, which turns a GitLab merge request into a draft. |
| `commit.author` | object | Deprecated. Ignored with a warning: commits are always authored by the writer account. |
| `commit.author.name` | string | — |
| `commit.author.email` | string | — |
| `pr` | object | Pull request settings. After creation, the title, draft state and labels belong to people. |
| `pr.title` | string | Title of new pull requests. Default: "chore: sync engineering assets". Must not start with Draft: or WIP:, which makes a GitLab or Gitea pull request a draft: set draft instead. |
| `pr.labels` | list of strings, each up to 50 characters | Labels of new pull requests. Default: [engineering-assets]; [] for none. |
| `pr.draft` | boolean | Open new pull requests as drafts. Default: false. |
| `pr.intro_file` | string | Hub file whose text opens every pull request body. |
| `pr.link_hub` | one of `auto`, `always`, `never` | Whether the body links to the hub. auto leaves the links out when the hub is private and the target public. Default: auto. |
| `limits` | object | Guards against mass actions. |
| `limits.max_new_prs_per_run` | integer ≥ 0 | New pull requests per run; the rest are deferred to the next run. Default: 100. |
| `limits.max_close_fraction` | number > 0, ≤ 1 | If a run would close more than max(5, this fraction of the hub's open pull requests), it closes none. Default: 0.1. |
| `memory` | object | Memory of pull requests closed without merging. |
| `memory.auto_close_cooldown` | string | How long content closed by a bot waits before it is proposed again, in days or hours. Default: 30d. |
| `security` | object | — |
| `security.write_isolation` | one of `platform`, `external`, `none` | Where the write key lives. platform: in the hub's CI platform, checked by a probe (on Bitbucket Pipelines only with a reason); external: issued by an external vault over OIDC; none: risk accepted, needs a reason. Default: platform. |
| `security.reason` | string | Why the write key cannot be isolated. Required with write_isolation: none, and on Bitbucket Pipelines with platform, where it states the deployment permissions (Premium) that keep the write key on the default branch, which touchmark cannot read. |
| `security.private_targets_in_public_hub` | one of `skip`, `deliver` | What a public hub does with non-public targets: skip them, or deliver and let their names reach public CI logs. Default: skip. |
| `providers` | list of objects | Platforms this hub delivers to. Targets refer to them by id. |
| `providers[].id` | string, required | Name targets.yml uses for this provider, and the &lt;ID&gt; of its TOUCHMARK_&lt;ID&gt;_\* variables. |
| `providers[].type` | one of `github`, `gitlab`, `gitea`, `forgejo`, `bitbucket`, required | github: github.com, GHE.com or GitHub Enterprise Server; gitlab; gitea; forgejo; bitbucket: Bitbucket Cloud (plan, distribute and doctor; setup does not set up a hub on it yet). |
| `providers[].url` | string | Web URL of the instance. Default for github: https://github.com; for gitlab: https://gitlab.com; for bitbucket: https://bitbucket.org, the only url it takes without api_url. Required for gitea and forgejo. A target targets.yml names by web URL belongs to the provider whose url it lies under. |
| `providers[].api_url` | string | API URL, when it cannot be derived from url. |
| `providers[].ca_file` | string | Hub file with extra CA certificates for this instance. |
| `providers[].writer` | string, up to 255 characters | The account that writes to targets, as the platform names it: on GitHub the App's bot, &lt;slug&gt;[bot]; on GitLab, Gitea and Forgejo the username of the service account or bot user; on Bitbucket the bot account's UUID in braces, {…}. |
| `providers[].known_authors` | list of strings, each up to 255 characters | Former writer accounts whose pull requests still count as this hub's own. |
| `providers[].automation_accounts` | list of strings, each up to 255 characters | Accounts whose closing of a pull request is automatic, not a team's decision. |
| `providers[].sign` | one of `auto`, `always` | auto: sign where it costs nothing and where target rules require it; always: sign every commit. Default: auto. |
| `providers[].limits` | object | Overrides of the platform's rate defaults; 0 keeps the default. |
| `providers[].limits.writes_per_minute` | integer ≥ 0 | Writes in any minute (HTTP writes and pushes). |
| `providers[].limits.writes_per_hour` | integer ≥ 0 | Writes in any hour. |
| `providers[].limits.reads` | integer ≥ 0 | Targets inspected at once. |
| `providers[].limits.git_reads` | integer ≥ 0 | Git fetches at once. |
| `providers[].limits.reads_per_minute` | integer ≥ 0 | API reads in any minute. |
| `providers[].limits.comments_per_minute` | integer ≥ 0 | Comments in any minute. |
| `providers[].limits.min_interval` | string | Least time between two writes, in milliseconds or seconds, e.g. 250ms or 1s. |
| `sensitive_paths` | list of strings | Patterns added to the built-in list of paths highlighted in pull requests. |
| `packs` | map of pack to object | Optional metadata of packs, by pack name. |
| `packs.<pack>.description` | string | — |
| `packs.<pack>.requires` | list of strings, each up to 64 characters | Packs that must come before this one. They are added to every target that gets this pack. |
| `packs.<pack>.formerly` | list of strings, each up to 64 characters | Former names of this pack. Their history counts as this pack's own. |
| `writer` | string, up to 255 characters | Single-provider shorthand: the writer account, as the platform names it (on GitHub the App's bot, &lt;slug&gt;[bot]). Without platform, the provider's type and URL come from the CI environment, or locally from the hub's origin remote. Not allowed with providers. |
| `sign` | one of `auto`, `always` | Single-provider shorthand: commit signing. Not allowed with providers. |
| `platform` | one of `github`, `gitlab`, `gitea`, `forgejo`, `bitbucket` | Single-provider shorthand: the provider type, which is also its id. Not allowed with providers. |
| `base_url` | string | Single-provider shorthand: the provider URL, for self-managed instances. Needs platform; not allowed with providers. |

<!-- end generated -->

See [Deliver to several platforms](../guide/providers.md) for `providers`, and
[Security model](../concepts/security.md) for `security`.
