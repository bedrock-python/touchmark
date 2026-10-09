# A hub on Azure DevOps

A hub in Azure Repos of Azure DevOps Services, whose CI is Azure Pipelines: the template's
`azure-pipelines.yml` checks every pull request to the hub and delivers from the default
branch. Azure DevOps Server is not supported. A hub on any other platform can also
deliver to Azure DevOps as one of its providers: see
[Azure DevOps](../guide/providers.md#azure-devops).

!!! warning "Not verified on a live Azure DevOps yet"
    The Azure Pipelines support is built from Microsoft's documentation and tested against
    a stand-in of its API, not on a live organization. The points a first live run must
    confirm are listed under [Assumed, not yet seen live](#assumed-not-yet-seen-live).

`touchmark setup` has nothing for Azure DevOps yet: the steps below are by hand.

## Where the write key can live

Azure Pipelines gives a stage the variables of a variable group only when the stage links
it, and checks the group's **approvals and checks** before the stage starts. But any
branch's copy of `azure-pipelines.yml` can link the group too, so the write key stays on
the default branch only when a **Branch control** check on the group admits the default
branch alone: the stage of any other branch then fails its checks and never starts
([checks](https://learn.microsoft.com/en-us/azure/devops/pipelines/process/approvals)). The
check is free on every plan, but a job's token is not known to read it, so touchmark
cannot check it from the run:

| `hub.yml` | What keeps the write key |
|---|---|
| `write_isolation: platform` and a `reason` that states the check | the variable group `touchmark-distribute` with a Branch control check that admits the default branch only; you state it, and `doctor --hub-token` verifies it |
| `write_isolation: none` and a `reason` | nothing but who may push: every branch of the hub can read the write key |
| `write_isolation: external` | an external secret store that releases the key to the default branch only |

```yaml
# hub.yml of a hub on Azure DevOps
version: 1
id: acme-eng
writer: 3f2a8d4e-1b6c-4f0a-9e7d-5c2b1a0f9e8d   # the writer user's identity id
security:
  write_isolation: platform
  reason: "Azure: Branch control admits refs/heads/main only to touchmark-distribute"
```

Under `platform` without a `reason`, which is how the template ships, `distribute` and
`doctor` refuse to run on Azure Pipelines (exit 2). In every run the `probe` stage fails
when it sees a write key in a variable of the pipeline itself, which every branch's runs
get.

## Steps

1. **Create the hub.** In your project choose *Repos → Import repository*, and give the
   URL of [the template](https://github.com/bedrock-python/engineering-assets-template),
   `https://github.com/bedrock-python/engineering-assets-template.git`. Then choose
   *Pipelines → New pipeline → Azure Repos Git*, the hub, *Existing Azure Pipelines YAML
   file* and `/azure-pipelines.yml`, and save the pipeline without running it.
2. **Create two bot users** in the organization (with Basic access), each with a personal
   access token for this organization only:
    - the *reader*, with read access to your target repositories and the scope
      *Code (read)*;
    - the *writer*, with *Contribute*, *Create branch* and *Contribute to pull requests*
      on the targets only (not administration, and no access to the hub), and the scope
      *Code (read & write)*.
   Tokens expire: note when, and renew them before. See
   [Azure DevOps](../guide/providers.md#azure-devops) for the details.
3. **Store the keys**, all secret:
    - the pipeline's variable `TOUCHMARK_READ_TOKEN`, the reader's token (*Edit →
      Variables*, *Keep this value secret*; leave *Let users override this value when
      running this pipeline* off);
    - the variable group `touchmark-distribute` (*Pipelines → Library*) with the writer's
      token as `TOUCHMARK_WRITE_TOKEN`, and nowhere else: not as a variable of the
      pipeline. Under the group's *Pipeline permissions*, allow the hub's pipeline alone;
      under *Approvals and checks*, add **Branch control** with *Allowed branches*
      `refs/heads/main` (your default branch), *Verify branch protection* on, and *Allow
      unknown status* off.
   The hub delivers to Azure DevOps with these one-provider names; with `providers` in
   `hub.yml` they carry the provider's id (`TOUCHMARK_ADO_WRITE_TOKEN`), and the step
   template `.azure-pipelines/touchmark.yml` must map those names instead.
4. **Create the environment** `touchmark-distribute` (*Pipelines → Environments*, with no
   resources). `distribute` and `doctor` run as deployment jobs to it, and touchmark
   refuses them in any other job. Add its checks **Exclusive lock**, so that two runs
   never overlap, and the same **Branch control**.
5. **Protect the hub.** Under *Project settings → Repositories → the hub → Policies*, on
   the default branch:
    - add **Build validation** with the hub's pipeline, required. That policy is what runs
      the pipeline for pull requests: Azure Repos ignores `pr:` triggers;
    - add your maintainers as automatically included reviewers, **required**, and require
      a minimum number of reviewers with *Reset all approval votes* when there are new
      changes and *Prohibit the most recent pusher from approving their own changes*.
      A pull request build runs its own branch's YAML with the job access token, so a
      policy that any vote satisfies is not enough (step 6).
   Azure DevOps reads none of the template's CODEOWNERS files: delete them with the other
   platforms' CI files. Under *Organization settings → Pipelines → Settings* turn on
   *Limit variables that can be set at queue time*, *Limit job authorization scope to
   current project* and *Protect access to repositories in YAML pipelines*
   ([below](#what-the-pipeline-does)).
6. **Optionally, let the pipeline comment.** `plan` keeps its report in a comment of the
   hub pull request only when `<project> Build Service (<organization>)`, the identity of
   the job access token, may *Contribute to pull requests* on the hub (*Project settings →
   Repositories → the hub → Security*). That right also lets the job vote on pull
   requests, and a pull request build runs the YAML of the branch under review: grant it
   only with your maintainers as required reviewers (step 5). Without it the report is
   still in the run's summary and artifacts, and `plan` says the comment was not posted.
7. **Describe your hub.** In `hub.yml` set `id`, `writer` to the writer user's identity id
   (`GET https://dev.azure.com/<organization>/_apis/connectionData` with its token shows
   it as `authenticatedUser.id`), and `security` as above. List your repositories in
   `targets.yml` (`org: <organization>` with `match: ["<project>/*"]`, or
   `repo: <project>/<repository>`; Azure DevOps has no topics).
8. **Open a pull request** and check the `probe`, `check` and `plan` stages. The first run
   asks you to permit the pipeline to use the variable group and the environment. The
   report files are the run's artifacts, the run's summary shows them, and with step 6
   `plan` keeps the report in one comment of the pull request.
9. **Merge it.** The run of the default branch does `probe`, then `distribute`, which opens
   a pull request in every repository that has [opted in](opt-in.md). The schedules run
   `distribute` every day and `doctor` every Monday, and *Run pipeline* asks which of the
   two to run.

## What the pipeline does

| Run | Stages |
|---|---|
| pull request to the hub (Build validation) | `probe`, then `check` and `plan --strict --comment` |
| push to `main` or `master`, the schedule "touchmark distribute", *Run pipeline* with `distribute` | `probe`, then `distribute` in the environment `touchmark-distribute`, with the variable group |
| the schedule "touchmark doctor", *Run pipeline* with `doctor` | `probe`, then `doctor`, likewise |

- **The image.** An Azure container job needs bash, glibc and Node.js in the image, and
  touchmark's image is Alpine with busybox, so the step template runs it with docker on
  the agent (`ubuntu-24.04`), as the agent's user, without capabilities, with the image
  pinned by digest in `.azure-pipelines/touchmark.yml` alone. The container gets `CI`,
  `TF_BUILD`, the `BUILD_*`, `SYSTEM_*`, `ENVIRONMENT_*` and `TOUCHMARK_*` variables of the
  step and the proxy settings, by name, and the sources directory. A self-hosted agent
  needs docker.
- **The keys.** A secret variable reaches a step only when the step maps it: the step
  template maps `System.AccessToken` into `SYSTEM_ACCESSTOKEN`, `TOUCHMARK_READ_TOKEN`
  always, and `TOUCHMARK_WRITE_TOKEN` in the probe and the deployment jobs. A secret the
  run does not define stays `$(NAME)` in the step's environment; the step leaves it out.
- **The default branch.** No variable names it, so touchmark reads it with the job access
  token and refuses `distribute` and `doctor` on any other branch, or when it cannot read
  it. The triggers and the stages' conditions name `main` and `master`: add your default
  branch if it has another name.
- **The environment.** `distribute` and `doctor` run only in a deployment job to the
  environment `touchmark-distribute` (`Environment.Name`).
- **Time.** A Microsoft-hosted job of a free private project ends after 60 minutes: the
  `distribute` job has `timeoutInMinutes: 60`, and touchmark starts no target after 50
  (`--deadline 50m`); the next run picks up what is left. With paid parallel jobs, raise
  both.
- **Reports.** The step template uploads `touchmark-report.md` and `touchmark-doctor.md`
  to the run's summary and publishes the report files as an artifact.
- **The plan comment** (with step 6) is written with the job access token, as a new thread touchmark
  closes at once, so that a policy that requires resolved comments never waits on it. It
  is plain Markdown, its marker a link reference definition,
  `[touchmark-plan]: # "touchmark plan: <hub id>"`. touchmark learns its own identity
  (`connectionData`) and edits only its own comment.
- **Variables set at queue time** reach every step of the run, also the steps that hold
  the write key: `BASH_ENV` makes the step's bash run a command, `DOCKER_HOST` would send
  the container, and the keys it gets, to another daemon (the step template unsets the
  `DOCKER_*` variables, but bash reads `BASH_ENV` before its first line). Keep *Limit
  variables that can be set at queue time* on, and the pipeline's variables not
  overridable: then a run can be given only the variables the YAML declares settable,
  and the template declares none. `doctor --hub-token` fails while the setting is off.
  For more, add an **Approval** check on the environment `touchmark-distribute`, or limit
  who may *Queue builds* on the hub's pipeline.
- **Logging commands.** The Azure agent runs any `##vso[…]` it finds in a step's output:
  under Azure Pipelines touchmark breaks every `##vso[` it prints, so that nothing a
  platform or a target says becomes a command.
- **The fingerprint** of the hub is `dev.azure.com/<repository id>`, the id lowercase:
  `dev.azure.com/0b7e5a2c-9d4f-4e1b-8a3c-6f5d2e1c0b9a`. Repository ids are unique across
  organizations, so the fingerprint names no organization, and survives renames.

## Check it

`touchmark doctor --hub-token --hub-fp dev.azure.com/<repository id>`, run from a clone of
the hub, reads where the keys live with the token in `TOUCHMARK_HUB_TOKEN`: a maintainer's
personal access token with *Code (read)*, *Build (read)* and *Variable Groups (read)*.
`GET https://dev.azure.com/<organization>/<project>/_apis/git/repositories/<repository>`
shows the repository's id. A hub without `providers` takes its organization from the
clone's origin (`https://dev.azure.com/<organization>/…`, or its SSH form); with
providers of several organizations, the origin picks the hub's.

- A write key in a variable of the hub's pipelines fails.
- One in a variable group is `ok` when a Branch control check admits the default branch
  alone (`refs/heads/<branch>`) and verifies its protection; it fails without such a
  check, warns in another group than `touchmark-distribute`, and is unknown when the
  checks could not be read.
- A group open to every pipeline of the project warns, and fails when no check verifies
  branch protection: Branch control compares only the run's branch name, which another
  repository's pipeline on its own default branch shares.
- A key that is not secret warns.
- The check `pipeline-settings` fails while *Limit variables that can be set at queue
  time* is off, and warns while the job access token reaches other projects or
  repositories.

## Assumed, not yet seen live

- The job access token, mapped into `SYSTEM_ACCESSTOKEN`, reads the hub repository
  (default branch, refs), its project (visibility) and `connectionData`
  (`authenticatedUser.id`, the build service's identity), with a Bearer header.
- The project's build service may create and edit pull request threads once given
  *Contribute to pull requests*, a thread created with `status: closed` stays closed,
  and Azure DevOps renders a link reference definition in a comment as nothing.
- The checkout of a pull request build fetches with the job's credentials, and the plan
  stage's `git fetch` of the target branch with the job access token in git's
  environment works.
- A Branch control check on a variable group fails the stage of another branch at once,
  for a run of any trigger; Check Configurations (a preview API) returns the check with
  `$expand=settings` as Microsoft's Terraform provider writes it
  (`evaluatebranchProtection`, `allowedBranches`, `ensureProtectionOfBranch`,
  `allowUnknownStatusBranch`), and a maintainer's token with *Build (read)* reads it.
- `Environment.Name` holds the environment's name in a deployment job, and the
  variables of a variable group linked by the job reach its steps when they map them.
- A secret variable the run does not define stays `$(NAME)` in the environment of a step
  that maps it.
- docker on `ubuntu-24.04` runs the image as the agent's user with the sources directory
  mounted, and the image's git reads the checkout made by the agent.
- The agent finds a logging command anywhere in a line of output, which is why touchmark
  breaks it wherever it appears.
- Build General Settings answers `enforceSettableVar`, `enforceJobAuthScope` and
  `enforceReferencedRepoScopedToken` to a maintainer's token with *Build (read)*.
- A run queued through the Runs API for `refs/heads/<default branch>` with the version of
  a commit that is not on that branch: whether Azure refuses it, or Branch control lets
  it through on the branch's name. Until that is seen, keep who may queue runs of the
  hub's pipeline to maintainers, or add an Approval check on the environment.
- The vote of the build service on a pull request counts toward a minimum number of
  reviewers, which is why step 5 asks for required reviewers.

Next: [opt a repository in](opt-in.md).
