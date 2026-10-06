# Run it in CI

The [hub template](https://github.com/bedrock-python/engineering-assets-template) ships
the CI files for GitHub Actions, GitLab CI, Gitea and Forgejo Actions. Copy them rather
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

In a public hub, targets that are not public are only counted in all of these outputs,
never named.

See [Reports](../reference/output.md) for the format, and
[Exit codes](../reference/exit-codes.md) for what fails a job.
