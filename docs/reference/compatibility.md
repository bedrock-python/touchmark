# Compatibility

touchmark is at 0.x and stays there for now. This page says what a hub, a target and a
script around touchmark can rely on from one release to the next.

## Versions

A patch release (`fix:`) breaks nothing listed below. A minor release (`feat:`) adds
things. Before 1.0, a breaking change also bumps the minor version (`feat!:` or a
`BREAKING CHANGE:` footer), and the changelog lists it under **⚠ BREAKING CHANGES**.
A hub pins touchmark to a release (the template pins the Action by commit and the image by
digest), so a new minor version arrives as a pull request: read that section of the
changelog before you merge it.

## Stable

- **Configuration files at `version: 1`:** `hub.yml`, `targets.yml`, the opt-in file and
  `.touchmark/operations.yml`. A file that a release accepts keeps working, with the same
  meaning, in every later release that reads `version: 1`. Changes only add: new optional
  keys and new values. An older release rejects a key it does not know, so a new key needs
  the release that added it; the reference says which release that is. The opt-in file
  lives in repositories the hub does not control, so it gains keys rarely. Removing a key
  or changing what it means takes a `version: 2` of that file, and touchmark then keeps
  reading `version: 1`, with a warning.
- **Versioned JSON reports:** `report/v1` (`plan`, `distribute`, and the lines of
  `--stream`), `doctor/v1`, `setup/v1`, `status/v1` (`status`, `apply`) and `check/v1`.
  Changes only add: new fields, and new values where a field lists its values (outcomes,
  reasons, states, check and step names). Ignore fields you do not know and treat values
  you do not know as opaque. Anything else is a new version, such as `report/v2`, released
  as a breaking change. Validate a report against the schema of the release that wrote it
  (`touchmark schema <name>`): the schemas published on `master` describe the next release
  and reject fields they do not know.
- **What touchmark leaves in repositories:** the marker in pull request bodies
  (`touchmark:v1`), the commit trailers (`Touchmark-Hub`, `Touchmark-Stream`,
  `Touchmark-Content`, `Touchmark-Hub-Commit`), the control lines of a pull request body,
  the marker of the plan comment, and the sync branch's name. Every release reads what an
  earlier one wrote. The marker's payload is frozen: a change to it is a `touchmark:v2`
  marker, which releases read alongside `v1`.
- **The command line and CI:** command and flag names, flag defaults, exit codes,
  environment variable names, the names of the report files in CI, the GitHub Action's
  inputs, and the image's tags (`X.Y.Z`, `X.Y`, `latest`) and what the hub template's jobs
  use in it (`sh`, `touchmark` on the `PATH`, user 65532).

## Not stable

Text and Markdown output, step summaries and annotations, the wording of pull request
bodies and comments, warnings and error messages, the output of `manifest` and `migrate`,
and the Go packages, `schemas` included. Parse the JSON reports, not the text.

## Deprecation

A deprecated key, flag or input is marked deprecated in its schema and in the reference,
with what replaces it. Using it warns: in `check`, and in the warnings of `status`, `plan`
and `distribute`, which reach the hub's pull request comment and the step summary. The
changelog announces the deprecation. A flag or an input keeps working for at least two
minor releases and six months; a configuration key keeps working for as long as touchmark
reads that version of its file. Removing either is a breaking change.

Deprecated now:

| What | Since | Instead |
|---|---|---|
| `commit.author` in `hub.yml` | before 0.1.0 | Nothing: the writer account authors every commit. The key is ignored, with a warning. |
| a bare `repos:` list in `targets.yml` | before 0.1.0 | `version: 1` with `targets:` entries. The list is read as `repo:` entries, with a warning. |
