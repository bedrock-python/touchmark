# Reports

Every command that inspects targets prints a report on stdout: text by default, JSON with
`--format json`, and for `plan`, `distribute` and `doctor` also Markdown with
`--format markdown`. Errors and CI annotations go to stderr, so JSON output stays valid.

| Report | Commands | Schema | Files in CI |
|---|---|---|---|
| `report/v1` | `plan`, `distribute` | `touchmark schema report` | `touchmark-report.json`, `touchmark-report.md`; `distribute` also `touchmark-report.jsonl` |
| `doctor/v1` | `doctor` | `touchmark schema doctor` | `touchmark-doctor.json`, `touchmark-doctor.md` |
| `setup/v1` | `setup` | `touchmark schema setup` | — |
| `status/v1` | `status`, `apply` | `touchmark schema status` | — |
| `check/v1` | `check` | `touchmark schema check` | — |

What may change in these formats between releases, and what may not, is in
[Compatibility](compatibility.md).

`--report FILE` (`distribute`, `doctor`) also writes the JSON report to a file;
`--stream FILE` (`distribute`) writes one JSON line per finished target as the run goes,
`touchmark-report.jsonl` by default in CI, which survives a killed job.

In a public hub, a target that is not public is never named: its entry has an empty path
and repository id, and the text output only counts it.

## plan and distribute

```text
touchmark plan · hub acme-eng (github.com/712345678) @ 3f2a1c9 (PR #41) · touchmark 0.1.0
gh    github.com          read acme-read[bot]  write acme-write[bot]: not checked  resolve complete
corp  gitlab.example.com  read tm-reader       write tm-writer: not checked        resolve complete
scope: packs base, python changed → 11 of 11 targets (--all for every target)
opt_in: assumed: 2 targets without an opt-in file are opted in by targets.yml; an opt-in file with enabled: false opts one out

  open       4  corp:platform/api, gh:acme/billing, gh:acme/docs and 1 more
  update     1  gh:acme/sdk #1 (content)
  unchanged  1  gh:acme/api #1
  close      1  gh:acme/old #1 (no-diff)
  skipped    3  gh:acme/archive (archived), gh:acme/web (not-opted-in); 1 private in public hub
  blocked    1  gh:acme/cli #1 (branch-in-use)

Warnings
  gh:acme/tools: orphaned 1: docs/python.md
Cost  gh ≈ 16 writes (<1 min) · corp ≈ 2 writes (<1 min) · 1 run
```

In `plan` and `distribute --dry-run` an outcome means what `distribute` would do, and the
text says *would*. Every target has one outcome:

| Outcome | Reasons |
|---|---|
| `opened` | — |
| `updated` | `content`, `rebase`, `recreate`, `body`, `title`, `base-renamed` |
| `unchanged` | — |
| `closed` | `no-diff`, `opted-out`, `target-dropped`, `duplicate` |
| `declined` | — (the declined pull request is in `pr`) |
| `skipped` | `not-opted-in`, `opted-out`, `archived`, `disabled`, `empty`, `mirror`, `pending-deletion`, `prs-disabled`, `sha256`, `unsafe-opt-in`, `private-in-public-hub`, `superseded` |
| `blocked` | `edited`, `branch-taken`, `branch-in-use`, `opt-in-invalid`, `marker-invalid`, `rules:<rule>`, `permission:<what>`, `cannot-sign`, `archived`, `mass-close` |
| `deferred` | `rate-limit`, `deadline`, `rollout-limit`, `provider-down`, `interrupted`, `cooldown` |
| `failed` | `transient`, `auth`, `access`, `git`, `integrity`, `race`, `secret-exposure`, `internal` |

[Troubleshoot](../guide/troubleshooting.md) says what each reason means. `ops` is the
audit journal of every write `distribute` made: when, by which account, to which target,
and the branch head before and after.

<!-- generated: schema report -->

