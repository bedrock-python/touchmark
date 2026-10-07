# A hub on GitLab

gitlab.com or a self-managed GitLab 17.0 and newer. Every job of the hub's pipeline runs
in the touchmark image; a reader account plans in merge request pipelines and a writer
account delivers from the default branch.

`touchmark setup gitlab --group <your group>`, run from a clone of the hub with the
token of a Maintainer of the hub who owns that group, does steps 2 to 4 through the API.
See [Set up a hub's platform](../guide/setup.md#gitlab).

## Steps

1. **Create the hub.** Choose *New project → Import project → Repository by URL* and give
   the URL of [the template](https://github.com/bedrock-python/engineering-assets-template),
   `https://github.com/bedrock-python/engineering-assets-template.git`.
2. **Create two service accounts** and add them to the groups that own your
   repositories. On instances without service accounts, use group access tokens. Keep
   the hub out of those groups: put it in a group or subgroup the accounts don't belong
   to, and add the accounts, or create the group access tokens, only on groups that hold
   targets and not the hub. A member of a group inherits its projects, and a group access
   token is a member of its group, so a writer on a group that holds the hub could push
   to it; `touchmark setup gitlab` refuses that layout, and `doctor` fails on it.
    - The *reader* has the role Reporter and a token with `read_api` and
      `read_repository`. Store it as the variable `TOUCHMARK_READ_TOKEN`, masked but not
      protected: merge request pipelines need it.
    - The *writer* has the role Developer and a token with `api` and `write_repository`.
      Store it as `TOUCHMARK_WRITE_TOKEN`: protected, masked and hidden, environment
      scope `touchmark-distribute`. Don't add the writer to the hub project.
3. **Lock down the hub.**
    - Protect only the default branch (*Allowed to push*: No one) and don't create
      protected tags: a protected variable reaches every protected ref.
    - Set *Minimum role to use pipeline variables* to *No one allowed*, and keep *Allow
      merge request pipelines to access protected variables* off.
    - The `probe` job fails a merge request pipeline that can see the write token. Run
      `touchmark doctor --hub-token` once with a Maintainer token to check the
      variable's settings.
    - Replace `@acme/hub-maintainers` in `.gitlab/CODEOWNERS` with your group. Requiring
      code owner approval needs Premium.
    - The hub's runners must honor `image:`: GitLab.com's hosted runners do, and so do
      the Docker, Docker Autoscaler and Kubernetes executors. The Shell executor ignores
      `image:`, and the jobs fail.
    - If your runners can't pull from ghcr.io, change the image in `.gitlab-ci.yml` in a
      reviewed merge request. The image is pinned by digest on purpose: an overridable
      variable could leak the token.
4. **Schedule the runs.** Under *Build → Pipeline schedules*, create two schedules for
   the default branch: a daily one described `touchmark distribute` and a weekly one
   described `touchmark doctor`. The `doctor` job runs in schedules whose description
   contains "doctor"; every other schedule runs `distribute`.
5. **Describe your hub.** In `hub.yml` set `id`, and `writer` to the writer's username.
   For a self-managed instance, describe it under `providers` instead, with its `url`
   and `writer`, so that touchmark also knows it outside CI. List your repositories in
   `targets.yml`.
6. **In your target projects,** turn on the CI/CD job token allowlist (*Limit access to
   this project*). Sync pipelines run as the writer, so their job token carries the
   writer's access to your other projects. Use a separate writer for groups you trust
   differently.
7. **Open a merge request** and check the `check`, `plan` and `probe` jobs. The plan
   report is attached to the merge request as the artifact *touchmark plan*.
8. **Merge it.** The `distribute` job opens a merge request in every project that has
   [opted in](opt-in.md). *Run pipeline* on the default branch runs `distribute` and
   `doctor`.

## What the pipeline does

| Pipeline | Jobs |
|---|---|
| merge request to the hub | `check`, `plan --strict`, `probe` (in the environment, `action: prepare`) |
| push to the default branch, a schedule, *Run pipeline* | `distribute` |
| a schedule whose description contains "doctor", *Run pipeline* | `doctor` |

`distribute` runs in the environment `touchmark-distribute` with
`resource_group: touchmark-distribute`, so two runs never overlap, and a timeout of 3
hours: touchmark starts no target in the job's last 5 minutes, and the next run picks up
what is left.

## Notes

- **Service accounts** exist on GitLab Free from 18.11 (on gitlab.com up to 100 per
  top-level group); GitLab CE has them from 19.x. A self-managed instance lets group
  Owners create them only when an administrator allows it. Elsewhere, use group access
  tokens: a new token is a new bot user, so add the old bot to `known_authors` when you
  replace one.
- **The merge request comment.** GitLab's job token cannot write merge request notes, so
  the plan report stays in the job's log and artifacts.
- **Schedules** stop when their owner loses access: give them to an account that stays.

Next: [opt a repository in](opt-in.md).
