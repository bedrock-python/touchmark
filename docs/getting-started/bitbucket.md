# A hub on Bitbucket Cloud

A hub hosted on Bitbucket Cloud, whose CI is Bitbucket Pipelines: the template's
`bitbucket-pipelines.yml` checks every pull request to the hub and delivers from the
default branch. Bitbucket Data Center is not supported.

!!! warning "Not verified on a live Bitbucket yet"
    The Bitbucket support is built from Atlassian's documentation and tested against a
    stand-in of its API, not on a live workspace. The points a first live run must
    confirm are listed under [Assumed, not yet seen live](#assumed-not-yet-seen-live).

`touchmark setup` has nothing for Bitbucket yet: the steps below are by hand.

## Where the write key can live

Bitbucket Pipelines gives a step the variables of a deployment environment only when the
step names it (`deployment: touchmark-distribute`). But any branch's copy of
`bitbucket-pipelines.yml` can name it too, so the write key stays on the default branch
only when the environment's **deployment permissions** stop the steps of other branches
([they pause](https://support.atlassian.com/bitbucket-cloud/docs/set-up-and-monitor-deployments/)).
Those permissions are a **Premium** feature, and Bitbucket's API does not show them, so
touchmark cannot check them. Choose by plan:

| Plan | `hub.yml` | What keeps the write key |
|---|---|---|
| Premium | `write_isolation: platform` and a `reason` that states the restriction | the deployment `touchmark-distribute` admits the default branch only (or only admins); you state it, touchmark cannot read it |
| Free, Standard | `write_isolation: none` and a `reason` | nothing but who may push: every branch of the hub can read the write key; protect the branches and keep write access to the hub tight |
| any | `write_isolation: external` | an external secret store that releases the key to the default branch only |

```yaml
# hub.yml of a hub on Bitbucket Premium
version: 1
id: acme-eng
writer: "{3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d}"   # the writer account's UUID
security:
  write_isolation: platform
  reason: "Bitbucket Premium: only main may deploy to touchmark-distribute"
```

Under `platform` without a `reason`, which is how the template ships, `distribute` and
`doctor` refuse to run on Bitbucket Pipelines (exit 2): a hub on Free or Standard is
never taken for isolated. On every plan the `probe` step fails a pipeline that sees a
write key in a repository or workspace variable.

## Steps

1. **Create the hub.** In your workspace choose *Create → Repository → Import
   repository*, and give the URL of
   [the template](https://github.com/bedrock-python/engineering-assets-template),
   `https://github.com/bedrock-python/engineering-assets-template.git`. Keep it private
   unless the hub may be public. Turn Pipelines on under *Repository settings →
   Pipelines → Settings*.
2. **Create two bot accounts**, Atlassian accounts that only touchmark uses, each with an
   API token (Atlassian account settings → *Security → API tokens*, with scopes):
    - the *reader*, with read access to your target repositories and the scopes
      `read:user`, `read:workspace`, `read:repository`, `read:pullrequest` (all
      `…:bitbucket`);
    - the *writer*, with write access to the targets only (not admin, and no access to
      the hub), and the reader's scopes plus `write:repository` and `write:pullrequest`.
   A token's scopes do not narrow the repositories it reaches, the account's permissions
   do. See [Bitbucket Cloud](../guide/providers.md#bitbucket-cloud) for the details.
3. **Create the hub's access token.** Under *Repository settings → Security → Access
   tokens* of the hub, create one with *Repositories: Read* and *Pull requests: Write*.
   Pipelines gives a step no API token of its own: touchmark reads the hub's default
   branch, its tip and visibility with this token, and keeps `plan`'s comment in the hub
   pull request with it. It reaches the hub alone, and every pipeline of the hub sees
   it, so give it nothing more; `Pull requests: Write` also lets it merge, so restrict
   who may merge into the default branch (step 5).
4. **Store the keys** under *Repository settings → Pipelines → Repository variables*
   and *→ Deployments*, all **Secured**:
    - repository variables: `TOUCHMARK_READ_TOKEN` (the reader's API token) and
      `TOUCHMARK_PIPELINES_TOKEN` (the hub's access token);
    - create the deployment environment `touchmark-distribute` and store the writer's
      API token as its variable `TOUCHMARK_WRITE_TOKEN`, and nowhere else: not as a
      repository or workspace variable;
    - on Premium, open the environment's settings and allow deployments from the
      default branch only (or by admins only).
   The hub delivers to Bitbucket Cloud with these one-provider names; with `providers`
   in `hub.yml` they carry the provider's id (`TOUCHMARK_BB_WRITE_TOKEN`).
5. **Protect the hub.** Under *Repository settings → Branch restrictions*, let no one
   push to the default branch directly and only your maintainers merge into it; keep the
   bot accounts and the hub's access token out of those lists. Bitbucket reads none of
   the template's CODEOWNERS files: replace `@acme/hub-maintainers` in them with your
   team, or delete them with the other platforms' CI files (`touchmark check` warns
   about the placeholder).
6. **Schedule the runs.** Under *Pipelines → Schedules*, create two schedules on the
   default branch: a daily one for the custom pipeline `distribute`, and a weekly one
   for `doctor`.
7. **Describe your hub.** In `hub.yml` set `id`, `writer` to the writer account's UUID
   in quotes (`GET https://api.bitbucket.org/2.0/user` with its token shows it), and
   `security` as above. List your repositories in `targets.yml` (`org: <workspace>` with
   `match`, or `repo: <workspace>/<repository>`; Bitbucket has no topics).
8. **Open a pull request** and check the `probe`, `check` and `plan` steps. `plan`
   keeps its report in one comment of the pull request, and the report files are the
   step's artifacts.
9. **Merge it.** The push pipeline of the default branch runs `probe`, then
   `distribute`, which opens a pull request in every repository that has
   [opted in](opt-in.md). *Run pipeline* runs the custom pipelines `distribute` and
   `doctor` by hand.

## What the pipelines do

| Pipeline | Steps |
|---|---|
| pull request to the hub | `probe`, `check`, `plan --strict --comment` |
| push to `main` or `master`, the custom pipeline `distribute` | `probe`, then `distribute` with `deployment: touchmark-distribute` |
| the custom pipeline `doctor` | `probe`, then `doctor` with `deployment: touchmark-distribute` |

- **The default branch.** Pipelines names no default branch, so touchmark reads it with
  `TOUCHMARK_PIPELINES_TOKEN` (a public hub answers without it) and refuses
  `distribute` and `doctor` on any other branch, or when it cannot read it. The push
  pipeline starts on `main` and `master`: add your default branch to its pattern if it
  has another name.
- **The deployment.** `distribute` and `doctor` run only in a step with
  `deployment: touchmark-distribute` (`BITBUCKET_DEPLOYMENT_ENVIRONMENT`). Bitbucket
  runs one deployment to an environment at a time, so two runs never overlap.
- **Time.** The `distribute` step may run 6 hours (`max-time: 360`); touchmark starts no
  target after 5 hours 30 minutes (`--deadline 5h30m`), and the next run picks up what is
  left.
- **Reports.** Bitbucket has no step summary or annotations: the report is in the step's
  log, and `touchmark-report.{md,json,jsonl}` and `touchmark-doctor.{md,json}` are the
  steps' artifacts (kept 14 days).
- **The plan comment** is written with the hub's access token. Bitbucket shows HTML as
  text, so the comment has no folded sections, and its marker is a Markdown link
  reference definition, `[touchmark-plan]: # "touchmark plan: <hub id>"`, which renders
  as nothing. An access token cannot tell its own account, so touchmark edits the newest
  comment with the marker that Bitbucket lets it edit, and writes a new one otherwise.
- **The fingerprint** of the hub is `bitbucket.org/<repository UUID>`, lowercase and
  without braces: `bitbucket.org/3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d`. `--hub-fp` and
  `previous_fingerprints` also take the UUID as `BITBUCKET_REPO_UUID` gives it, in
  braces.
- **Custom pipelines** accept variables when they run: anyone with write access to the
  hub can run them, through the website or the API, and a pipeline variable overrides a
  deployment variable. The template's scripts reference no variable, so the website asks
  for none. On Premium, deployment permissions also pause a run by someone not allowed
  to deploy.

## Check it

`touchmark doctor --hub-token`, run from a clone of the hub, reads where the keys live
with the token in `TOUCHMARK_HUB_TOKEN`: a one-off access token of the hub with
*Repositories: Read* and *Pipelines: Read*, or a maintainer's API token with
`read:repository:bitbucket` and `read:pipeline:bitbucket` (not the pipelines' own
token, which needs no Pipelines scope). It finds that a write key in a repository or workspace variable fails, one in the
deployment `touchmark-distribute` is unknown (the API does not show who may deploy; it
is `ok` when the environment admits only admins), and a variable that is not secured
warns. Workspace variables need a workspace administrator's token; without one they are
reported as not read.

## Assumed, not yet seen live

- Pipelines runs the step scripts with the image's shell, not its `touchmark`
  entrypoint, and the image's user (uid 65532) may write the report files into the
  clone. If not, set `run-as-user` on the steps.
- `git fetch origin` works in a pull request's step with Pipelines' own credentials.
- `BITBUCKET_DEPLOYMENT_ENVIRONMENT` holds the environment's name (`touchmark-distribute`).
- An access token with *Pull requests: Write* may comment on the hub's pull requests,
  and a `PUT` of another account's comment answers 403.
- Bitbucket renders a link reference definition in a comment as nothing.
- `restrictions.admin_only` of a deployment environment, which the API returns but does
  not document.

Next: [opt a repository in](opt-in.md).
