# Check the write account

`touchmark doctor` checks the writer against every target without writing anything: can
it still write there, will its token expire soon, will a target's rules block it. The
template runs it weekly from the default branch, with the write key, and on *Run
workflow*.

```sh
touchmark doctor --hub . --strict
```

## What it checks

Each check has a name, a status and a detail. The names you will see:

| Check | About | Warns or fails when |
|---|---|---|
| `writer` | the write identity | the write key belongs to another account than `writer` in `hub.yml` |
| `token-expiry` | the write identity | the token expires within 30 days (GitLab; Gitea and Forgejo tokens never expire) |
| `scopes`, `2fa` | the write identity (Gitea, Forgejo) | the token lacks a scope the writer needs, or has more than it needs |
| `signing-key` | the write identity | `TOUCHMARK_<ID>_SIGNING_KEY` is set but cannot sign for the writer |
| `hub-hidden` | the write identity | the writer can see the hub, so a leaked write key could change the packs |
| `markers` | the write identity | a pull request carries a marker with this hub's `id` but another fingerprint, or touchmark's marker from an unknown author |
| `access` | each target | the writer cannot write there; on GitHub, the App's installation has more permissions than touchmark asks for |
| `rules` | each target | a branch rule would block the sync branch or require what touchmark cannot do |
| `workflows` | each target (GitHub) | the App lacks the Workflows permission a target may need |
| `signing` | each target | the target requires signed commits and touchmark has no way to sign there |
| `write-isolation` | the hub | `security.write_isolation` is `none`: a warning with its reason |

A check touchmark cannot read is `unknown`, never `ok`. Targets it does not check say why
(`skipped`: not opted in, archived, deferred, …).

## Where the hub keeps its write key: `--hub-token`

With a maintainer's token of the hub in `TOUCHMARK_HUB_TOKEN`, `doctor --hub-token` also
reads the hub's settings, outside CI only:

```sh
read -rs TOUCHMARK_HUB_TOKEN && export TOUCHMARK_HUB_TOKEN
touchmark doctor --hub . --hub-fp gitlab.example.com/1234 --hub-token
```

- **GitHub:** the names of the repository's and the organisation's secrets, and the
  environment `touchmark-distribute`'s branch policy.
- **GitLab:** the hub's variables (protected, masked, environment scope), protected
  branches and tags, and the minimum role to use pipeline variables. The variables of the
  groups above the hub are visible to their Owners only; for a Maintainer those checks
  stay `unknown`.

These hub checks are named `environment`, `key-location`, `protected-branches`,
`protected-tags` and `pipeline-variables`; `touchmark setup` runs the same ones at its end.

The token goes only to the hub's own API host, which `doctor` prints before the first
request. It needs no write key: without one, `doctor` checks the hub only and leaves the
writer and target checks to the hub's CI. In CI, `--hub-token` exits 2.

## Reading the result

Every check is `ok`, `warn`, `fail` or `unknown`.

| Exit | Meaning |
|---|---|
| `0` | no check failed |
| `1` | a check failed, or a provider was unavailable |
| `2` | a configuration or guard error, or `--hub-token` in CI |
| `3` | with `--strict`: a check warns or is unknown, or `targets.yml` did not fully resolve |

In CI it leaves `touchmark-doctor.json` and `touchmark-doctor.md`, and on GitHub Actions
annotates the first ten failed and ten warning checks. See the
[doctor report](../reference/output.md#doctor).
