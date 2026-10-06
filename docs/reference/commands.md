# Commands

The help of every command, as `touchmark <command> --help` prints it. The blocks between
the `generated` markers in this page's source come from the code (`go run ./scripts/docs`),
and a test fails when they fall behind it. Flags take one dash or two (`-hub` is
`--hub`) and a value after a space or an `=`. A boolean flag is on when named; one whose
help says `(default true)` is on unless turned off with `=false` (`--schedules=false`).

<!-- generated: help -->

```text
touchmark keeps shared files in sync across many repositories.

Usage: touchmark <command> [flags]

Commands:
  check      validate hub.yml, targets.yml and every pack
  plan       report what distribute would do in every target; changes nothing
  distribute open, update and close the pull requests of every target (--dry-run: report what it would do)
  doctor     check the write identity against every target, and with --hub-token where the hub keeps its write key; changes nothing
  probe      exit 2 when this job sees a write credential or signing key (the isolation probe)
  migrate    print hub.yml, targets.yml and operations.yml for a hub that replaces a multi-gitter setup on GitLab
  setup      set up the hub's platform with a maintainer's token from $TOUCHMARK_HUB_TOKEN: the reader and the writer, the write key kept to the default branch, the hub's protection; changes only what differs
  status     list every managed path of the target and its state; changes nothing
  apply      apply the packs to the target's working tree
  manifest   print the ownership manifest built from the hub's history
  schema     print the JSON Schema of a configuration file or of a report
  version    print the version

Run touchmark <command> --help for the flags of a command.
Exit codes: 0 success, 1 some action failed, 2 usage, configuration or guard error,
3 plan or distribute --strict found a blocked or deferred target or an incomplete resolve,
doctor --strict a check that warns or is unknown, or setup a step left to you.
```

<!-- end generated -->

## In the hub

### check

Validates `hub.yml`, `targets.yml`, `.touchmark/operations.yml`, the write isolation
probe of the GitHub Actions workflows, the history and every pack, and reports every
finding rather than the first. Entries of `operations.yml` past their date are warnings.
Exit 2 on an error.

<!-- generated: help check -->

```text
Usage: touchmark check [--hub DIR] [--worktree] [--format text|json]

Validate hub.yml, targets.yml and every pack.

Flags:
  --format FORMAT  output FORMAT: text or json (default text)
  --hub DIR        the hub checkout DIR (default $TOUCHMARK_HUB)
  --worktree       read configs and packs from the hub's working tree instead of its HEAD commit
```

<!-- end generated -->

### plan

