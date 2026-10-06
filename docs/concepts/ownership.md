# Ownership by provenance

touchmark manages a file in a target only while its content is, byte for byte, a version
the hub has shipped at that path. That one rule is what makes it safe to point at many
repositories: a file a team has changed is the team's, and touchmark never overwrites or
deletes it.

## The manifest

touchmark reads every commit of the hub that touched `packs/` and records, for every path
and every pack, each version it ever shipped: the git blob id and the size. That record
is the **manifest**; `touchmark manifest` prints it.

```json
{
  "version": 1,
  "hub_commit": "ffae0953c140298cf045e600f15710104e9d5d0d",
  "paths": {
    "AGENTS.md": {
      "agents": [
        { "oid": "71ed00304c07e4619ac2353b8aae07e7fa4b48c1", "size": 93 }
      ]
    }
  }
}
```

The current version of a pack is what the hub's last commit holds, not the working tree:
uncommitted edits never ship. `--worktree` makes `check`, `plan`, `status` and `apply`
read the working tree instead, to try a pack before committing it; `distribute` refuses
it.

Because the history is the record, the hub must be a full clone. touchmark refuses a
shallow clone (every managed file would look `local`) and a partial clone (every old
version would be fetched one at a time).

## States

For every path the selected packs have ever shipped, the target's file gets one state:

| State | The target file… | touchmark… |
|---|---|---|
| `missing` | does not exist | creates it |
| `current` | matches the current pack version | leaves it |
| `outdated` | matches an older pack version | updates it |
| `local` | matches no version the hub ever shipped | leaves it: it is the repository's own |
| `ignored` | is listed under `ignore` | leaves it |
| `retired` | is no longer shipped, and its content came from the hub | deletes it |
| `retired-local` | is no longer shipped, and its content is local | leaves it |
| `unsafe` | is a symlink, is not a regular file, or points outside the repository | leaves it |
| `orphaned` | was shipped only by a pack this repository no longer gets, and still matches its version | leaves it, and lists it |

Only the packs selected for the target count, with their former names from `formerly`.
A version shipped by a pack the target does not get proves nothing about the target's
file.

**Content under 64 bytes never counts as evidence.** An empty `.gitkeep` or a `{}` that a
team wrote is never mistaken for a shipped file, and `touchmark check` rejects pack files
under 64 bytes.

## Identity is git's

A target file's identity is its git blob id, and what counts is what the repository
holds. In the hub's CI, touchmark reads the committed tree of each target and never checks
it out, so runner line-ending settings never matter. In a checkout, `status` and `apply`
judge a file whose bytes on disk are the blob in the index (committed or staged) by that
blob alone, the same blob `plan` reads. Any other file counts as it is on disk and as
`git hash-object` would store it after the target's line-ending and attribute filters:
a file you changed or never added, an LF blob that a Windows checkout with
`core.autocrlf=true` writes out with CRLF (so that checkout behaves the same as a Linux
one), and a file stored through a clean filter such as Git LFS.

**Line endings are content.** A file committed with CRLF line endings is another blob than
the pack's LF version, so it is `local`, even though an editor shows the same text. git
keeps those line endings on later commits, even with `core.autocrlf=true`; only
`git add --renormalize` converts them. Editing such a file to the pack's text therefore
does not make it the pack's: `status` may call the edit `current`, since `git hash-object`
converts it to LF, but the commit keeps CRLF and `plan` calls it `local`. touchmark leaves
the file byte for byte, in `apply` and in pull requests alike. To take the pack's version,
run `touchmark apply --adopt <path>`, which writes the pack's LF bytes, and commit the
result; the file is `current` from then on.

When `distribute` builds a commit, a path is also `unsafe` when its parent is a file, a
symlink or a submodule; when it differs from an existing path only by case; when
`.gitattributes` gives it a `filter` (Git LFS included) or a `working-tree-encoding`; or
when git would store the hub's content under another blob id in that target, so that a
checkout would see the file as changed. touchmark never writes through those.

## Consequences

- **Editing a file opts it out of updates.** From the first changed byte the file is
  `local`, and touchmark leaves it alone even when the pack changes it later. To receive
  updates again, restore the pack's version, or run `touchmark apply --adopt <glob>` in a
  checkout and commit the result. `ignore` is stronger than `--adopt`.
- **Deleting a managed file is not opting out.** The next run restores it. To stop
  receiving a file, add it to `ignore` in the opt-in file.
- **Seed files.** A pack can ship a skeleton a team is expected to fill in, such as a
  project profile. Once edited, it belongs to the repository and is never touched again.
- **A pack the hub stops assigning leaves its files.** They stay in the target, and
  `status` and `plan` list them as `orphaned`.
- **A renamed pack keeps its files** if the new pack lists the old name under `formerly`
  in `hub.yml`: the old name's history keeps proving ownership. Without it, the files
  look `local`.
- **Retiring a file** is deleting it from the pack: repositories that never changed it
  get a pull request deleting it; repositories that did keep theirs (`retired-local`).

## The safety net

`status` and `apply` run the same checks as `touchmark check` and refuse a hub that fails
them, with nothing written: a wrong `formerly`, for example, could otherwise make a
pack's files look retired. `apply` writes each file atomically, and stops between files
on Ctrl-C or SIGTERM without leaving a temporary file behind.
