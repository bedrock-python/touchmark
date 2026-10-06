# FAQ

## For teams that receive sync pull requests

**I changed a managed file. Will the next sync revert it?**
No. From that moment the file is `local`, and touchmark leaves it alone even when a pack
changes it later. To receive updates again, restore the pack version or run
`touchmark apply --adopt <path>`.

**Our file looks the same as the pack's. Why is it `local`?**
Most often it was committed with CRLF line endings and the pack's version has LF.
touchmark compares what the repository holds, the committed blob, so the line endings
count, on Windows checkouts too. Editing the text is not enough while the file keeps
CRLF: git commits CRLF again, even with `core.autocrlf=true`. Run
`touchmark apply --adopt <path>` and commit the result to take the pack's version.

**How do I stop receiving a file?**
Add it to `ignore` in `.engineering-assets.yml`. Deleting a managed file is not enough:
the next sync restores it.

**We closed the sync pull request. Will it come back?**
Not with the same content. A marker in the description records what the pull request
carried, and touchmark won't propose content you closed without merging. A new pull
request arrives when a pack changes those files, when you change `packs` or `ignore` in
`.engineering-assets.yml`, or when you make one of those files your own. Changed your
mind? Reopen the pull request, or tick *Propose this content again* in its description.
A pull request closed by a bot, such as a stale bot, doesn't count at first: touchmark
proposes it again after 30 days, then after 60; the third bot close counts as a decline.

**Someone pushed a commit to the sync branch. What happens?**
touchmark stops updating the branch and says so in the pull request. Tick *Rebuild this
branch* to let it start over; that drops the added commits. *Update branch* is fine:
touchmark recognises a clean merge of the base branch. Merging any other branch into the
sync branch pauses it too.

**Who authors the commits? Are they signed?**
The hub's write account. touchmark signs with the bot's own key where one is set up, and
on GitHub through the platform where a target requires signed commits. On GitLab, a
target that rejects unsigned commits needs the writer's signing key; otherwise touchmark
reports it as blocked.

**Can we edit the title of a sync pull request?**
Yes. Once a person changes the title, labels or draft state, touchmark leaves them. The
description is touchmark's: it rewrites it when the proposed changes change. Comment
instead of editing it.

**Can we get the changes without the pull request?**
Run `touchmark apply` in a checkout with the hub next to it, and commit the result: see
[Use it on your machine](local.md). The open sync pull request then has nothing left to
propose and is closed as `no-diff`.

**How do we leave entirely?**
Delete `.engineering-assets.yml`. touchmark closes its open pull request as `opted-out`
and delivers nothing more. Files already merged stay.

## For hub maintainers

**Why did touchmark skip some of our repositories?**
A public hub's CI logs are public, so touchmark skips private and internal targets there
and prints only how many. A hub whose visibility the CI does not report counts as public.
Set `security.private_targets_in_public_hub: deliver` to deliver to them anyway; their
names will then appear in the logs. Repositories without the opt-in file are skipped too.

**Can two packs ship the same path?**
Yes. Packs are layered in order, and the later one wins.

**We renamed a pack. Do the files stay managed?**
Yes, if the new pack lists its old name under `formerly` in `hub.yml`: the old name's
history keeps proving ownership. A pack the hub no longer assigns to a repository leaves
its files there; `status` lists them as `orphaned`.

**Why does the hub need its full git history?**
The history is the ownership record: touchmark recognises older versions by reading every
commit that touched `packs/`. On a shallow clone, managed files would look `local`, so
touchmark refuses to run on one. It refuses a partial clone (`--filter`) too, which would
fetch every old version one at a time.

**What if the hub has a mistake?**
`status` and `apply` run the same checks as `touchmark check` and refuse a hub that fails
them, with nothing written. A wrong `formerly`, for example, could otherwise make a
pack's files look retired. In CI, `plan` on the hub's pull request shows every target's
outcome before anything is merged.

**Does touchmark run anything from my repositories?**
No. It reads and writes files and calls git. It never executes scripts or hooks from the
hub or from the targets. In the hub's CI it never checks your repository out.

**Why two accounts?**
The read key reaches every branch of the hub: anyone who can push a branch can use it.
The write key must not. A separate reader also has its own rate limits, and on GitLab a
role belongs to the user, not to the token. See the
[security model](../concepts/security.md).

**How many writes does a rollout cost?**
`plan` prints it: writes per provider, the time the provider's limits give them, and the
number of runs. As an order of magnitude, a first rollout to 500 GitHub targets is about
2000 writes and four hours on the git path, in five runs with the default
`max_new_prs_per_run: 100`.

**How do we upgrade touchmark?**
Read the release notes, then merge the Dependabot (the Action) or Renovate (the image)
pull request. Dependabot's `plan` gets no secrets and shows the hub's side only: run
`plan` with the read key yourself before you merge. Renovate waits for your approval on
its Dependency Dashboard for the touchmark image.

## How it compares

| | touchmark | Copier `update` | repo-file-sync-action | rulesync, ruler |
|---|---|---|---|---|
| Scope | many repositories from one hub | one repository from one template | many repositories from one repository | agent formats inside one repository |
| A file the repository changed | kept; it becomes the repository's own | three-way merge, conflicts land in the pull request | overwritten, or never touched again | regenerated |
| State in the target | none | `.copier-answers.yml` | none | a source directory |
| Platforms | GitHub, GitLab, Gitea, Forgejo; several at once from one hub | any (runs locally) | GitHub | any (runs locally) |
