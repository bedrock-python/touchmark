# A hub on GitHub

github.com, GHE.com or GitHub Enterprise Server 3.19 and newer. The hub runs its CI on
GitHub Actions with the touchmark Action; a reader App plans on pull requests and a
writer App delivers from the default branch.

```text
your-org/engineering-assets              the hub: packs/, hub.yml, targets.yml
        │  one pull request per repository on every change to a pack
        ▼
your-org/billing, your-org/sdk-python    opted in with .engineering-assets.yml
```

`touchmark setup github`, run from a clone of the hub with an administrator's token,
does most of steps 2 to 4: it creates both Apps, the environment and the ruleset, and
prints the commands that store the keys. You install the Apps. See
[Set up a hub's platform](../guide/setup.md#github).

## Steps

1. **Create the hub.** Click **Use this template** on
   [the template](https://github.com/bedrock-python/engineering-assets-template) and
   create `your-org/engineering-assets`. It can be private, but see the note on GitHub
   Free below. Don't fork: forks of public repositories are always public.
2. **Create two GitHub Apps** in your organisation, and install both on the repositories
   you will target, not on the hub:
    - *reader*, with Metadata, Contents and Pull requests read-only;
    - *writer*, with Metadata read-only, and Contents, Pull requests and Workflows read
      and write. touchmark asks for Workflows only for a target that needs it.
3. **Store the keys.**
    - Reader: the repository variable `TOUCHMARK_READ_APP_ID` (the App ID) and the
      repository secret `TOUCHMARK_READ_APP_KEY` (its private key).
    - Writer: create an environment named `touchmark-distribute`. Set its *Deployment
      branches and tags* to *Selected branches and tags*, with your default branch only.
      Store the variable `TOUCHMARK_WRITE_APP_ID` and the secret
      `TOUCHMARK_WRITE_APP_KEY` in that environment, and nowhere else. Don't choose
      *Protected branches only*: with no protection rules, every branch qualifies.

    The workflow's `probe` job stops `distribute` if the write key is also visible as a
    repository or organisation secret, and touchmark itself checks the environment's
    branch policy before it writes.
4. **Protect the hub.** Add a ruleset for the default branch that requires a pull request
   with an approving review and a review from Code Owners. Replace
   `@acme/hub-maintainers` in `.github/CODEOWNERS` with your team.
5. **Describe your hub.** In `hub.yml` set `id`, and `writer` to the writer App's bot
   name, like `acme-assets-write[bot]`. List your repositories in `targets.yml`.
6. **Open a pull request.** The `check` job validates the hub; the `plan` job reports
   what each repository would receive and keeps the report in a comment. Write access is
   checked when `distribute` runs, and every week by `doctor`.
7. **Merge it.** The `distribute` job opens a pull request in every repository that has
   [opted in](opt-in.md). It runs again every day and on every merge; **Run workflow**
   runs `distribute` and `doctor` at once. The workflow starts on pushes to `main` and
   `master`: if your default branch has another name, add it under `push: branches:`.

## What the workflow does

| Event | Jobs |
|---|---|
| pull request to the hub | `check`; `plan --strict --comment` after an inline probe |
| push to the default branch, every day, *Run workflow* | `probe`, then `distribute` |
| every Monday, *Run workflow* | `probe`, then `doctor` |

Pull requests from Dependabot and from forks get no Actions secrets, so their `plan` runs
without the read key: it shows the hub's side only, warns that no target was checked, and
passes. Before you merge an engine update, read its release notes and run `plan` with the
read key yourself.

## Notes

- **GitHub Free.** A private hub on GitHub Free cannot limit an environment secret to a
  branch, so the isolation check fails. Make the hub public, use the Team plan, or keep
  the write key in an external secret store released through OIDC
  (`security.write_isolation: external`).
- **Public hubs** skip private and internal targets and print only how many: their CI
  logs are public. See [the security model](../concepts/security.md#public-hubs).
- **Scheduled workflows** in a public repository are disabled by GitHub after 60 days
  without activity. Push to the hub or re-enable the workflow if the daily run stops.
- **GitHub Enterprise Server**: use `actions/upload-artifact` v3.2.2 in the workflow;
  v4 and later do not support it. Signed commits through the API need *web commit
  signing* turned on by an administrator.

Next: [opt a repository in](opt-in.md).
