# The opt-in file

`.engineering-assets.yml` at the root of a target repository (`opt_in_file` in `hub.yml`
renames it). Its presence is the repository's consent: nothing is delivered without it,
and an empty file is enough, unless the hub subscribes the repository with `opt_in:
assumed` in [`targets.yml`](targets.md#opt-in). `touchmark schema opt-in` prints the
JSON Schema.

```yaml
version: 1
packs: [claude]                 # in addition to what the hub assigns
ignore:
  - .claude/settings.json       # this repository keeps its own
  - .agents/guidelines/**
```

- `enabled: false` opts the repository out, whatever `targets.yml` says: it is skipped
  (`opted-out`), and its open sync pull request is closed as `opted-out`. Files already
  merged stay. The default is `true`.
- **Deleting the file** opts a repository out too, unless the hub subscribes it
  (`opt_in: assumed`): such a repository returns to what the hub assigns, so only
  `enabled: false` opts it out.
- `packs` adds packs; it cannot remove one the hub assigns. A pack the hub does not have
  blocks the target (`opt-in-invalid`).
- `ignore` patterns: a path without glob characters covers everything beneath it; `*`
  matches within a path segment, `**` any number of directories; paths compare without
  regard to case.
- Editing `packs` or `ignore` lifts every decline the repository made; comments and
  formatting don't. See [memory](../concepts/memory.md).
- The file is read from the default branch through the platform's API: a regular file of
  at most 64 KiB, strict YAML with bounded anchors and aliases. An invalid file blocks the
  target (`opt-in-invalid`); a symlink or a directory in its place skips it
  (`unsafe-opt-in`). A file without `version` is accepted.
- No pack can ship this file: `check` rejects it.

## Keys

<!-- generated: schema opt-in -->

| Key | Type | Description |
|---|---|---|
| `version` | `1` | Format version. Only 1 exists. |
| `enabled` | boolean | false opts the repository out, even when the hub subscribes it (opt_in: assumed in targets.yml): it gets nothing, and the hub's open pull requests here are closed. Default: true. |
| `packs` | list of strings, each up to 64 characters | Packs to add to those the hub assigns. Packs the hub assigns cannot be removed here. |
| `ignore` | list of strings, at most 1000 | Paths the hub must never create, update or delete here. A path without glob characters covers everything beneath it; \*\* matches any number of directories. At most 1000 patterns. |

<!-- end generated -->
