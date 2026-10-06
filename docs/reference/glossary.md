# Glossary

**adopt** — take a `local` file back under the hub: `touchmark apply --adopt <glob>`
replaces it with the pack's current version. Also `adopt_unmarked`, the one-off
operation that takes over a multi-gitter setup's pull requests.

**automatic close** — a sync pull request closed without merging by a bot (a GitHub
`Bot`, an account in `automation_accounts`) or because its base branch is gone. Not a
decline: the content comes back after `memory.auto_close_cooldown`.

**content key** — `sha256:` of every `(path, from, mode, to)` a pull request carries. The
memory of declines compares by it.

**decline** — a sync pull request a person closed without merging. touchmark does not
propose the same content to that repository again.

**distribute** — the command that writes: opens, updates and closes sync pull requests,
with the writer, from the hub's default branch.

**fingerprint** — the hub's identity: the host and immutable repository id of the hub,
like `github.com/712345678`. In the commit trailers and the marker. CI provides it;
`--hub-fp` outside CI.

**hub** — the repository, created from the template, that holds the packs, `hub.yml`,
`targets.yml` and `.touchmark/operations.yml`.

**hub channel** — the hub CI's own token (`GITHUB_TOKEN`, `CI_JOB_TOKEN`, Gitea's token),
used only towards the hub repository: the tip of its default branch, its visibility, its
environment, the plan comment.

**`known_authors`** — former writers whose pull requests are still the hub's own.

**`local`** — the state of a file whose content matches no version the hub ever shipped
at its path: the repository's own, never touched.

**manifest** — every version each pack ever shipped at each path, read from the hub's
history. `touchmark manifest` prints it.

**marker** — the HTML comment at the end of a sync pull request's description:
`<!-- touchmark:v1 hub=… fp=… stream=sync key=sha256:… data=… -->`. It records the
fingerprint and what the pull request carries.

**opt-in file** — `.engineering-assets.yml` at a target's root. Its presence is consent;
it can add packs and `ignore` paths.

**`orphaned`** — the state of a file shipped only by a pack the target no longer gets, and
still unchanged: left in place and listed.

**pack** — a directory `packs/<name>/` of the hub, laid out like a target repository.

**plan** — the command that reports, with the reader, what `distribute` would do; it runs
on every hub pull request.

**probe** — the check that the write key is not visible to jobs any branch can start: a
job on GitHub Actions, `touchmark probe` on GitLab CI.

**provenance** — ownership decided by content: a file is the hub's while its content is a
version the hub shipped at that path.

**provider** — a platform instance the hub delivers to, in `hub.yml`'s `providers`, with
its own reader and writer. Targets name it as `<provider>:<path>`.

**reader** — the read-only account `plan` uses.

**sweep** — the search for the hub's open pull requests in repositories that are no
longer targets, to close them.

**sync branch** — `touchmark/<id>` in each target: one touchmark commit on the target's
default branch.

**sync pull request** — touchmark's pull request (merge request on GitLab) from the sync
branch.

**target** — a repository `targets.yml` selects.

**write isolation** — keeping the write key visible to the default branch only:
`security.write_isolation` (`platform`, `external`, `none`).

**writer** — the account `distribute` and `doctor` use, different from the reader, with
no access to the hub.
