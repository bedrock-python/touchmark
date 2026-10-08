# Delivery and the sync branch

`distribute` delivers through git and each platform's API. It fetches only the git
objects it needs, builds each commit from the hub's blobs, pushes with a lease, and
opens pull requests through the platform's API. It never checks a target out, so no
hook, filter, LFS or submodule of a target ever runs.

## The sync branch

Each target has one sync branch, `touchmark/<id>` (`branch` in `hub.yml` changes it),
holding **one touchmark commit on top of the target's default branch**:

```text
chore: sync engineering assets

Touchmark-Hub: acme-eng@github.com/712345678
Touchmark-Stream: sync
Touchmark-Content: sha256:6b1f…
Touchmark-Hub-Commit: 3f2c1ab9…
```

- The commit changes exactly the paths touchmark decided to change, and git checks that
  before every write. Its author and committer are the writer account; its date is the
  later of the hub commit and the target's base.
- The same inputs give the same commit, so a run that lost a response and retries writes
  nothing new.
- `Touchmark-Hub` carries the hub's fingerprint: a branch of another hub with the same
  `id` is never taken for this one (`blocked:branch-taken`).
- The branch is rebuilt from the target's current default branch only when the content
  touchmark wants differs from what the branch carries. A target's busy default branch
  does not make touchmark push: CI of hundreds of targets is not rerun on every commit
  to their main branch.

### When someone else pushes to it

touchmark rewrites the branch only when nothing on it is someone else's. It checks by
content, not by author:

- *Update branch* (a clean merge of the base branch) is fine: touchmark recognises it and
  carries on.
- Any other commit — a fix pushed to the branch, a merge of another branch, a rebase that
  moved touchmark's commit onto other commits — **pauses** the branch:
  `blocked:edited`. The pull request gets a *touchmark paused* block that says what a
  rebuild would bring, and a tick box:

  ```markdown
  - [ ] Rebuild this branch (drops commits added by others)
  ```

  Ticking it lets the next run rebuild the branch from scratch; the added commits are
  dropped. From the hub, a `recreate` entry in `.touchmark/operations.yml` does the same
  (see [One-off operations](../guide/operations.md)).
- A branch that someone else's open pull request uses is never moved or deleted:
  `blocked:branch-in-use`.

Every move of a branch is a compare-and-swap: a push with `--force-with-lease` on the
head touchmark read, or the API's equivalent. A concurrent change makes the write fail
and the target be inspected again.

## Signing

`sign` in `hub.yml` (per provider) chooses:

- `auto` (the default): sign where it costs nothing, and where a target's rules require
  signed commits;
- `always`: sign every commit, and block a target where that is impossible.

| Writer | With `TOUCHMARK_<ID>_SIGNING_KEY` | Without it, where a target requires signed commits |
|---|---|---|
| GitHub App | — (an App has no signing key) | an API commit GitHub signs as the App: 3 writes instead of 1 |
| GitHub user token | every commit signed with the key | `blocked:cannot-sign` |
| GitLab | every commit signed with the key | `blocked:cannot-sign` |
| Gitea, Forgejo | every commit signed with the key | `blocked:cannot-sign` |

GitHub Enterprise Server signs API commits only when an administrator has turned on web
commit signing.

The key is an ssh ed25519 private key registered as a signing key of the writer account,
whose email must be verified. touchmark signs the commit object itself, in git's SSH
signature format.

## The pull request

One pull request per target, from the sync branch to the default branch.

| Field | Belongs to | touchmark writes it |
|---|---|---|
| body | touchmark | when the changes it proposes change, and to record a decline, a close or a rebuild; also when a repository the hub subscribed (`opt_in: assumed`) gains or loses its opt-in file, which adds or drops the subscription paragraph |
| title | people, after creation | at creation; later only if `pr.title` changed and nobody edited the title |
| draft | people, after creation | at creation (`pr.draft`) |
| labels | shared | at creation (`pr.labels`); later only labels it never set; never removes one |
| base | touchmark | when the default branch is renamed |

The body, in English:

1. the text of `pr.intro_file`, then the hub, the hub commit and the packs;
   for a repository the hub subscribed without an opt-in file, a paragraph on how to
   choose packs or keep files out (add the opt-in file with `packs` or `ignore`) and how
   to opt out (`enabled: false` in it). Never cut;
2. **⚠ Sensitive paths**: changes to workflows, CI configuration, agent settings, skills,
   subagents, hooks, CODEOWNERS, `.gitattributes` and executable files (plus
   `sensitive_paths` from `hub.yml`). Never cut;
3. a table of the changes, by file and pack: sensitive first, then deletions, updates,
   additions, up to 100 rows;
4. a folded list of the files the repository made its own, with how to take them back
   (a plain section on Bitbucket Cloud, which shows HTML as text);
5. blocks for the situation: *previously declined in #N*, *touchmark paused*, *the hub
   proposes nothing more*;
6. the tick boxes;
7. a footnote on how the memory works;
8. the **marker**, last: an HTML comment with the hub's fingerprint, the content key and
   compressed data on what the pull request carries.

Paths and strings from a target are only ever shown in code spans; no line of a body or
comment starts with `/`, so GitLab quick actions never fire; `@` never mentions anyone.
With `pr.link_hub: auto`, a private hub's name and links stay out of public targets'
pull requests.

A pull request is **touchmark's own** only if it is in the target repository itself (not
a fork), on a sync branch (`branch` or `branch_aliases`), opened by the writer or a
`known_authors` account, with this hub's fingerprint in its marker. touchmark touches no
other pull request.

## Closing stale pull requests

| Reason | When |
|---|---|
| `no-diff` | the target already has everything; the branch is deleted |
| `opted-out` | the repository is no longer opted in: its opt-in file says `enabled: false`, or it has no opt-in file and the hub does not subscribe it (the file was deleted, or the hub dropped `opt_in: assumed`); the comment says which |
| `target-dropped` | the target left `targets.yml` or entered `exclude` |
| `duplicate` | two of touchmark's pull requests are open, on the branch and on an alias |

A close is one edit of the pull request with the reason in its marker, then a comment
with the reason. The **sweep** finds touchmark's open pull requests in repositories that
are no longer targets. It runs only when every organisation and group of `targets.yml`
was listed in full, and never with `--only`.

**The mass-close guard:** if a run would close more than
max(5, `limits.max_close_fraction` × touchmark's open pull requests), it closes none
(`blocked:mass-close`, exit 1). An `allow_mass_close` entry in `operations.yml` lifts it.

## Limits and budgets

Reads and fetches run in parallel, 8 targets at a time per provider by default (4 on
Gitea and Forgejo). Writes go through one queue per provider and account, with sliding
windows per minute and per hour and a minimum interval; `Retry-After` and rate-limit
headers pause the whole queue. Defaults follow each platform's documented limits, for
example 60 writes a minute and 450 an hour on github.com; `providers[].limits` overrides
them.

- A run opens at most `limits.max_new_prs_per_run` (100) pull requests; the rest are
  `deferred:rollout-limit` until the next run.
- `--deadline` (default: 5h30m on GitHub Actions, the job's timeout less 5 minutes on
  GitLab) stops starting new targets; the rest are `deferred:deadline`.
- Rate limits and outages defer the provider's remaining targets.

`plan` prints the estimate: the writes per provider, the time the limits give them, and
the number of runs a first rollout needs. A new pull request costs 2 to 3 writes on the
git path (push, pull request, labels), an API commit 2 more.
