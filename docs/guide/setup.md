# Set up a hub's platform

`touchmark setup github` and `touchmark setup gitlab` do the platform's part of creating
a hub: the reader and the writer, the write key kept to the default branch, and the hub's
protection. Run it once from a checkout of the hub, with a maintainer's token.

```sh
cd ~/src/engineering-assets          # setup reads hub.yml and the origin remote
read -rs TOUCHMARK_HUB_TOKEN && export TOUCHMARK_HUB_TOKEN   # never on the command line
touchmark setup gitlab --hub . --group acme/services --dry-run
touchmark setup gitlab --hub . --group acme/services
touchmark setup github --hub .
```

It sets up only layouts that keep the write key isolated (`security.write_isolation:
platform` or `external`), reads before it writes, and changes only what differs: a second
run writes nothing, and `--dry-run` only reads. It never prints a token or a key. It ends
with the checks of `doctor --hub-token`.

## GitLab

Everything goes through the API. The token needs the `api` scope and must belong to a
Maintainer of the hub and an Owner of the group whose projects are the targets (or to an
administrator). The hub must live outside that group, unless `hub.yml` says
`security.writer_on_hub: guard`: then setup accepts a hub inside it, checks that the writer
is below Maintainer on the hub (`writer-guard`), and also turns on *Pipelines must succeed*,
turns off *Skipped pipelines are considered successful* (`merge-checks`), and sets the *CI/CD
configuration file* to `.gitlab-ci.yml@<hub path>:<default branch>` (`ci-config`), so that
merge request pipelines run the default branch's CI file. The final check also reads the
variables of the groups above the hub, which GitLab shows to their Owners only: for a
Maintainer that check stays `unknown`.

setup:

- **makes the reader and the writer**: service accounts where GitLab lets the token
  create them (GitLab CE has them from 19.x; a self-managed instance lets group Owners
  create them only when an administrator allows it), group access tokens of the group
  otherwise. `--accounts` chooses, `--reader-name` and `--writer-name` name them. The
  reader is a Reporter of the group, the writer a Developer, and the writer must not
  reach the hub (under `writer_on_hub: guard`, not as a Maintainer);
- **mints their tokens straight into the hub's variables**: `TOUCHMARK_READ_TOKEN` masked
  and not protected, since merge request pipelines read it, and `TOUCHMARK_WRITE_TOKEN`
  protected, masked and hidden, with the environment scope `touchmark-distribute`. A
  write key that it finds unprotected, unmasked or in another scope is rotated, which
  revokes the old token. Tokens live `--token-days` days (365). To renew one, delete its
  variable and run setup again;
- **locks the hub down**: creates the environment `touchmark-distribute`, protects the
  default branch so that no one pushes to it (Maintainers merge, no force push), sets
  *Minimum role to use pipeline variables* to *No one allowed*, keeps protected variables
  out of merge request pipelines;
- **schedules the runs**: the daily `touchmark distribute` and weekly `touchmark doctor`
  pipeline schedules (`--schedules=false` skips them).

## GitHub

The token must administer the hub: a classic token with `repo`, or a fine-grained one
with Administration, Environments and Variables write and Secrets read.

setup:

- **limits the environment** `touchmark-distribute` to the default branch. It keeps the
  environment's reviewers and wait timer, and deletes any other branch policy;
- **creates the reader and writer Apps** from a manifest: the reader reads metadata,
  contents and pull requests; the writer reads metadata and writes contents, pull
  requests and workflows. They are named `<owner>-assets-read` and `<owner>-assets-write`
  unless `--reader-name` and `--writer-name` say otherwise. This step needs a browser:
  setup prints a link to a page it serves on `127.0.0.1` (`--listen`), and you confirm
  each App on GitHub within `--timeout` (30 minutes);
- **stores the Apps' ids** in the Actions variables `TOUCHMARK_READ_APP_ID` (the
  repository's) and `TOUCHMARK_WRITE_APP_ID` (the environment's);
- **writes each App's private key** to a file in a new private directory (`--key-dir`)
  and prints the `gh secret set` command that stores it. GitHub accepts a secret only
  encrypted as a libsodium sealed box, which touchmark does not implement. Run the
  commands, delete the directory, and run setup again: it checks the secrets by name;
- **adds the ruleset** `touchmark hub` to the default branch: pull requests with an
  approval and a code owner's review, no force push, no deletion. Where the plan or the
  token does not allow it, setup says what to set by hand.

Then install both Apps on the target repositories only, never on the hub, and set
`writer` in `hub.yml` to the writer App's bot. setup prints both links and the name.

## Variants

- **`security.write_isolation: external`**: setup does all of this except storing the
  write key, which belongs in your secrets store: it leaves the writer's token for you to
  mint on GitLab, or the writer App's key in its file on GitHub, and prints the OIDC
  claims to bind it to.
- **Several providers** in `hub.yml`: the variables are `TOUCHMARK_<ID>_…`. `--provider
  ID` picks the one to set up, and `--url` names a self-managed instance that `hub.yml`
  does not list yet. `--project` names the hub when the origin remote does not.
- **Gitea and Forgejo** have no `setup`: their Actions give every branch the secrets, so
  nothing can keep the write key to the default branch. Run the hub's CI on GitHub or
  GitLab and deliver to them from there, or follow
  [A hub on Gitea or Forgejo](../getting-started/gitea-forgejo.md).

## Reading the result

Each step ends with a status: `ok` (as wanted, nothing written), `done` (changed in this
run), `would` (what `--dry-run` would change), `warn` (as wanted, with a caveat),
`manual` (left to you), `unknown` (could not be read) or `fail`. `--format json` prints
the [setup report](../reference/output.md#setup).

| Exit | Meaning |
|---|---|
| `0` | everything is in place |
| `1` | a step failed |
| `2` | nothing written: the token, the hub or the flags are wrong |
| `3` | a step is left to you: do it, then run setup again |

Every flag: [`touchmark setup --help`](../reference/commands.md#setup).