Runs with the read account on hub pull requests. In a pull request it plans in full the
targets whose packs the pull request changes, and every target when it changes `hub.yml`,
`targets.yml`, `.touchmark/`, `schemas/` or a file `hub.yml` names. Without any read
secret (Dependabot's pull requests, forks) it runs offline: it reads no target, reports
the hub's side with a warning, and exits 0, or 3 with `--strict`. In CI it exits 2 when
it can see a write key.

<!-- generated: help plan -->

```text
Usage: touchmark plan [--hub DIR] [--worktree] [--only REF]... [--hub-fp HOST/ID] [--strict] [--all] [--comment] [--assume-opt-in] [--format text|json|markdown]

Report what distribute would do in every target; changes nothing.

Flags:
  --all             in a hub pull request, process every target, not only those of the packs it changes
  --assume-opt-in   plan every target as if it had an empty opt-in file when it has none (for the report only)
  --comment         keep one comment with the report in the hub pull request, through the CI's own token
  --format FORMAT   output FORMAT: text, json or markdown (default text)
  --hub DIR         the hub checkout DIR (default $TOUCHMARK_HUB)
  --hub-fp HOST/ID  the hub's fingerprint HOST/ID: the host (with a port other than 443) of its server URL and its repository id, e.g. github.com/712345678 (CI provides it)
  --only REF        plan only the target REF, as [PROVIDER:]PATH; repeatable or comma-separated
  --strict          exit 3 when a target is blocked or deferred or targets.yml does not fully resolve
  --worktree        read configs and packs from the hub's working tree instead of its HEAD commit
```

<!-- end generated -->

### distribute

Runs with the write account on the hub's default branch. It never touches pull requests
it did not open, or a branch that someone else's pull request uses. The flags marked
*local only* exit 2 in CI: use [operations.yml](operations.md) there.

<!-- generated: help distribute -->

```text
Usage: touchmark distribute [--hub DIR] [--dry-run] [--only REF]... [--hub-fp HOST/ID] [--deadline DURATION] [--strict] [--format text|json|markdown] [--report FILE] [--stream FILE] [local operation flags]

Open, update and close the pull requests of every target (--dry-run: report what it would do).

Flags:
  --adopt-unmarked             local only: take over the open pull requests without a marker on the branch aliases (a multi-gitter setup's)
  --allow-mass-close MAX       local only: let this run close up to MAX pull requests
  --allow-stale                local only: run although the hub's HEAD is not the tip of origin's default branch in this clone
  --deadline DURATION          start no target after DURATION, e.g. 50m (default: CI_JOB_TIMEOUT less 5m on GitLab, 5h30m on GitHub Actions, none elsewhere; 0 for none)
  --dry-run                    read and check everything with the write credential, write nothing, and report what distribute would do
  --forget-declines TARGET#PR  local only: propose again what TARGET#PR, a declined pull request, carried (repeatable)
  --format FORMAT              output FORMAT: text, json or markdown (default text)
  --hub DIR                    the hub checkout DIR (default $TOUCHMARK_HUB)
  --hub-fp HOST/ID             the hub's fingerprint HOST/ID, e.g. github.com/712345678 (CI provides it)
  --only REF                   distribute only to the target REF, as [PROVIDER:]PATH; repeatable or comma-separated (the sweep is off)
  --recreate TARGET@HEAD       local only: rebuild the paused sync branch of TARGET@HEAD while its head is HEAD (repeatable)
  --report FILE                also write the JSON report to FILE
  --stream FILE                write one JSON line per finished target to FILE as the run goes (default in CI: touchmark-report.jsonl)
  --strict                     exit 3 when a target is blocked or deferred or the sweep did not run
  --worktree                   refused: distribute ships only the packs committed at the hub's HEAD
```

<!-- end generated -->

### doctor

<!-- generated: help doctor -->

```text
Usage: touchmark doctor [--hub DIR] [--only REF]... [--hub-fp HOST/ID] [--strict] [--format text|json|markdown] [--report FILE] [--hub-token]

Check the write identity against every target, and with --hub-token where the hub keeps its write key; changes nothing.

Flags:
  --format FORMAT   output FORMAT: text, json or markdown (default text)
  --hub DIR         the hub checkout DIR (default $TOUCHMARK_HUB)
  --hub-fp HOST/ID  the hub's fingerprint HOST/ID, e.g. github.com/712345678 (CI provides it)
  --hub-token       local only: read where the hub keeps its secrets with a maintainer's token of the hub, from $TOUCHMARK_HUB_TOKEN; needs no write key
  --only REF        check only the target REF, as [PROVIDER:]PATH; repeatable or comma-separated
  --report FILE     also write the JSON report to FILE
  --strict          exit 3 when a check warns or is unknown, or a provider's targets could not all be listed
```

<!-- end generated -->

### probe

Runs in the jobs any branch of the hub can start: the GitLab template runs it in every
merge request pipeline, in the environment with `action: prepare`.

<!-- generated: help probe -->

```text
Usage: touchmark probe

Exit 2 when this job sees a write credential or signing key (the isolation probe).
```

<!-- end generated -->

### migrate

Prints the three files on stdout under `# ==> <name> <==` lines. See
[Migrate from multi-gitter](../guide/migrate.md).

<!-- generated: help migrate -->

```text
Usage: touchmark migrate --from-multi-gitter FILE [--id ID] [--writer LOGIN] [--bot LOGIN]... [--ca-file FILE]

Print hub.yml, targets.yml and operations.yml for a hub that replaces a multi-gitter setup on GitLab.

Flags:
  --bot LOGIN               the LOGIN that opened multi-gitter's merge requests; repeatable (default: found through the read credential)
  --ca-file FILE            a PEM FILE of CA certificates to trust, besides the system's, when checking accounts
  --from-multi-gitter FILE  the multi-gitter config FILE (YAML) to migrate from
  --id ID                   the new hub's ID, a slug like acme-eng (default: a placeholder that check rejects)
  --writer LOGIN            the writer service account's LOGIN
```

<!-- end generated -->

### setup

Reads `TOUCHMARK_HUB_TOKEN`. Exit 0 when everything is in place, 1 when a step failed, 2
when it wrote nothing because of the token, the hub or the flags, 3 when a step is left
to you. See [Set up a hub's platform](../guide/setup.md).

<!-- generated: help setup -->

```text
Usage: touchmark setup github|gitlab [--hub DIR] [--provider ID] [--url URL] [--project PATH] [--dry-run] [--format text|json]
         gitlab: --group PATH [--accounts auto|service-account|group-access-token] [--reader-name NAME] [--writer-name NAME] [--token-days N] [--schedules=false]
         github: [--reader-name NAME] [--writer-name NAME] [--listen ADDR] [--key-dir DIR] [--timeout DURATION]

Set up the hub's platform with a maintainer's token from $TOUCHMARK_HUB_TOKEN: the reader and the writer, the write key kept to the default branch, the hub's protection; changes only what differs.

Flags:
  --accounts KIND     gitlab: the KIND of the reader and the writer: auto, service-account or group-access-token (default auto)
  --dry-run           read the platform and print what setup would change; change nothing
  --format FORMAT     output FORMAT: text or json (default text)
  --group PATH        gitlab: the full PATH of the group whose projects are the targets
  --hub DIR           the hub checkout DIR (default $TOUCHMARK_HUB); setup reads its hub.yml and origin remote
  --key-dir DIR       github: the DIR for the new Apps' private keys (default: a new private temporary directory)
  --listen ADDR       github: the loopback ADDR of the page of the App manifest flow (default 127.0.0.1:0)
  --project PATH      the hub's PATH on the platform, owner/name (default: from the origin remote)
  --provider ID       the hub.yml provider ID to set up (default: the one of the platform on the hub's host)
  --reader-name NAME  the reader's NAME: a service account's username, a group access token's name, a GitHub App's name
  --schedules         gitlab: create the daily distribute and weekly doctor pipeline schedules (default true)
  --timeout DURATION  github: how long to wait for the browser step, a DURATION (default 30m)
  --token-days DAYS   gitlab: how many DAYS the tokens setup mints live (default 365)
  --url URL           the platform's URL, for a self-managed instance hub.yml does not list yet
  --writer-name NAME  the writer's NAME, as --reader-name
```

<!-- end generated -->

### manifest

<!-- generated: help manifest -->

```text
Usage: touchmark manifest [--hub DIR] [--format json]

Print the ownership manifest built from the hub's history.

Flags:
  --format FORMAT  output FORMAT: json, or text, which prints the same JSON (default json)
  --hub DIR        the hub checkout DIR (default $TOUCHMARK_HUB)
```

<!-- end generated -->

## In a target

### status

<!-- generated: help status -->

```text
Usage: touchmark status [--hub DIR] [--dir DIR] [--repo REF] [--packs a,b] [--worktree] [--format text|json]

List every managed path of the target and its state; changes nothing.

Flags:
  --dir DIR          the target working tree DIR (default: the current directory)
  --format FORMAT    output FORMAT: text or json (default text)
  --hub DIR          the hub checkout DIR (default $TOUCHMARK_HUB)
  --packs PACKS      comma-separated PACKS to apply instead of the selection from targets.yml and the opt-in file
  --repo OWNER/NAME  the target in targets.yml, as OWNER/NAME or PROVIDER:PATH (default: from the origin remote)
  --worktree         read configs and packs from the hub's working tree instead of its HEAD commit
```

<!-- end generated -->

### apply

<!-- generated: help apply -->

```text
Usage: touchmark apply [status flags] [--adopt GLOB]... [--dry-run]

Apply the packs to the target's working tree.

Flags:
  --adopt GLOB       overwrite local files matching GLOB with the pack version (repeatable)
  --dir DIR          the target working tree DIR (default: the current directory)
  --dry-run          print what apply would change without writing
  --format FORMAT    output FORMAT: text or json (default text)
  --hub DIR          the hub checkout DIR (default $TOUCHMARK_HUB)
  --packs PACKS      comma-separated PACKS to apply instead of the selection from targets.yml and the opt-in file
  --repo OWNER/NAME  the target in targets.yml, as OWNER/NAME or PROVIDER:PATH (default: from the origin remote)
  --worktree         read configs and packs from the hub's working tree instead of its HEAD commit
```

<!-- end generated -->

## Anywhere

### schema

`report` is the JSON report of `plan` and `distribute`; `doctor` and `setup` those of
their commands. Point an editor's YAML language server at a schema for completion:

```yaml
# yaml-language-server: $schema=https://raw.githubusercontent.com/bedrock-python/touchmark/master/schemas/hub.schema.json
```

<!-- generated: help schema -->

```text
Usage: touchmark schema hub|targets|opt-in|operations|report|doctor|setup

Print the JSON Schema of a configuration file or of a report.
```

<!-- end generated -->

### version

```text
touchmark v0.1.0 (go1.26.8 linux/amd64, commit 3f2a…, 2026-10-01T12:00:00Z)
```

<!-- generated: help version -->

```text
Usage: touchmark version

Print the version.
```

<!-- end generated -->
