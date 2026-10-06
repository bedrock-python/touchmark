# Memory of declined pull requests

A team that closes a sync pull request without merging it has said no. touchmark
remembers that, and does not propose the same content again. It keeps no state for it:
the memory lives in the closed pull requests themselves, in the marker at the end of
their description.

## Who closed it

| Closed without merging, and… | It is | touchmark… |
|---|---|---|
| touchmark closed it, or the writer or a `known_authors` account did | its own close | remembers nothing |
| a bot closed it (a GitHub `Bot`, an account in `automation_accounts`), or its base branch is gone | an automatic close | proposes the same content again after `memory.auto_close_cooldown` (30 days) |
| anyone else, including an actor the platform does not name | a **decline** | does not propose the same content again |

Automatic closes in a row with the same content back off: the second waits twice as
long, and the third counts as a decline. Its comment suggests keeping the
`engineering-assets` label out of the stale bot. Until the cooldown ends, the target is
`deferred:cooldown`.

## What a decline covers

The marker records every change the pull request carried as a pair `(path, from, mode,
to)`: the path, its blob in the target before, the mode, and the blob the hub proposed (or
a deletion). A decline covers exactly those pairs.

touchmark does not open a new pull request when everything it wants to change now is
covered by the target's declines. It does open one when anything is new:

| Declined | Then | Result |
|---|---|---|
| #7: A created, B created | nothing | `declined` |
| #7: A created, B created | B was edited in the target (it is `local`) | a new pull request with A |
| #7: A created, B created | B added to `ignore` | a new pull request with A |
| #7: A created | a comment changed in the opt-in file | `declined`: the parsed file is the same |
| #7: A created, in a repository the hub subscribed without an opt-in file | an empty opt-in file added | `declined`: the file is parsed the same as the empty one assumed |
| #7: A created; #9: B created | nothing | `declined`: both pairs are covered |
| #7: A created, B created | the pack changes B | a new pull request with A and the new B, saying *A was declined in #7* |
| #7: A at v1 | A at v2 was merged, then the hub went back to v1 | a new pull request: `v2 → v1` is another pair |
| #7: A created | the hub changed its `id`, the old branch is in `branch_aliases` | `declined`: the fingerprint is the same |
| #7, closed by a stale bot | 30 days pass | a new pull request; a third bot close in a row counts as a decline |

A new pull request always carries everything the target is missing, declined pairs
included: pack files belong together, and a partial delivery would leave the repository
inconsistent. The overlap is named in its description.

A decline holds while nothing changed:

- **the opt-in file is parsed the same** — editing `packs` or `ignore` is a new explicit
  choice and lifts every decline of that repository (comments and formatting don't);
- **no declined path became `local` or `ignored`** — a team that made a file its own has
  answered for it.

An open sync pull request is stronger than the memory: touchmark updates it with the
current changes, or closes it as `no-diff`.

## Bringing declined content back

When touchmark first sees a decline, it records it in the marker, adds a tick box to the
description and leaves one comment saying what it remembered, when a new pull request
would come, and how to undo it. Two writes, once.

To undo a decline:

- **reopen the pull request** — an open pull request is stronger than the memory;
- **tick** *Propose this content again* in its description — touchmark marks it revoked
  and opens a new pull request with the current changes;
- **from the hub**, add a `forget_declines` entry to `.touchmark/operations.yml` and merge
  it through review (see [One-off operations](../guide/operations.md)).

To refuse a file for good instead, add it to `ignore`, or set `enabled: false` in the opt-in
file (deleting it works too, unless the hub subscribed the repository) to stop
receiving anything.

## Limits

- Only the last 50 closed pull requests on the sync branches count.
- The marker holds the full list of pairs as long as it fits 48 KiB; beyond that it keeps
  only the content key, and a decline then covers exactly the same set of changes.
- GitLab names who closed a merge request with a delay after a branch is deleted. When a
  person deletes the sync branch while GitLab is still processing touchmark's push,
  GitLab can record the writer as the closer, and the close then counts as touchmark's
  own: that decline is lost and the content is proposed again.
- Closed pull requests of the multi-gitter era, without a marker, are never declines:
  their content is unknown.
