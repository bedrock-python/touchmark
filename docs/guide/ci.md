# Run it in CI

The [hub template](https://github.com/bedrock-python/engineering-assets-template) ships
the CI files for GitHub Actions, GitLab CI, Gitea and Forgejo Actions, Bitbucket
Pipelines and Azure Pipelines; each platform reads only its own. Copy them rather
than writing your own: the jobs, the probe and the environment fit together. This page
explains what they rely on.

Every job checks the hub out with its **whole history** (touchmark refuses a shallow
clone) and passes credentials **in the environment**, under the names touchmark reads.

## GitHub Actions

The Action runs touchmark from the image of its own release:

```yaml
- uses: actions/checkout@<commit> # v7.0.1
  with:
    fetch-depth: 0              # the hub's whole history is its ownership record
    persist-credentials: false
- uses: bedrock-python/touchmark@<commit> # vX.Y.Z
  with:
    command: plan
    strict: true
    comment: true
  env:
    GITHUB_TOKEN: ${{ github.token }}   # the hub channel: the hub repository only
    TOUCHMARK_READ_APP_ID: ${{ vars.TOUCHMARK_READ_APP_ID }}
    TOUCHMARK_READ_APP_KEY: ${{ secrets.TOUCHMARK_READ_APP_KEY }}
```

- **Pin it by commit**, with the version in a comment, and let Dependabot propose the next
  one. There is no moving `v1` tag.
- **Credentials are never inputs.** Pass them in `env` under the names touchmark reads
  (see [Environment variables](../reference/environment.md)), where `touchmark check`
  finds them for the isolation probe.
- **Each input is one flag**; there is no free-form argument. The commands it runs are
  `check`, `plan`, `distribute`, `doctor` and `version`. See
  [The GitHub Action](../reference/action.md) for every input.
- **It needs a Linux runner with Docker, `docker buildx` and the GitHub CLI**, as GitHub's
  hosted Linux runners have. It resolves the image of its version to a digest, verifies
  that digest's build provenance attestation, and runs the image by that digest as the
  runner's user.

The template's workflow, in short:

| Event | Jobs |
|---|---|
| `pull_request` | `check`; `plan` with `strict` and `comment`, after an inline probe of the write secrets |
| `push` to the default branch, a daily `schedule`, `workflow_dispatch` | `probe`, then `distribute` in the environment `touchmark-distribute` |
| a weekly `schedule`, `workflow_dispatch` | `probe`, then `doctor` in the same environment |

Permissions per job: `contents: read` everywhere; `actions: read` for `plan`,
`distribute` and `doctor`, which read the environment's deployment branches;
`pull-requests: write` for `plan --comment`. `distribute` has `concurrency:
touchmark-distribute` and a timeout of 360 minutes: touchmark starts no target after
5 hours 30 minutes, and the next run picks up the rest.

Pull requests from Dependabot and from forks get no secrets, so their `plan` runs
without the read key: it reads no target, reports the hub's side with a warning, and
passes; with `--strict` it would exit 3. The template passes `strict` only for pull
requests from the hub's own branches.

## GitLab CI

Every job runs in the image, pinned by digest:

```yaml
default:
  image:
    name: ghcr.io/bedrock-python/touchmark:X.Y.Z@sha256:<digest>
    entrypoint: [""]   # GitLab runs the job script with sh

variables:
  GIT_DEPTH: "0"       # touchmark reads the hub's whole history

plan:
  rules:
    - if: $CI_PIPELINE_SOURCE == "merge_request_event"
  script:
    # plan reads providers from the default branch's hub.yml
    - git fetch --no-tags origin "+refs/heads/$CI_DEFAULT_BRANCH:refs/remotes/origin/$CI_DEFAULT_BRANCH"
    - touchmark plan --hub . --strict
```

- **Pin the image by digest**, never by a variable: a variable that names the image could
  be overridden to leak the token. Renovate keeps the tag and digest current.
- **`distribute`** runs in `environment: touchmark-distribute` with
  `resource_group: touchmark-distribute`. touchmark checks `CI_COMMIT_REF_PROTECTED=true`
  and `CI_ENVIRONMENT_NAME=touchmark-distribute`, and starts no target in the job's last
  5 minutes (`CI_JOB_TIMEOUT`).
- **`probe`** runs `touchmark probe` in every merge request pipeline, with
  `environment: {name: touchmark-distribute, action: prepare}` and `GIT_STRATEGY: none`.
- **`doctor`** runs in schedules whose description contains "doctor", in the environment
  with `action: prepare`.
- The runners must honor `image:` (Docker, Docker Autoscaler and Kubernetes executors do;
  the Shell executor does not).

## Gitea and Forgejo Actions

Run the job in the image (`container:`). The image has no Node.js, so check the hub out
with git in a `run:` step rather than with a JavaScript action:

```yaml
jobs:
  check:
    if: github.event_name == 'pull_request'
    runs-on: ubuntu-latest
    container:
      image: ghcr.io/bedrock-python/touchmark:X.Y.Z@sha256:<digest>
    defaults:
      run:
        shell: sh
    steps:
      - name: Clone the hub with its whole history
        env:
          HUB_TOKEN: ${{ github.token }}
        run: |
          git init -q /tmp/hub && cd /tmp/hub
          git remote add origin "$GITHUB_SERVER_URL/$GITHUB_REPOSITORY.git"
          # the job token reaches git through its environment, for this host only
          auth=$(printf 'touchmark:%s' "$HUB_TOKEN" | base64 | tr -d '\n')
          GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0="http.$GITHUB_SERVER_URL/.extraHeader" \
            GIT_CONFIG_VALUE_0="Authorization: Basic $auth" \
            git fetch --no-tags origin "+refs/heads/*:refs/remotes/origin/*" "+$GITHUB_REF:refs/touchmark/run"
          git checkout -q --detach "$GITHUB_SHA"
      - run: cd /tmp/hub && touchmark check --hub .
```

The template's `.gitea/workflows/` has the full set: `check`, `plan --comment` (with
`--strict` for the hub's own branches), `distribute` with `--deadline 2h40m` under the
runner's 3-hour limit, and `doctor` weekly in a workflow of its own. Remember that Gitea
and Forgejo Actions give every branch every secret: see
[A hub on Gitea or Forgejo](../getting-started/gitea-forgejo.md).

## Bitbucket Pipelines

Every step runs in the image, pinned by digest, with the hub's whole history; the
template's `bitbucket-pipelines.yml`:

```yaml
image: ghcr.io/bedrock-python/touchmark:X.Y.Z@sha256:<digest>
clone:
  depth: full                     # touchmark reads the hub's whole history

pipelines:
  pull-requests:
    "**":
      - step: { name: probe, clone: { enabled: false }, script: [touchmark probe] }
      - step: { name: check, script: [touchmark check --hub .] }
      - step:
          name: plan
          script:
            - git fetch --no-tags origin "+refs/heads/$BITBUCKET_PR_DESTINATION_BRANCH:refs/remotes/origin/$BITBUCKET_PR_DESTINATION_BRANCH"
            - touchmark plan --hub . --strict --comment
          artifacts: [touchmark-report.md, touchmark-report.json]
  branches:
    "{main,master}":
      - step: { name: probe, clone: { enabled: false }, script: [touchmark probe] }
      - step:
          name: distribute
          deployment: touchmark-distribute
          max-time: 360
          script: [touchmark distribute --hub . --deadline 5h30m]
          artifacts: [touchmark-report.md, touchmark-report.json, touchmark-report.jsonl]
  custom:
    distribute: [...]             # probe, then distribute: Run pipeline and a daily schedule
    doctor: [...]                 # probe, then doctor in the deployment: a weekly schedule
```

- **The write key** is a secured variable of the deployment `touchmark-distribute`, which
  only a step with `deployment: touchmark-distribute` gets. touchmark checks
  `BITBUCKET_DEPLOYMENT_ENVIRONMENT` in `distribute` and `doctor`. Which branches may
  deploy is a Premium setting the API does not show: see
  [A hub on Bitbucket Cloud](../getting-started/bitbucket.md#where-the-write-key-can-live).
- **The hub channel** uses `TOUCHMARK_PIPELINES_TOKEN`, an access token of the hub:
  Pipelines names no default branch and gives a step no API token, so touchmark reads
  the default branch, its tip and the hub's visibility through the API, and keeps
  `plan`'s comment with it.
- **`probe`** runs without the deployment at the start of every pipeline, so a write key
  in a repository or workspace variable stops it before anything else.
- **Schedules** are set up in the website (*Pipelines → Schedules*) for the custom
  pipelines `distribute` (daily) and `doctor` (weekly), on the default branch.
- **The hub's fingerprint** is `bitbucket.org/` and `BITBUCKET_REPO_UUID` without its
  braces, lowercase.

## Azure Pipelines

A hub in Azure Repos (Azure DevOps Services) runs touchmark with docker on the agent: an
Azure container job needs bash, glibc and Node.js in the image, which touchmark's Alpine
image does not have. The template's `azure-pipelines.yml` calls one step template,
`.azure-pipelines/touchmark.yml`, which runs the image pinned by digest as the agent's
user and maps the keys into the step:

```yaml
stages:
  - stage: probe                    # every run, without the variable group
    jobs:
      - job: probe
        steps:
          - checkout: none
          - template: .azure-pipelines/touchmark.yml
            parameters: { name: probe, args: probe, write: true }
  - stage: check                    # pull request builds (Build validation)
    condition: and(succeeded(), eq(variables['Build.Reason'], 'PullRequest'))
    jobs:
      - job: check                  # touchmark check --hub .
      - job: plan                   # fetch the target branch, then plan --hub . --strict --comment
  - stage: distribute               # pushes to the default branch, the daily schedule, Run pipeline
    jobs:
      - deployment: distribute
        environment: touchmark-distribute
        variables:
          - group: touchmark-distribute
        timeoutInMinutes: 60
        strategy:
          runOnce:
            deploy:
              steps:
                - checkout: self
                  fetchDepth: 0
                - template: .azure-pipelines/touchmark.yml
                  parameters: { name: distribute, args: distribute --hub . --deadline 50m, write: true, report: true }
  - stage: doctor                   # the weekly schedule, Run pipeline: the same deployment job
```

- **The write key** is a secret variable of the variable group `touchmark-distribute`,
  which only the deployment jobs link. Any branch's copy of the file could link it too:
  the group's Branch control check, admitting the default branch only, is what keeps
  the others out, and touchmark cannot read it from the job, so `platform` needs
  `security.reason` there. touchmark checks `Environment.Name` (`ENVIRONMENT_NAME`) in
  `distribute` and `doctor`: see
  [A hub on Azure DevOps](../getting-started/azure-devops.md#where-the-write-key-can-live).
- **The hub channel** uses the job access token, `System.AccessToken`, which the step
  template maps into `SYSTEM_ACCESSTOKEN`: no variable names the default branch, so
  touchmark reads it, its tip and the project's visibility through the API, and keeps
  `plan`'s comment with it.
- **`probe`** runs in a stage of its own, without the group, at the start of every run,
  and maps the writer's variable: a write key in a pipeline variable stops the run there.
- **Pull request builds** come from the branch policy *Build validation*: Azure Repos
  ignores `pr:` triggers.
- **Logging commands.** The Azure agent runs any `##vso[…]` it finds in a step's output.
  Under Azure Pipelines (`TF_BUILD=True`) touchmark breaks every `##vso[` it prints, so
  that nothing a platform or a target says becomes a command.
- **The hub's fingerprint** is `dev.azure.com/` and `Build.Repository.ID`, lowercase.

## Reports, summaries and annotations

In CI, `plan` and `distribute` leave their report as `touchmark-report.json` and
`touchmark-report.md` in the working directory, and `distribute` also streams
`touchmark-report.jsonl`, one line per finished target, which survives a killed job.
`doctor` leaves `touchmark-doctor.json` and `touchmark-doctor.md`. Upload them as the
job's artifacts.

- **GitHub Actions:** the step summary (cut to fit its 1 MiB), and `::error` and
  `::warning` annotations for the first ten failed and ten blocked targets of `plan` and
  `distribute`, and the first ten failed and ten warning checks of `doctor`.
  `plan --comment` keeps the report in one comment of the hub pull request, through the
  hub channel.
- **GitLab CI:** the job token cannot write merge request notes, so the report stays in
  the job's log and its artifacts (`expose_as` shows it on the merge request).
- **Gitea 1.27 with runner 2.0:** the step summary. **Forgejo:** the log and the
  artifacts.
- **Bitbucket Pipelines:** no step summary or annotations: the log and the step's
  artifacts. `plan --comment` keeps the report in one comment of the hub pull request
  with `TOUCHMARK_PIPELINES_TOKEN`, without HTML (Bitbucket shows it as text), its marker
  a Markdown link reference definition.
- **Azure Pipelines:** the step template uploads the Markdown reports to the run's
  summary and publishes the report files as an artifact. `plan --comment` keeps the
  report in one closed comment thread of the hub pull request with the job access token,
  as on Bitbucket without HTML, when the project's build service may contribute to pull
  requests on the hub (see the guide for when to grant it).

In a public hub, targets that are not public are only counted in all of these outputs,
never named.

See [Reports](../reference/output.md) for the format, and
[Exit codes](../reference/exit-codes.md) for what fails a job.
