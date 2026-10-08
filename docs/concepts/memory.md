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
would come, and how to undo it. Two writes, once. (Not on Bitbucket Cloud, where a
declined pull request cannot be edited: see [below](#on-bitbucket-cloud).)

To undo a decline:

- **reopen the pull request** — an open pull request is stronger than the memory;
- **tick** *Propose this content again* in its description — touchmark marks it revoked
  and opens a new pull request with the current changes;
- **from the hub**, add a `forget_declines` entry to `.touchmark/operations.yml` and merge
  it through review (see [One-off operations](../guide/operations.md)).

To refuse a file for good instead, add it to `ignore`, or set `enabled: false` in the opt-in
file (deleting it works too, unless the hub subscribed the repository) to stop
receiving anything.

## On Bitbucket Cloud

A declined pull request on Bitbucket can never be edited or reopened, by anyone. touchmark
writes nothing to it, and remembers the decline from what the marker held while the pull
request was open:

- **The opt-in state is recorded up front.** Every pull request touchmark opens records
  in its marker the state of the opt-in file it is proposed under, which the
  acknowledgment records elsewhere. When the file changes while the pull request is
  open, the next run records the new state, with one edit of the description even when
  the proposed changes stay the same.
- **A decline** holds while the opt-in file is parsed the same as that recorded state,
  and lapses when the team edits `packs` or `ignore`, as an acknowledged decline does
  elsewhere. A declined path becoming `local` or `ignored` lifts it too.
- **No comment, no tick box.** touchmark leaves no comment on the declined pull request
  (with nothing to record that it did, it would post one every run), and descriptions
  carry no tick boxes there: Bitbucket shows the HTML comments they rest on as text. The
  report says `declined`, and the description's footnote names the way back.
- **Bringing declined content back** is a `forget_declines` entry, or a change of `packs`
  or `ignore`. touchmark cannot mark the pull request revoked, so the entry acts on every
  run while it is present: keep it until the pull request it brings is merged or closed.
  Removed earlier, the decline is in force again; an open pull request stays (an open pull
  request is stronger than memory), and its description then names the decline under
  *Previously declined*.
- **A marker without the opt-in state** (one touchmark did not write in full) holds its
  decline whatever the opt-in file says, since there is no state to compare with; the
  report warns about it on every run and names the `forget_declines` entry that lifts
  it.
- **Who closed it** counts as everywhere: Bitbucket names who declined a pull request.
  touchmark's own closes write the close into the description in the same edit that
  declines it; bots' closes wait for the cooldown, and the third in a row counts as a
  decline.

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
- On Bitbucket Cloud the opt-in state of a decline is the one recorded by the last run
  before the decline. When the team changes `packs` or `ignore` and declines the pull
  request before touchmark runs again, the decline counts from the earlier state and has
  lapsed already: the content is proposed once more, under the new choice.
