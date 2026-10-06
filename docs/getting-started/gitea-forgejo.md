# A hub on Gitea or Forgejo

Gitea 1.26 and newer, Forgejo 15 and newer, self-hosted or hosted.

Gitea and Forgejo Actions can't limit a secret to the default branch: every branch's jobs
see every Actions secret, and `environment` is ignored. So touchmark won't use their CI
with `security.write_isolation: platform`. Pick one:

- **Deliver from GitHub or GitLab (recommended).** Run the hub's CI on GitHub or GitLab,
  and deliver to Gitea or Forgejo from there: add them as `providers` in `hub.yml`, and
  their keys next to the others, in the protected environment or variable. See
  [Deliver to several platforms](../guide/providers.md).
- **Host the hub on Gitea or Forgejo** with `security.write_isolation: none` and a
  `reason`, and protect every branch (`*`) so that only maintainers can push.
  Contributors then work from forks, which receive no secrets.

```yaml
# hub.yml of a hub hosted on Forgejo
version: 1
id: acme-eng
providers:
  - id: cb
    type: forgejo
    url: https://codeberg.org
    writer: acme-touchmark
security:
  write_isolation: none
  reason: "Forgejo Actions cannot keep a secret to one branch; every branch is protected and only maintainers push"
```

## A hub on Gitea or Forgejo

1. **Create the hub** by migrating
   [the template](https://github.com/bedrock-python/engineering-assets-template).
2. **Create two bot users:** a reader in a team with read access and a writer in a team
   with write access to your repositories. Give the teams your target repositories one by
   one, not *all repositories* of the organisation, which would include the hub.
    - Reader token: `read:repository, read:issue, read:organization, read:user`.
    - Writer token: `write:repository, write:issue, read:organization, read:user`.
    - Never use an admin token: touchmark refuses it, because through `Sudo` it acts as
      anyone.
    - Store them as the Actions secrets `TOUCHMARK_READ_TOKEN` and
      `TOUCHMARK_WRITE_TOKEN`.
3. **Check the runner label.** The workflows in `.gitea/workflows/` run on
   `ubuntu-latest`, the label of Gitea's default runners. If your runners are registered
   with other labels, change `runs-on`. Every job runs in the touchmark image and clones
   the hub with git, so it needs no actions from github.com or a mirror. The report is in
   the job's log, and on Gitea 1.27 with runner 2.0 in its summary.
4. **Protect the hub** with a branch protection on `*` that lets only maintainers push,
   and replace `@acme/hub-maintainers` in `.gitea/CODEOWNERS` with your team.
5. **Describe your hub.** Set `id`, `providers` (with `url`: Gitea and Forgejo have no
   default instance) and `security` in `hub.yml`, then open a pull request and merge it.
   `distribute` runs on every merge to `main` or `master` (add your default branch under
   `push: branches:` if it has another name) and every day; `doctor` every Monday, from
   `.gitea/workflows/engineering-assets-doctor.yml`.

## Notes

- **Bot users on Gitea 1.26** created with `admin user create --user-type bot` must
  change their password first, and their tokens get 403 on every request until the flag
  is cleared: `gitea admin user must-change-password --unset <login>`. With
  `TWO_FACTOR_AUTH=enforced`, the bot needs TOTP.
- **Tokens** of Gitea and Forgejo do not expire; `doctor` checks their scopes on Gitea
  1.27 and reports the expiry as `ok`.
- **Drafts** are the `WIP:` title prefix on Gitea and Forgejo; set `pr.draft: true`,
  never the prefix in `pr.title`.
- **Instances that require sign-in to view anything** (`REQUIRE_SIGNIN_VIEW`) count as
  non-public: a public hub skips their repositories.

Next: [opt a repository in](opt-in.md).
