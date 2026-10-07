# Use it on your machine

`status` and `apply` work on a checkout of a target, with a checkout of the hub next to
it. They need no account and no network: everything comes from the two working copies.
`check` and `manifest` work offline on the hub. `plan`, `doctor` and `distribute` work
from a laptop too, with credentials.

## See what a repository would get

```sh
cd ~/src/billing
touchmark status --hub ~/src/engineering-assets
```

```text
touchmark status · hub acme-eng @ ffae095 · target acme/billing
packs: agents (defaults)

  create  missing  .agents/project.md  agents
  create  missing  AGENTS.md           agents

missing 2
2 changes to apply: run touchmark apply
```

- `--hub DIR` is the hub checkout; set `TOUCHMARK_HUB` to leave it out.
- `--dir DIR` is the target checkout (default: the current directory).
- `--repo REF` names the target in `targets.yml`, as `owner/name` or `provider:path`. By
  default touchmark takes it from the target's `origin` remote.
- `--format json` prints every path with its state and action, for scripts.

`status` changes nothing and exits 0; read the summary line, or the JSON `summary`.

## Apply

```sh
touchmark apply --hub ~/src/engineering-assets --dry-run
touchmark apply --hub ~/src/engineering-assets
git diff && git add -A && git commit -m "chore: sync engineering assets"
```

`apply` writes the working tree only: it never commits, pushes or opens a pull request.
It writes each file atomically, never through a symlink, and stops cleanly on Ctrl-C.

## Take a file back under the hub

A file the repository changed is `local` and is left alone. To replace it with the pack's
current version and receive updates again:

```sh
touchmark apply --hub ~/src/engineering-assets --adopt AGENTS.md
touchmark apply --hub ~/src/engineering-assets --adopt '.agents/guidelines/**'
```

`--adopt` takes a glob and repeats. `ignore` in the opt-in file is stronger: an ignored
path is never adopted. In a repository the hub subscribes through an `org:` or `group:`
entry with `opt_in: assumed`, and that has no opt-in file, add `--assume-opt-in` (below).

## Organisations, groups and explicit packs

A local run reads `targets.yml`, but it cannot list an organisation or a group, so only
`defaults` and `repo:` entries count. When `targets.yml` has `org:` or `group:` entries
that might match, `status` warns, and `apply` refuses until you name the packs:

```sh
touchmark apply --hub ~/src/engineering-assets --packs agents,claude
```

The same goes for a hub that subscribes repositories (`opt_in: assumed`). In a checkout
without the opt-in file:

- a `repo:` entry with `assumed` that names the target opts it in: `status` prints
  `opted in by targets.yml (opt_in: assumed)`, and `apply` writes the packs of
  `targets.yml`;
- an `org:` or `group:` entry with `assumed` that might hold the target cannot be
  resolved here: the target counts as not opted in, and `status` warns that `plan` has
  the answer;
- without `--repo` or an `origin` remote, touchmark cannot tell, and warns when
  `targets.yml` subscribes anything.

`--assume-opt-in` settles the last two cases here: a target without the opt-in file
counts as opted in, as such an entry would make it. `status` prints `opted in by
--assume-opt-in`, and `apply` writes the packs of `targets.yml`:

```sh
touchmark apply --hub ~/src/engineering-assets --assume-opt-in --adopt AGENTS.md
```

An opt-in file in the checkout settles it either way, whatever the flag; with
`enabled: false` in it, `status` prints `opted out` and `apply` writes nothing. In
`--format json` the target's `opt_in` says which: `file`, `assumed`, `flag` (opted in
by `--assume-opt-in`), `opted-out` or `none`.

## Try a pack before committing it

```sh
touchmark status --hub ~/src/engineering-assets --worktree
```

`--worktree` reads packs and configuration from the hub's working tree instead of its
last commit (`check`, `plan`, `status`, `apply`).

## Plan from a laptop

`plan` needs the read key, the hub's fingerprint, and providers it can name: a
`providers` list or `platform` (and `base_url`) in `hub.yml`, or an origin on github.com,
a `*.ghe.com` host or gitlab.com.

```sh
export TOUCHMARK_READ_TOKEN=…            # or TOUCHMARK_READ_APP_ID and TOUCHMARK_READ_APP_KEY
touchmark plan --hub . --hub-fp github.com/712345678 --all
touchmark plan --hub . --hub-fp github.com/712345678 --only acme/billing --format json
```

The fingerprint is the hub's host and repository id: on GitHub
`gh api repos/OWNER/HUB --jq .id`, on GitLab the project id on the project's page. A
local run cannot read the tip of the hub's default branch, and says so in a warning.

`plan` needs git 2.45 or newer, as `distribute` does: both read every target with it.
`distribute` from a laptop also needs the write key, and is where the
[operation flags](operations.md#on-your-machine) work. Start with `--dry-run`.

## In Docker

The image runs the same commands; `--user` lets `apply` write files you own:

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD:/work" -v "$HOME/src/engineering-assets:/hub:ro" -w /work \
  ghcr.io/bedrock-python/touchmark:X.Y.Z status --hub /hub --repo acme/billing
```

## Windows

touchmark works on Windows checkouts with `core.autocrlf=true`. It judges a file you have
not changed by the blob git committed, as `plan` does in CI, so CRLF in the working tree
does not make a managed file `local`. A file the repository committed with CRLF line
endings is another matter: its blob differs from the pack's LF version, so it is `local`
on every machine, and `touchmark apply --adopt <path>` takes the pack's version back. See
[Identity is git's](../concepts/ownership.md#identity-is-gits).
