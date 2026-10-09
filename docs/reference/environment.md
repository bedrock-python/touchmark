# Environment variables

touchmark takes credentials only from its environment: never from flags, inputs or
configuration files. It removes each credential from its environment once read, and no
git it runs sees any of them.

## Credentials, per provider

`<ID>` is the provider's `id` from `hub.yml` in upper case, with `-` as `_`: provider
`corp-eu` reads `TOUCHMARK_CORP_EU_*`. With one provider, the names without `<ID>_` work
too: `TOUCHMARK_READ_TOKEN`, `TOUCHMARK_WRITE_APP_KEY`.

| Variable | Read by | What |
|---|---|---|
| `TOUCHMARK_<ID>_READ_TOKEN` | `plan`, `migrate` | the reader's token: GitLab `read_api` and `read_repository`; Gitea and Forgejo `read:repository, read:issue, read:organization, read:user` |
| `TOUCHMARK_<ID>_READ_APP_ID`, `TOUCHMARK_<ID>_READ_APP_KEY` | `plan` | GitHub: the reader App's id and private key (PEM) |
| `TOUCHMARK_<ID>_WRITE_TOKEN` | `distribute`, `doctor` | the writer's token: GitLab `api` and `write_repository`; Gitea and Forgejo `write:repository, write:issue, read:organization, read:user` |
| `TOUCHMARK_<ID>_WRITE_APP_ID`, `TOUCHMARK_<ID>_WRITE_APP_KEY` | `distribute`, `doctor` | GitHub: the writer App's id and private key |
| `TOUCHMARK_<ID>_SIGNING_KEY` | `distribute` | optional: an ssh ed25519 private key registered as the writer's signing key |

A token or an App, not both, per role. With an App, touchmark mints its own short-lived
installation tokens: read tokens per installation, write tokens per target, revoked when
done. A Gitea or Forgejo admin token is refused.

## Other variables touchmark reads

| Variable | What |
|---|---|
| `TOUCHMARK_HUB` | the default of `--hub` |
| `TOUCHMARK_HUB_TOKEN` | a maintainer's token of the hub, for `setup` and `doctor --hub-token`; local runs only |
| `TOUCHMARK_KEY_EXPOSED` | GitHub Actions: the probe job's answer. `distribute` and `doctor` need `false`; `true` means a write key is visible outside the environment, unset means the probe is missing |
| `GITHUB_TOKEN` | the hub channel on GitHub Actions (and on Gitea and Forgejo when `GITEA_TOKEN` is unset): the tip of the default branch, the hub's visibility, the environment's branch policy, `plan --comment` |
| `CI_JOB_TOKEN` | the hub channel on GitLab CI |
| `GITEA_TOKEN` | the hub channel on Gitea and Forgejo Actions |
| `SYSTEM_ACCESSTOKEN` | the hub channel on Azure Pipelines: the job access token, `System.AccessToken`, which a step gets only when it maps it (`env: SYSTEM_ACCESSTOKEN: $(System.AccessToken)`): the default branch, its tip, the project's visibility, `plan --comment` |
| `TOUCHMARK_PIPELINES_TOKEN` | the hub channel on Bitbucket Pipelines, which gives a step no API token: an access token of the hub repository with *Repositories: Read* (the default branch, its tip, the visibility) and *Pull requests: Write* (`plan --comment`) |
| `CI` | any value but `""`, `false` and `0` makes a run a CI run: operation flags exit 2, and an unknown hub visibility counts as public |

From the CI, touchmark also reads the platform's own variables: the server and API URLs,
the repository and its id (the hub's fingerprint), the ref and the event, the step
summary file, the job's timeout and start (the default `--deadline`), the merge request
and its base (the scope of `plan`), the project's visibility, on GitLab
`CI_COMMIT_REF_PROTECTED`, `CI_ENVIRONMENT_NAME` and `CI_SERVER_TLS_CA_FILE`, and on
Bitbucket Pipelines `BITBUCKET_REPO_UUID`, `BITBUCKET_REPO_FULL_NAME`, `BITBUCKET_BRANCH`,
`BITBUCKET_TAG`, `BITBUCKET_PR_ID`, `BITBUCKET_DEPLOYMENT_ENVIRONMENT` and
`BITBUCKET_REPO_IS_PRIVATE`, and on Azure Pipelines `TF_BUILD`, `SYSTEM_COLLECTIONURI`,
`SYSTEM_TEAMPROJECT`, `BUILD_REPOSITORY_PROVIDER`, `BUILD_REPOSITORY_ID`,
`BUILD_REPOSITORY_NAME`, `BUILD_REPOSITORY_URI`, `BUILD_SOURCEBRANCH`, `BUILD_REASON`,
`SYSTEM_PULLREQUEST_SOURCEBRANCH`, `SYSTEM_PULLREQUEST_PULLREQUESTID` and
`ENVIRONMENT_NAME`.

## In the GitHub Action

The Action passes the container only `CI`, every `GITHUB_*` and `TOUCHMARK_*` variable of
the step, and the runner's proxy settings. Set credentials in the step's `env`:

```yaml
- uses: bedrock-python/touchmark@<commit> # vX.Y.Z
  with:
    command: plan
  env:
    GITHUB_TOKEN: ${{ github.token }}
    TOUCHMARK_READ_APP_ID: ${{ vars.TOUCHMARK_READ_APP_ID }}
    TOUCHMARK_READ_APP_KEY: ${{ secrets.TOUCHMARK_READ_APP_KEY }}
```

`touchmark check` reads the workflows: every write key a step hands to touchmark
(`TOUCHMARK_*_WRITE_*`, `*_SIGNING_KEY`, in `env` or `with`) must be tested by the
probe's expression.
