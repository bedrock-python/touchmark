# Packs and opt-in

A **pack** is a directory in the hub's `packs/`, laid out like the target repository:
`packs/agents/AGENTS.md` becomes `AGENTS.md`, `packs/claude/.claude/settings.json`
becomes `.claude/settings.json`. Every file in it is delivered. Only `packs/` is
delivered; the rest of the hub configures it.

```text
packs/
  agents/
    AGENTS.md
    .agents/project.md
    .agents/prompts/review.md
  claude/
    CLAUDE.md
    .claude/settings.json
```

A pack's name is lowercase words of letters and digits joined by single hyphens, at
most 64 characters.

## Which packs a target gets

The hub assigns packs in `targets.yml`; the target adds packs in its opt-in file. The
final list, in order:

1. `defaults.packs` of `targets.yml`;
2. the `packs` of every entry of `targets.yml` that matches the target, in file order;
3. the `packs` of the target's opt-in file.

Duplicates are dropped; the first occurrence stays. Each pack's `requires` from
`hub.yml` is added and placed before it. When two packs ship the same path, the **later**
one wins: a pack can override a file of a pack it requires.

```yaml
# hub.yml
packs:
  agents:
    description: AGENTS.md for every coding agent, a project profile to fill in
  claude:
    description: Claude Code instructions, skills and settings
    requires: [agents]
  python-library:
    formerly: [python-lib]
```

The hub sets the minimum: a target can add packs and ignore paths, but it can't remove
a pack the hub assigns. The central choice stays with the organisation, and every
exception is visible in the target itself.

## Opt-in

A target receives nothing until it has the opt-in file, `.engineering-assets.yml` at its
root (`opt_in_file` in `hub.yml` renames it). An empty file is consent.

```yaml
version: 1
packs: [claude]
ignore:
  - .claude/settings.json
  - docs/guidelines/**
```

- `packs` adds packs. A pack that does not exist in the hub blocks the target with
  `opt-in-invalid`.
- `ignore` lists paths or globs touchmark must never create, update or delete there. A
  path without glob characters covers everything beneath it; `**` matches any number of
  directories.
- Editing `packs` or `ignore` is a new choice of the team, so it lifts every decline the
  repository made: see [memory](memory.md).

The opt-in file belongs to each repository: `touchmark check` rejects a pack that ships
it.

## Renaming and retiring

- **Rename** a pack by moving its directory and listing the old name under `formerly`.
  The old name's history then counts as the new pack's own, and files it shipped stay
  managed. `check` rejects a `formerly` name that is still a current pack, or that two
  packs claim.
- **Retire a file** by deleting it from the pack: targets whose copy is still the hub's
  get it deleted (`retired`); targets that changed it keep it (`retired-local`).
- **Stop assigning a pack** by taking it out of `targets.yml`: its files stay in the
  targets, listed as `orphaned`.

## What makes a good pack

- **Shared instructions, local facts.** Keep tracker keys, branch patterns, the pull
  request language and build commands out of shared files. Put them in a project profile
  each team fills in (a seed file), and have the shared instructions point to it.
- **Seed files.** A file a team is expected to fill in ships as a skeleton. Once edited,
  it is the repository's own.
- **At least 64 bytes per file**, regular files only: `check` rejects smaller files,
  symlinks and submodules, a top-level `.lfsconfig`, and two paths that differ only by
  case.
- **No descriptions inside the pack.** Every file in it is delivered: describe a pack in
  `hub.yml`, not in a README inside it.

See [Write packs](../guide/packs.md) for the workflow.
