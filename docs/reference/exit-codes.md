# Exit codes

| Code | Meaning |
|---|---|
| `0` | success |
| `1` | an action failed, or an unexpected git or I/O error |
| `2` | a usage, configuration or guard error; nothing was written |
| `3` | `--strict` found a problem that is not a failure; `setup`: a step is left to you |

Per command:

| Command | `0` | `1` | `2` | `3` |
|---|---|---|---|---|
| `check` | no error (warnings allowed) | — | an error in a file, a pack or the history | — |
| `plan` | no target `failed`, including a run without the read key | a target `failed`, a provider unavailable, the sweep failed | configuration, an unknown fingerprint, a write key visible in CI, git older than 2.45 (unless offline) | with `--strict`: a target `blocked` or `deferred`, an explicit target the platform does not know, `targets.yml` not fully resolved, the sweep off, or a run without the read key |
| `distribute` | no target `failed`; a `superseded` run | a target `failed`, a provider unavailable, the sweep failed, the mass-close guard | a guard: the ref, the event, the probe, git older than 2.45, an operation flag in CI, the writer not matching `hub.yml`, `--worktree` | with `--strict`: a target `blocked` or `deferred`, `targets.yml` not fully resolved, the sweep off |
| `doctor` | no check failed | a check failed, a provider unavailable | configuration, a guard, `--hub-token` in CI | with `--strict`: a check `warn` or `unknown`, `targets.yml` not fully resolved |
| `probe` | no write credential or signing key in the job | — | a write credential or signing key is visible | — |
| `setup` | everything in place | a step failed | nothing written: the token, the hub or the flags | a step is left to you; run setup again once done |
| `migrate` | the files printed | an account it checked does not exist, or the check failed | the file is unreadable, not a GitLab multi-gitter config, or names something touchmark cannot express | — |
| `status`, `apply` | done, whatever the states | a file could not be written | configuration, a hub that fails `check`, `apply` without `--packs` when an `org` or `group` entry with `packs` might include the target | — |
| `manifest`, `schema`, `version` | done | an I/O error | usage, an unknown schema | — |

A usage error also prints the command's usage on stderr. Errors go to stderr; reports go
to stdout, so `--format json` output stays valid JSON, annotations included.

In CI, `1` and `2` fail the job; `3` fails it only where `--strict` is set. The template
sets `--strict` for `plan` on the hub's own pull requests, so a blocked or deferred target
is visible before the merge, and not for `distribute`, whose `deferred` targets the next
run picks up.
