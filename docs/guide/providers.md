# Deliver to several platforms

One hub can deliver to GitHub, GitLab, Gitea and Forgejo at once, including
self-managed instances. Each platform is a **provider** in `hub.yml`, with its own reader
and writer; `targets.yml` names repositories by provider. Bitbucket Cloud is a provider
too, and can host the hub: see [Bitbucket Cloud](#bitbucket-cloud).

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
out). `plan`, `distribute` and `doctor` work on it from a hub on GitHub or GitLab, from
a hub on Bitbucket itself, whose CI is Bitbucket Pipelines (see
[A hub on Bitbucket Cloud](../getting-started/bitbucket.md)), or wherever you run
touchmark; `setup` has nothing for it yet. Bitbucket Data Center is not supported.

```yaml
# hub.yml
providers:
  - id: bb
    type: bitbucket
    writer: "{3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d}"   # the writer account's UUID, in braces
```

**Accounts.** Make two bot accounts (Atlassian accounts that only touchmark uses), each
with an API token; app passwords are gone, and repository, project and workspace access
tokens are not supported as reader or writer, since they cannot tell who they are (a
hub on Bitbucket uses one for its own repository only, `TOUCHMARK_PIPELINES_TOKEN`). A token's scopes do not
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

## Azure DevOps

A provider of type `azure-devops` is one organization of Azure DevOps Services, and its
`url` names it: `https://dev.azure.com/<organization>` (one provider per organization;
the old `https://<organization>.visualstudio.com` form and Azure DevOps Server are not
supported). `plan`, `distribute` and `doctor` work on it from a hub on GitHub or GitLab,
or wherever you run touchmark; a hub whose own CI is Azure Pipelines is not supported
yet, and `setup` has nothing for it.

```yaml
# hub.yml
providers:
  - id: ado
    type: azure-devops
    url: https://dev.azure.com/acme
    writer: 3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d   # the writer user's identity id
```

**Accounts.** Use two users that only touchmark uses, each with a personal access token
(PAT) for this organization only: service principals of Microsoft Entra ID are not
supported yet. Global PATs (for all organizations) stop working on 2026-12-01.

| | Reader | Writer |
|---|---|---|
| Variable | `TOUCHMARK_ADO_READ_TOKEN` | `TOUCHMARK_ADO_WRITE_TOKEN` |
| Scopes | Code (read) | Code (read & write) |
| Repositories | read access to the targets | Contribute, Create branch and Contribute to pull requests on the targets only; no administration, and no access to the hub |

A PAT cannot read its own expiry or scopes: `doctor` reports both as unknown, so keep a
reminder to renew the tokens before they expire.

- **Accounts are identity ids.** Display names are neither unique nor stable, so
  `writer`, `known_authors` and `automation_accounts` name users by their identity id, a
  GUID: `GET https://dev.azure.com/<organization>/_apis/connectionData` with the user's
  token shows it as `authenticatedUser.id`.
- **Targets.** A repository is `<project>/<repository>`; web URLs such as
  `https://dev.azure.com/acme/Billing/_git/api` work. `org` names the whole organization
  (`org: acme`, or its URL `https://dev.azure.com/acme`); select a project's repositories
  with `match` (`Billing/*`). Azure DevOps has no topics: `check` refuses `topics` on its
  entries. Disabled repositories are skipped. Project and repository names with spaces
  can be reached through `org` only: `targets.yml` paths take letters, digits, `.`, `-`
  and `_`.
- **The marker lives in a pull request property** (`touchmark.marker`), not in the
  description, which Azure DevOps limits to 4 000 characters: the description is short,
  its lists are cut to fit, and the marker still records every change. Editing the
  description in the web UI leaves the marker as it is, and a marker pasted into the
  description is ignored. The property is not protected, though: anyone who may
  contribute to pull requests in the project, project Readers included by default, may
  be able to write it through the API (see the
  [threat model](../project/threat-model.md)). touchmark writes the property right after
  it opens a pull request, trying a few times; should it still fail, it abandons the new
  pull request and opens another, so that none stays without its marker.
- **People's text in the description** is theirs: a close or a refresh of the marker
  writes the property alone and sends no description, so a description people filled up
  to the limit never stops touchmark from closing its pull request.
- **Labels** are added by name, as `pr.labels` says. **Drafts** are Azure DevOps' own
  draft flag (`pr.draft`).
- **Closing.** touchmark closes its own pull request by abandoning it, after it stored the
  closing marker. It never reactivates an abandoned pull request, nor changes it: a pull
  request touchmark closed stays closed, and the next proposal is a new one.
- **Declines** are remembered without writing to the abandoned pull request, as on
  Bitbucket Cloud: while a pull request is open its marker records the state of the
  opt-in file, and a person's decline holds while that file is parsed the same. touchmark
  leaves no comment on it; the report says `declined`. To have declined content proposed
  again, edit `packs` or `ignore`, or add a `forget_declines` entry, which acts while it
  is present (see [Memory of declined pull requests](../concepts/memory.md#on-bitbucket-cloud)).
- **Comments** are threads touchmark closes at once, so that they never wait for a
  resolution.
- **Branch policies** refuse direct pushes to the branches they protect, and the writer
  reads none upfront: `doctor` shows `rules` as unknown; a push a policy refuses
  (TF402455) is `blocked:rules:policy`, one the writer lacks a permission for (TF401027)
  `blocked:permission:push`. The writer's permissions are read with the Has Permissions
  API when the token may use it; when it may not, `doctor` shows `access` as unknown and
  the first push or pull request meets a missing permission.
- **Commit author.** touchmark authors the writer's commits with its sign-in address when
  Azure DevOps shows one, else with `<identity id>@touchmark.invalid`.
- **Pace.** Azure DevOps meters a user's load over five minutes and announces it in its
  `X-RateLimit-*` headers, which pause the provider before it delays requests, and in
  `Retry-After` on requests it delayed, which pauses the provider for that long; touchmark
  reads 120 times a minute with four targets at once and writes at most 30 times a minute
  by default; `limits` overrides it.
- **The reader needs a token.** Azure DevOps sends anonymous reads of files (the Trees
  API) and of pull request properties to its sign-in page even in public projects, so a
  plan without the reader's token fails on Azure DevOps targets.
- **Cost.** Listing a target's pull requests reads the marker of every open one and of the
  60 newest abandoned and completed ones (touchmark remembers the 50 newest closed pull
  requests); older closed pull requests count as without a marker.

What only a live organization can confirm, and the driver assumes from the REST
reference: the size limit of pull request properties and whether they survive an
abandon; whether labels in a create request are applied (touchmark adds missing ones
after it); the error of a duplicate pull request (TF401179); whether `closedBy` is set
on abandoned pull requests; the order of pull request listings and where they cut
descriptions; the answer to a refused PAT (401, a sign-in redirect or a 203 page); the
exact texts of push refusals; the `mode` strings of the Trees API; how disabled and
renamed repositories answer; who can write pull request properties (whether project
Readers, through "Contribute to pull requests", can set or change `touchmark.marker`);
and whether `Retry-After` comes on answers that went through.

## Moving

- **A target moves to another provider on the same host** (a new App, a new group): add
  the old writer to `known_authors` of the new provider, so its open pull request stays
  the hub's.
- **The hub moves** to another repository or host: list the old fingerprint, `host/id`,
  under `previous_fingerprints`; the next write gives each marker the new one.
- **The hub changes its `id`**: the sync branch's name changes; put the old one in
  `branch_aliases`. Open pull requests and the memory of declines stay, because the hub is
  known by its fingerprint, not its `id`.
