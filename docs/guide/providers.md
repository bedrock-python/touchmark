# Deliver to several platforms

One hub can deliver to GitHub, GitLab, Gitea and Forgejo at once, including
self-managed instances. Each platform is a **provider** in `hub.yml`, with its own reader
and writer; `targets.yml` names repositories by provider. Bitbucket Cloud is a provider
too, delivered to from a hub on another platform: see [Bitbucket Cloud](#bitbucket-cloud).

## One provider: the shorthand

A hub that delivers only to the platform its CI runs on needs no list: `writer` and
`sign` at the top level of `hub.yml` describe that one provider. Its type and URL come
from the CI (`GITHUB_SERVER_URL`, `CI_SERVER_URL`); when you run touchmark yourself, from
the hub's origin remote, if that is github.com, a `*.ghe.com` host or gitlab.com.

```yaml
version: 1
id: acme-eng
writer: acme-assets-write[bot]
```

For a self-managed instance, name it so that touchmark also knows it outside CI: set
`platform` and `base_url` next to `writer`, or list it under `providers`. The provider's
id is then the platform's type (`github`, `gitlab`, `gitea`, `forgejo`), and its
variables keep their short names (`TOUCHMARK_READ_TOKEN`; `TOUCHMARK_GITLAB_READ_TOKEN`
works too).

```yaml
version: 1
id: acme-eng
platform: gitlab
base_url: https://gitlab.example.com
writer: touchmark-writer
```

## Several providers

```yaml
version: 1
id: acme-eng
providers:
  - id: gh
    type: github                    # github.com, GHE.com or GitHub Enterprise Server
    url: https://github.com
    writer: acme-assets-write[bot]
  - id: corp
    type: gitlab
    url: https://gitlab.example.com # a subpath works: https://example.com/gitlab
    ca_file: certs/corp-ca.pem      # a file in the hub, for git and the HTTP client
    writer: touchmark-writer
    known_authors: [group_42_bot_8f3c]
    automation_accounts: [stale-bot]
    limits: { writes_per_minute: 120 }
  - id: cb
    type: forgejo
    url: https://codeberg.org       # required for gitea and forgejo
    writer: acme-touchmark
```

- `id` is how `targets.yml` names the provider, and the `<ID>` of its variables:
  upper-cased, with `-` as `_`.
- `api_url` is needed only when it cannot be derived: `api.github.com`, `<url>/api/v3`
  for GitHub Enterprise Server, `/api/v4` for GitLab, `/api/v1` for Gitea and Forgejo,
  `https://api.bitbucket.org/2.0` for Bitbucket Cloud.
- `known_authors` are former writers whose pull requests stay the hub's own: a replaced
  App, a recreated group access token's bot, the account of a multi-gitter setup.
  touchmark never adds anyone there itself.
- `automation_accounts` are bots whose closing of a pull request is not a team's decision
  (stale bots): their closes are not declines.
- `sign` and `limits` are per provider.
- In a hub pull request, `providers` and `ca_file` are read from the default branch: a
  pull request cannot point the read key at a new host. New or changed providers are
  planned without credentials until the change is merged.

## Targets on several providers

```yaml
# targets.yml
version: 1
defaults:
  provider: gh                     # entries that name no provider
  packs: [agents]
targets:
  - repo: acme/billing             # gh, the default
  - repo: corp:platform/api        # <provider>:<path>
  - repo: https://gitlab.example.com/platform/web   # corp: the URL is under its url
  - provider: corp
    group: platform                # with its subgroups; subgroups: false turns them off
    topics: [python]
  - org: https://codeberg.org/acme # cb:acme
exclude:
  - corp:platform/sandbox
  - corp:platform/legacy/**        # a subgroup and everything beneath it
```

A target's provider is, in order: the entry's `provider`, the `<provider>:` prefix,
`defaults.provider`, the only provider. When that leaves a choice, `check` fails.

A target written as a web URL takes the provider whose `url` it lies under, matched by
scheme, host, port and path (`https://example.com/gitlab/platform/api` is under a
provider at `https://example.com/gitlab`); exactly one must match. touchmark rewrites
it as `<provider>:<path>` before anything else reads it. See
[targets.yml](../reference/targets.md#web-urls).

Everywhere a single repository is named — in output, in `--only`, in
`.touchmark/operations.yml` — it is `<provider>:<path>`. Inside a run, a repository is
identified by its host and immutable id, so one repository named by two entries, or by
two providers on the same host, is delivered to once, with the packs of every entry.

## Credentials per provider

```text
TOUCHMARK_GH_READ_APP_ID    TOUCHMARK_GH_READ_APP_KEY
TOUCHMARK_GH_WRITE_APP_ID   TOUCHMARK_GH_WRITE_APP_KEY
TOUCHMARK_CORP_READ_TOKEN   TOUCHMARK_CORP_WRITE_TOKEN   TOUCHMARK_CORP_SIGNING_KEY
TOUCHMARK_CB_READ_TOKEN     TOUCHMARK_CB_WRITE_TOKEN
```

Keep every write key next to the others, in the protected environment (GitHub) or the
protected, environment-scoped variable (GitLab), and add each to the probe: `check`
fails when a GitHub workflow hands touchmark a write key the probe does not test.

## Delivering to Gitea or Forgejo from GitHub or GitLab

Gitea and Forgejo Actions cannot keep a secret to the default branch. A hub whose CI runs
on GitHub or GitLab can still deliver to them: add them as providers, and keep their
write token in the GitHub environment or the GitLab protected variable, where the probe
covers it.

## Bitbucket Cloud

A provider of type `bitbucket` is Bitbucket Cloud at `https://bitbucket.org` (leave `url`
out). `plan`, `distribute` and `doctor` work on it from a hub on GitHub or GitLab, or
wherever you run touchmark; a hub whose own CI is Bitbucket Pipelines is not supported
yet, and `setup` has nothing for it. Bitbucket Data Center is not supported.

```yaml
# hub.yml
providers:
  - id: bb
    type: bitbucket
    writer: "{3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d}"   # the writer account's UUID, in braces
```

**Accounts.** Make two bot accounts (Atlassian accounts that only touchmark uses), each
with an API token; app passwords are gone, and repository, project and workspace access
tokens are not supported, since they cannot tell who they are. A token's scopes do not
narrow the repositories it reaches, the account's permissions do: that is why the writer
is an account of its own.

| | Reader | Writer |
|---|---|---|
| Variable | `TOUCHMARK_BB_READ_TOKEN` | `TOUCHMARK_BB_WRITE_TOKEN` |
| Scopes (all `…:bitbucket`) | `read:user`, `read:workspace`, `read:repository`, `read:pullrequest` | the reader's, plus `write:repository` and `write:pullrequest`; no admin or delete scope |
| Repositories | read access to the targets | write access to the targets only (directly, through a group or the project); not admin, and no access to the hub |

API tokens expire within a year, and Bitbucket's API shows neither their expiry nor
their scopes: `doctor` reports both as unknown, so keep a reminder to rotate them.

- **Accounts are UUIDs.** Bitbucket finds no account by its nickname, so `writer`,
  `known_authors` and `automation_accounts` name accounts by their UUID in braces, as
  `GET https://api.bitbucket.org/2.0/user` shows it.
- **Targets.** `org` is a workspace, `repo` is `workspace/repository`, and web URLs such as
  `https://bitbucket.org/acme/billing` work. Bitbucket has no topics: `check` refuses
  `topics` on a Bitbucket entry; select repositories with `match` (`acme/svc-*`) or list
  them with `repo`.
- **No labels.** Bitbucket pull requests have none: `pr.labels` sets nothing there, and
  touchmark records no label as set.
- **Drafts** are Bitbucket's own draft flag (`pr.draft`).
- **Closing.** touchmark closes its own pull request by declining it, after writing the
  close into its description in the same edit. Bitbucket can never reopen a declined pull
  request, nor change it: a pull request touchmark closed stays closed, and the next
  proposal is a new one.
- **Declines** are remembered without writing to the declined pull request: while a pull
  request is open, its marker records the state of the opt-in file it is proposed under
  (touchmark brings it up to date when the file changes), and a person's decline holds
  while the opt-in file is parsed the same. touchmark leaves no comment on it; the report
  says `declined`. To have declined content proposed again, edit `packs` or `ignore`, or
  add a `forget_declines` entry, which acts while it is present on Bitbucket (see
  [Memory of declined pull requests](../concepts/memory.md#on-bitbucket-cloud)).
- **No tick boxes, no HTML.** *Rebuild this branch* and *Propose this content again* rest
  on HTML comments, which Bitbucket shows as text: descriptions there carry neither. A
  paused pull request asks for a `recreate` entry in `.touchmark/operations.yml` instead,
  and the footnote names `forget_declines`. The files the repository made its own are a
  plain section, not a folded one. Keep `pr.intro_file` free of HTML too: Bitbucket shows
  it as text.
- **Branch restrictions** are readable with admin rights only, which the writer should
  not have: `doctor` shows `rules` as unknown, and a push a restriction refuses
  ("Permission denied to update branch") is `blocked:rules:protected-branch`.
- **The marker.** Bitbucket shows HTML comments in descriptions as text, so the marker
  is a Markdown link reference definition there, which renders as nothing:
  `[touchmark]: # "touchmark:v1 hub=… fp=… stream=sync key=… data=…"`, the same payload as
  the comment elsewhere. An edit on the website may add backslashes before punctuation
  and Windows line endings; touchmark reads the marker either way.
- **Commit author.** Bitbucket links a commit to the account one of whose confirmed
  addresses is the author's: touchmark authors the writer's commits with its account's
  primary confirmed address, and with `<uuid>@touchmark.invalid` (an address that can
  belong to no one) when the account has none.
- **Pace.** Bitbucket allows an account about 1 000 API requests an hour, so touchmark
  reads 15 times a minute by default, with two targets at once, and writes at most once a
  second; `limits` overrides it.

## Moving

- **A target moves to another provider on the same host** (a new App, a new group): add
  the old writer to `known_authors` of the new provider, so its open pull request stays
  the hub's.
- **The hub moves** to another repository or host: list the old fingerprint, `host/id`,
  under `previous_fingerprints`; the next write gives each marker the new one.
- **The hub changes its `id`**: the sync branch's name changes; put the old one in
  `branch_aliases`. Open pull requests and the memory of declines stay, because the hub is
  known by its fingerprint, not its `id`.