| Key | Type | Description |
|---|---|---|
| `schema` | `report/v1`, required | Report format: report/v1. |
| `command` | one of `plan`, `distribute`, required | The command that wrote the report. |
| `dry_run` | boolean | Set for distribute --dry-run: nothing was written, and outcomes and writes mean what distribute would do. |
| `engine` | string, required | touchmark's version. |
| `hub` | object, required | The hub the run read. |
| `hub.id` | string, required | hub.yml's id. |
| `hub.fingerprint` | string, required | The hub's host and immutable repository id, e.g. github.com/712345678. |
| `hub.commit` | string, required | Full commit id: 40 or 64 lowercase hex digits. |
| `hub.pr` | integer ≥ 1 | The hub pull request plan ran on. |
| `outcome` | one of `completed`, `superseded`, required | completed, or superseded when the hub's default branch had moved on and nothing was inspected or written. |
| `strict` | boolean, required | The run had --strict: blocked and deferred targets and an incomplete resolve exit with code 3. |
| `scope` | object | The part of the fleet a plan in a hub pull request processed in full: every target (all), the targets whose final pack list holds a pack the pull request changes (packs), or none, when it changes only hub files no target receives (hub). The targets outside the scope are only counted. |
| `scope.mode` | one of `all`, `packs`, `hub`, required | — |
| `scope.reason` | string | What decided the mode: --all, the files every target depends on that changed, or why the scope could not be computed. |
| `scope.packs` | list of strings | The packs the pull request changes. |
| `scope.base` | string | Full commit id: 40 or 64 lowercase hex digits. |
| `scope.processed` | integer ≥ 0, required | The targets the plan processed in full. |
| `scope.total` | integer ≥ 0, required | The targets the resolve found. |
| `assume_opt_in` | boolean | plan --assume-opt-in: for this report only, every target without an opt-in file, or whose opt-in file is not a regular file or is too large, counts as opted in; one whose opt-in file says enabled: false stays opted out. |
| `providers` | list of objects, required | The hub's providers, in hub.yml order. |
| `providers[].id` | string, required | — |
| `providers[].type` | one of `github`, `gitlab`, `gitea`, `forgejo`, required | — |
| `providers[].host` | string, not empty, required | The provider's host, with a port when it has one. |
| `providers[].reader` | string | Login of the read account. |
| `providers[].writer` | string | Login of the write account. |
| `providers[].write_check` | string, required | What is known about the write account's access, e.g. "not checked". |
| `providers[].resolve_complete` | boolean, required | Every organisation and group of targets.yml was listed in full. |
| `providers[].missing` | list of strings | Repositories targets.yml names that the platform does not know, as &lt;provider&gt;:&lt;path&gt;. |
| `providers[].error` | string | Why the provider was unavailable. |
| `targets` | list of objects, required | Every target, sorted by provider and path. |
| `targets[].provider` | string, required | — |
| `targets[].host` | string, not empty, required | — |
| `targets[].repo_id` | string, required | The repository's immutable id on the platform. |
| `targets[].path` | string, required | The repository's canonical path. |
| `targets[].outcome` | one of `opened`, `updated`, `unchanged`, `closed`, `declined`, `skipped`, `blocked`, `deferred`, `failed`, required | — |
| `targets[].reason` | string, not empty | — |
| `targets[].pr` | object | — |
| `targets[].pr.number` | integer ≥ 1, required | — |
| `targets[].pr.url` | string | — |
| `targets[].pr.state` | one of `open`, `closed`, `merged` | — |
| `targets[].key` | string | Content key of the changes; absent when there are none. |
| `targets[].packs` | list of strings | The packs the target gets, in layering order. |
| `targets[].opt_in_assumed` | boolean | The target was processed as if it had an empty opt-in file: it has none, or, with plan --assume-opt-in, one that is not a regular file or is too large. opt_in_assumed_by says why. |
| `targets[].opt_in_assumed_by` | one of `targets.yml`, `--assume-opt-in` | Why opt_in_assumed is set: targets.yml (an entry with opt_in: assumed selects the target, for real) or --assume-opt-in (plan's flag, for the report only). |
| `targets[].changes` | object, required | The changes D holds, by action. |
| `targets[].changes.create` | integer ≥ 0, required | — |
| `targets[].changes.update` | integer ≥ 0, required | — |
| `targets[].changes.delete` | integer ≥ 0, required | — |
| `targets[].changes.chmod` | integer ≥ 0, required | — |
| `targets[].orphaned` | list of strings | Files a pack the target no longer gets shipped, still unchanged in the target. |
| `targets[].warnings` | list of strings | — |
| `targets[].writes` | integer ≥ 0, required | Writes for this target (HTTP writes of the API and git pushes): estimated by plan, counted by distribute. |
| `ops` | list of objects, required | Audit journal of every mutation distribute made; empty for plan. |
| `ops[].time` | string (date-time), required | — |
| `ops[].account` | string, required | Login of the account that wrote. |
| `ops[].target` | string, required | The target as &lt;provider&gt;:&lt;path&gt;. |
| `ops[].kind` | one of `push`, `create-pr`, `edit-pr`, `comment`, `delete-branch`, `create-label`, `api-commit`, `update-refs`, `delete-ref`, required | What the mutation did: a git push, a pull request created or edited, a comment, a branch deleted, a label created, or a step of an API commit (the commit, the refs moved, a stage ref deleted). |
| `ops[].pr` | integer ≥ 1 | — |
| `ops[].before` | string | What the mutation replaced, e.g. the branch head before a push. |
| `ops[].after` | string | What the mutation left, e.g. the branch head after a push. |
| `warnings` | list of strings, required | Warnings about the run as a whole; warnings about one target are in the target. |
| `summary` | object, required | Targets per outcome, every outcome present. |
| `summary.opened` | integer ≥ 0, required | — |
| `summary.updated` | integer ≥ 0, required | — |
| `summary.unchanged` | integer ≥ 0, required | — |
| `summary.closed` | integer ≥ 0, required | — |
| `summary.declined` | integer ≥ 0, required | — |
| `summary.skipped` | integer ≥ 0, required | — |
| `summary.blocked` | integer ≥ 0, required | — |
| `summary.deferred` | integer ≥ 0, required | — |
| `summary.failed` | integer ≥ 0, required | — |
| `sweep` | object, required | The sweep of stale pull requests. |
| `sweep.ran` | boolean, required | — |
| `sweep.complete` | boolean, required | — |
| `sweep.failed` | boolean, required | — |
| `sweep.reason` | string | Why the sweep did not run. |
| `cost` | map of provider to integer ≥ 0, required | Writes per provider id (HTTP writes of the API and git pushes): estimated by plan, counted by distribute. |
| `estimate` | object | What the writes of a plan or a dry run take: per provider the writes and the time its write limits give them, and the runs the rollout needs under limits.max_new_prs_per_run. |
| `estimate.new_prs` | integer ≥ 0, required | Pull requests the rollout opens, in this run and the next ones. |
| `estimate.max_new_prs_per_run` | integer ≥ 0, required | — |
| `estimate.runs` | integer ≥ 0, required | Runs distribute needs to open them all: 0 when nothing is written, or when max_new_prs_per_run is 0 and some would open. |
| `estimate.providers` | map of provider to object, required | — |
| `estimate.providers.<provider>.writes` | integer ≥ 0, required | This run's writes by the git path: every push one write. |
| `estimate.providers.<provider>.seconds` | integer ≥ 0, required | — |
| `estimate.providers.<provider>.api_writes` | integer ≥ 0 | This run's writes when every push that needs or may need a signature goes through the platform's API commit; absent when that changes nothing. |
| `estimate.providers.<provider>.api_seconds` | integer ≥ 0 | — |
| `estimate.providers.<provider>.total_writes` | integer ≥ 0 | The writes of every run the rollout needs, by the git path; absent when it fits one run. |
| `estimate.providers.<provider>.total_seconds` | integer ≥ 0 | — |
| `paths` | list of objects | What the pushes of a plan or a dry run change, by path across the targets: sensitive paths first, then deletions, updates, mode changes and additions, each by path. |
| `paths[].action` | one of `add`, `update`, `delete`, `chmod`, required | — |
| `paths[].path` | string, not empty, required | — |
| `paths[].sensitive` | boolean | The path matches a sensitive pattern (built-in or hub.yml's sensitive_paths) or is written executable. |
| `paths[].targets` | integer ≥ 1, required | How many targets the change goes to. |
| `operations` | list of objects | What each one-off operation does in a plan or a dry run: the entries of .touchmark/operations.yml in file order (recreate, forget_declines, allow_mass_close, adopt_unmarked), then the operation flags of a local run. |
| `operations[].kind` | one of `recreate`, `forget_declines`, `allow_mass_close`, `adopt_unmarked`, required | — |
| `operations[].flag` | boolean | The operation is a flag of a local run, not an entry of operations.yml. |
| `operations[].target` | string, not empty | The entry's target as operations.yml writes it; absent for a target a public hub's report does not name. |
| `operations[].provider` | string | — |
| `operations[].pr` | integer ≥ 1 | The pull request the entry names (forget_declines) or whose branch it rebuilds (recreate). |
| `operations[].head` | string | Full commit id: 40 or 64 lowercase hex digits. |
| `operations[].max` | integer ≥ 1 | — |
| `operations[].until` | string | The last day the entry is in force: YYYY-MM-DD, UTC. |
| `operations[].effect` | one of `applies`, `none`, `expired`, `unknown`, required | applies: the entry changes what the run does; none: it is in force and changes nothing; expired: its date has passed; unknown: the run cannot tell (its target was not inspected). |
| `operations[].detail` | string, not empty, required | — |
| `notes` | map of name to string | Free-form remarks about the run, by name. |

<!-- end generated -->

## doctor

Each check has a name, a status (`ok`, `warn`, `fail`, `unknown`) and a detail, for the
hub, for each provider's write identity, and for each target. See
[Check the write account](../guide/doctor.md).

<!-- generated: schema doctor -->

| Key | Type | Description |
|---|---|---|
| `schema` | `doctor/v1`, required | Report format: doctor/v1. |
| `command` | `doctor`, required | — |
| `engine` | string, required | touchmark's version. |
| `hub` | object, required | The hub the run read. |
| `hub.id` | string, required | hub.yml's id. |
| `hub.fingerprint` | string, required | The hub's host and immutable repository id, e.g. github.com/712345678. |
| `hub.commit` | string, required | Full commit id: 40 or 64 lowercase hex digits. |
| `strict` | boolean, required | The run had --strict: a check that warns or is unknown, or an incomplete resolve, exits with code 3. |
| `hub_token` | boolean, required | doctor --hub-token read, with a maintainer's token, where the hub keeps its secrets. |
| `hub_checks` | list of objects, required | The checks of the hub itself. |
| `hub_checks[].check` | string, required | What was checked, e.g. access, token-expiry, rules, markers, hub-hidden, write-isolation, key-location. |
| `hub_checks[].status` | one of `ok`, `warn`, `fail`, `unknown`, required | — |
| `hub_checks[].detail` | string | What the check found; absent for a target the report does not name. |
| `providers` | list of objects, required | The hub's providers, in hub.yml order. |
| `providers[].id` | string, required | — |
| `providers[].type` | one of `github`, `gitlab`, `gitea`, `forgejo`, required | — |
| `providers[].host` | string, not empty, required | The provider's host, with a port when it has one. |
| `providers[].writer` | string | Login of the writer hub.yml names. |
| `providers[].self` | string | Login the write credential acts as. |
| `providers[].resolve_complete` | boolean, required | Every organisation and group of targets.yml was listed in full. |
| `providers[].missing` | list of strings | Repositories targets.yml names that the platform does not know, as &lt;provider&gt;:&lt;path&gt;. |
| `providers[].error` | string | Why the provider could not be checked. |
| `providers[].checks` | list of objects, required | The checks of the write identity. |
| `providers[].checks[].check` | string, required | What was checked, e.g. access, token-expiry, rules, markers, hub-hidden, write-isolation, key-location. |
| `providers[].checks[].status` | one of `ok`, `warn`, `fail`, `unknown`, required | — |
| `providers[].checks[].detail` | string | What the check found; absent for a target the report does not name. |
| `targets` | list of objects, required | Every target, sorted by provider and path; targets a public hub does not name come last within their provider. |
| `targets[].provider` | string, required | — |
| `targets[].host` | string, not empty, required | — |
| `targets[].repo_id` | string, required | The repository's immutable id on the platform; empty for a target the report does not name. |
| `targets[].path` | string, required | The repository's canonical path; empty for a target the report does not name. |
| `targets[].skipped` | string, not empty | Why the target was not checked: archived, disabled, empty, mirror, pending-deletion, prs-disabled, sha256, not-opted-in, opted-out, unsafe-opt-in, or deferred:&lt;reason&gt; (the run could not check the target; an unknown check says why). |
| `targets[].checks` | list of objects, required | — |
| `targets[].checks[].check` | string, required | What was checked, e.g. access, token-expiry, rules, markers, hub-hidden, write-isolation, key-location. |
| `targets[].checks[].status` | one of `ok`, `warn`, `fail`, `unknown`, required | — |
| `targets[].checks[].detail` | string | What the check found; absent for a target the report does not name. |
| `warnings` | list of strings, required | Warnings about the run as a whole. |
| `summary` | object, required | Checks per status, every status present. |
| `summary.ok` | integer ≥ 0, required | — |
| `summary.warn` | integer ≥ 0, required | — |
| `summary.fail` | integer ≥ 0, required | — |
| `summary.unknown` | integer ≥ 0, required | — |

<!-- end generated -->

## setup

Each step has a name, a status (`ok`, `done`, `would`, `warn`, `manual`, `unknown`,
`fail`) and a detail, and `next` lists what is left to do, in order. No secret ever
appears in it. See [Set up a hub's platform](../guide/setup.md).

<!-- generated: schema setup -->

| Key | Type | Description |
|---|---|---|
| `schema` | `setup/v1`, required | Report format: setup/v1. |
| `command` | `setup`, required | — |
| `engine` | string, required | touchmark's version. |
| `platform` | one of `github`, `gitlab`, required | The platform set up. |
| `host` | string, required | The platform's host, with a port that is not the scheme's default. |
| `hub` | string, required | The hub repository's path on the platform. |
| `hub_id` | string | The hub repository's immutable id, the second half of its fingerprint. |
| `group` | string | GitLab: the full path of the group whose projects are the targets. |
| `org` | string | GitHub: the account that owns the hub and the Apps. |
| `isolation` | one of `platform`, `external`, required | security.write_isolation of hub.yml: setup sets up isolated write keys only. |
| `dry_run` | boolean, required | The run had --dry-run: it read the platform and changed nothing; would steps say what it would change. |
| `accounts` | one of `service-account`, `group-access-token`, `app` | What the reader and the writer are: GitLab service accounts or group access tokens, GitHub Apps. |
| `reader` | string | The reader's login, when known. |
| `writer` | string | The writer's login, when known: hub.yml's writer must name it. |
| `steps` | list of objects, required | The steps in the order they ran. Steps named "check &lt;name&gt;" are the checks of doctor --hub-token on where the hub keeps its keys, made after the changes. |
| `steps[].step` | string, not empty, required | The step's name, such as writer-variable or check key-location. |
| `steps[].status` | one of `ok`, `done`, `would`, `warn`, `manual`, `unknown`, `fail`, required | ok: as wanted, nothing written; done: changed in this run; would: --dry-run would change it; warn: as wanted, with a caveat; manual: left to the person (run setup again once done); unknown: could not be read; fail: failed, or found what setup must not fix. |
| `steps[].detail` | string | — |
| `steps[].commands` | list of strings | Shell commands the person runs to finish the step, such as gh secret set for a GitHub App's private key. |
| `next` | list of strings, required | What the person does after this run, in order. |
| `summary` | object, required | Steps per status, every status present. |
| `summary.ok` | integer ≥ 0, required | — |
| `summary.done` | integer ≥ 0, required | — |
| `summary.would` | integer ≥ 0, required | — |
| `summary.warn` | integer ≥ 0, required | — |
| `summary.manual` | integer ≥ 0, required | — |
| `summary.unknown` | integer ≥ 0, required | — |
| `summary.fail` | integer ≥ 0, required | — |

<!-- end generated -->

## status and apply

`--format json` prints the hub (its directory, `id` and commit), the target (its
directory, its reference in `targets.yml`, its opt-in file, whether it opted in, and
`opt_in`, which says why: `file`, the opt-in file is there; `assumed`, there is none and
a `repo:` entry with `opt_in: assumed` subscribes it; `flag`, there is none and
`--assume-opt-in` counts it as opted in; `opted-out`, the file says
`enabled: false`; `none`, neither), the
pack selection and where each pack came from, one entry per managed path with its
`state`, `action`, `pack` and blob ids, a `summary` counting each state, and `warnings`:

```json
{
  "schema": "status/v1",
  "command": "status",
  "dry_run": false,
  "hub": { "dir": "/src/engineering-assets", "id": "acme-eng", "commit": "ffae0953c140298cf045e600f15710104e9d5d0d" },
  "target": { "root": "/src/billing", "ref": "acme/billing", "opt_in_file": ".engineering-assets.yml", "opted_in": true,
              "opt_in": "file" },
  "selection": { "packs": ["agents"], "complete": true, "unresolved": [], "sources": { "agents": ["defaults"] } },
  "entries": [
    { "path": "AGENTS.md", "state": "local", "action": "keep", "pack": "agents",
      "from": "", "to": "", "mode": "", "detail": "", "after_deletes": false }
  ],
  "summary": { "missing": 0, "current": 1, "outdated": 0, "local": 1, "ignored": 0, "retired": 0,
               "retired_local": 0, "unsafe": 0, "orphaned": 0, "changes": 0, "done": 0, "skipped": 0, "failed": 0 },
  "warnings": []
}
```

`apply` adds `results`, one per entry whose action is not `keep`, with its `outcome`
(`done`, `skipped`, `failed`); `apply --dry-run` has none.

<!-- generated: schema status -->

| Key | Type | Description |
|---|---|---|
| `schema` | `status/v1`, required | Report format: status/v1. |
| `command` | one of `status`, `apply`, required | — |
| `dry_run` | boolean, required | apply --dry-run: the plan only, nothing written. Always false for status. |
| `hub` | object, required | The hub the report was made from. |
| `hub.dir` | string, required | The hub's directory. |
| `hub.id` | string, required | hub.yml's id; empty for a hub without hub.yml. |
| `hub.commit` | string, required | The hub commit read; empty when touchmark stopped before reading it. |
| `target` | object, required | The target working tree. |
| `target.root` | string, required | The target's working tree. |
| `target.ref` | string, required | The target in targets.yml, path or provider:path; empty when it could not be determined. |
| `target.opt_in_file` | string, required | The opt-in file's path in the target. |
| `target.opted_in` | boolean, required | The target gets packs: opt_in is file, assumed or flag. |
| `target.opt_in` | one of `file`, `assumed`, `flag`, `opted-out`, `none`, required | Why the target is opted in or not. file: the opt-in file is there; assumed: there is none, and a repo: entry with opt_in: assumed subscribes it; flag: there is none, and --assume-opt-in counts it as opted in; opted-out: the file says enabled: false; none: neither. |
| `selection` | object, required | The resolved pack list; null when the target has not opted in. |
| `selection.packs` | list of strings, required | The packs the target gets, requires included. |
| `selection.complete` | boolean, required | Every pack named was found in the hub. |
| `selection.unresolved` | list of strings, required | Packs named that the hub does not have. |
| `selection.sources` | map of name to list of strings, required | Per pack, where it was selected: the opt-in file, targets.yml, defaults or requires &lt;pack&gt;. |
| `entries` | list of objects, required | The whole plan in plan order, one entry per managed path, current entries included. |
| `entries[].path` | string, not empty, required | The path in the target. |
| `entries[].state` | one of `missing`, `current`, `outdated`, `local`, `ignored`, `retired`, `retired-local`, `unsafe`, `orphaned`, required | The file's state in the target. |
| `entries[].action` | one of `keep`, `create`, `update`, `delete`, `adopt`, `chmod`, required | What apply does with it. |
| `entries[].pack` | string, required | The pack that ships the path, or shipped it. |
| `entries[].from` | string, required | The blob id the target has; empty when it has none. |
| `entries[].to` | string, required | The blob id apply writes; empty for keep and delete. |
| `entries[].mode` | one of ``, `100644`, `100755`, required | The file mode apply writes; empty when it writes nothing. |
| `entries[].detail` | string, required | Why, for states and actions that need a reason. |
| `entries[].after_deletes` | boolean, required | Written after the deletes, because a path it needs is deleted first. |
| `results` | list of objects | apply without --dry-run only: what it did with each entry whose action is not keep, in execution order. |
| `results[].path` | string, not empty, required | — |
| `results[].action` | one of `keep`, `create`, `update`, `delete`, `adopt`, `chmod`, required | — |
| `results[].state` | one of `missing`, `current`, `outdated`, `local`, `ignored`, `retired`, `retired-local`, `unsafe`, `orphaned`, required | — |
| `results[].outcome` | one of `done`, `skipped`, `failed`, required | — |
| `results[].detail` | string, required | The reason for skipped and the error for failed. |
| `summary` | object, required | Entries per state, the changes (entries whose action is not keep) and, for apply, the outcomes. |
| `summary.missing` | integer ≥ 0, required | — |
| `summary.current` | integer ≥ 0, required | — |
| `summary.outdated` | integer ≥ 0, required | — |
| `summary.local` | integer ≥ 0, required | — |
| `summary.ignored` | integer ≥ 0, required | — |
| `summary.retired` | integer ≥ 0, required | — |
| `summary.retired_local` | integer ≥ 0, required | — |
| `summary.unsafe` | integer ≥ 0, required | — |
| `summary.orphaned` | integer ≥ 0, required | — |
| `summary.changes` | integer ≥ 0, required | — |
| `summary.done` | integer ≥ 0, required | — |
| `summary.skipped` | integer ≥ 0, required | — |
| `summary.failed` | integer ≥ 0, required | — |
| `warnings` | list of strings, required | — |

<!-- end generated -->

## check

`--format json` prints the hub, its packs, and every error and warning; `errors` is empty
when the hub passes.

<!-- generated: schema check -->

| Key | Type | Description |
|---|---|---|
| `schema` | `check/v1`, required | Report format: check/v1. |
| `command` | `check`, required | — |
| `hub` | object, required | The hub checked. |
| `hub.dir` | string, required | The hub's directory. |
| `hub.id` | string, required | hub.yml's id; empty for a hub without hub.yml. |
| `hub.commit` | string, required | The hub commit read; empty when touchmark stopped before reading it. |
| `packs` | list of strings, required | The hub's packs. |
| `errors` | list of strings, required | What makes check exit 2; empty when the hub passes. |
| `warnings` | list of strings, required | — |

<!-- end generated -->
