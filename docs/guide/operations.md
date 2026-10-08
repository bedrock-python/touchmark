# One-off operations

Some actions override a safeguard: rebuilding a sync branch someone pushed to, proposing
content a team declined, closing many pull requests at once, taking over pull requests
that carry no marker. In CI they come only from `.touchmark/operations.yml` in the hub,
which goes through review like the packs. Only `distribute` on the default branch carries
them out.

```yaml
# .touchmark/operations.yml
version: 1

# Rebuild a paused sync branch. Commits others added to it are dropped.
recreate:
  - target: gh:acme/api
    head: 4b1d9e0c8f5a3b2e1d0c9b8a7f6e5d4c3b2a1f0e

# Propose again what a target declined by closing a sync pull request.
forget_declines:
  - target: gh:acme/docs
    pr: 44

# Let one run close more pull requests than the guard allows.
allow_mass_close: { max: 400, until: 2026-12-31 }

# Take over the open pull requests of a multi-gitter setup, without a marker.
adopt_unmarked: { until: 2026-12-31 }
```

A `target` is `[PROVIDER:]PATH`: here `gh` is the `id` of an entry of `providers` in
`hub.yml`. A hub with one provider, the single-provider shorthand included, writes the
path alone: `acme/api`.

**Every entry limits itself**, so a forgotten entry does nothing and the file needs no
cleanup to stay safe:

| Entry | Acts | Then |
|---|---|---|
| `recreate` | while the sync branch's head is exactly `head` | a new push to the branch makes it inert |
| `forget_declines` | once: touchmark marks pull request `pr` revoked; on Bitbucket Cloud, while the entry is present (see below) | inert |
| `allow_mass_close` | up to `max` closes in a run, until the end of `until` (UTC) | inert after the date |
| `adopt_unmarked` | until the end of `until` (UTC) | inert after the date |

`touchmark check` warns about entries past their date, so you can delete them.

## How to use them

1. Find what to write. The report of `plan` or `distribute` names the target and the
   reason, such as `gh:acme/api #31 (edited)`; the paused pull request shows the head.
2. Add the entry in a pull request to the hub. `plan` on that pull request shows what
   each entry would do: `applies`, `none` (in force, changes nothing), `expired` or
   `unknown`.
3. Merge it. The next `distribute` carries it out.

`target` is `<provider>:<path>`, or `<path>` with one provider, as touchmark prints it.

## Without the hub

A target's team can do two of these themselves, from the pull request:

- tick **Rebuild this branch** in a paused pull request: the same as `recreate`, for the
  head at that moment;
- tick **Propose this content again** in a declined pull request: the same as
  `forget_declines`, or simply reopen it.

On Bitbucket Cloud descriptions carry no tick boxes, and a declined pull request can
never be reopened or edited: a `recreate` entry is the only way to rebuild a paused
branch, and a `forget_declines` entry the only way, besides editing `packs` or `ignore`,
to have declined content proposed again. touchmark cannot mark the declined pull request revoked there, so the entry acts
on every run while it is present: keep it until the pull request it brings is merged or
closed. Removed earlier, the decline is in force again; an open pull request stays, as
memory never closes one, and its description then names the decline under *Previously
declined*.

## On your machine

A maintainer who holds the write key can run the same operations as flags of
`distribute`, outside CI only (with `CI` set they exit 2):

```sh
touchmark distribute --hub . --hub-fp github.com/712345678 \
  --recreate gh:acme/api@4b1d9e0c8f5a3b2e1d0c9b8a7f6e5d4c3b2a1f0e \
  --forget-declines gh:acme/docs#44 \
  --allow-mass-close 400
```

`--adopt-unmarked` takes over the multi-gitter pull requests, and `--allow-stale` runs
although the hub's HEAD is not the tip of its default branch in this clone. Add
`--dry-run` first: the report lists each operation and its effect.
