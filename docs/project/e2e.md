# e2e tests: Gitea, Forgejo, GitLab and GitHub

`internal/e2e` tests touchmark against real Gitea and Forgejo servers in Docker. The build tag `e2e` keeps them out of the ordinary `go test ./...`; `scripts/e2e/gitea.sh` starts a server, seeds it and runs them. `internal/e2e/gitlab` does the same for GitLab with `scripts/e2e/gitlab.sh`: see [GitLab](#gitlab) below. `internal/e2e/github` runs the GitHub driver against an HTTP fake of GitHub, in the ordinary `go test ./...`, and lists what only a live GitHub can confirm: see [GitHub](#github). With `--template`, both scripts also run the hub template's own CI files, and a dry run checks its GitHub workflow: see [The hub template](#the-hub-template).

## Run

You need Docker and bash (Git Bash on Windows). Nothing else: the tests run in `golang:1.27`.

```sh
bash scripts/e2e/gitea.sh docker.gitea.com/gitea:28.1.0
bash scripts/e2e/gitea.sh codeberg.org/forgejo/forgejo:16.0.5
bash scripts/e2e/gitea.sh all                          # Gitea 1.26.4, 1.27.3, 28.1.0, Forgejo 15.0.9 (LTS), 16.0.5
bash scripts/e2e/gitea.sh --run TestFacts all          # one test, every image
bash scripts/e2e/gitea.sh --keep docker.gitea.com/gitea:1.26.4
bash scripts/e2e/gitea.sh --require-signin --run TestVisibility all
```

The flavor comes from the image name. One image takes 8 to 10 minutes, the build with the race detector included; `all` takes about 35 minutes (Docker Desktop on Windows, 2026-09-27). The first run also pulls the images.

`--run` takes a `go test -run` expression. A `/` in it separates the levels of subtests, so an alternation of top-level tests (`TestFacts|TestDistribute`) cannot also name a subtest.

`--keep` leaves the server and its network in place, writes an env file with the tokens, publishes the server on a loopback port and prints how to rerun the tests against it and how to remove everything. Without `--keep`, the script removes all it created on exit, also after a failure or Ctrl-C.

`--require-signin` starts the server with `[service] REQUIRE_SIGNIN_VIEW = true`, as many company instances run. `TestVisibility` then expects the driver to report no repository as public. The other tests assume the default and are not meant for this mode.

## What the script does

1. Creates the network `touchmark-e2e-<rand>` and starts the server as `touchmark-e2e-<rand>-forge`: SQLite on a tmpfs, `INSTALL_LOCK`, `ROOT_URL` `http://localhost:3000/`.
2. Waits for `/api/healthz`, at most 180 seconds.
3. Seeds four accounts through the server's command line, each with an access token:

   | Account | Kind | Token scopes |
   |---|---|---|
   | `touchmark-admin` | site admin, for fixtures only | `all` |
   | `touchmark-reader` | bot (a user on Forgejo) | `read:repository,read:issue,read:organization,read:user` |
   | `touchmark-writer` | bot (a user on Forgejo) | `write:repository,write:issue,read:organization,read:user` |
   | `jdoe` | person | `write:repository,write:issue,read:user` |

4. Creates the private organisation `acme`: the team `readers` gives the reader read access to code, issues and pull requests, the team `writers` gives the writer write access, and `jdoe` is an owner. The script checks all of this through the API, and that the reader and the writer cannot create repositories.
5. Runs `go test -tags e2e -count=1 -race -v ./internal/e2e ./internal/e2e/gitlab` in `golang:1.27`, in the server container's network namespace (`internal/e2e/github` needs no server: the unit suite runs it).

touchmark sends a credential over plain http only to loopback ([Tokens and git](threat-model.md#tokens-and-git)). So the tests reach the server at `http://localhost:3000`, and that is also its `ROOT_URL`, so clone URLs in API answers work.

Tokens never appear on a command line. curl reads its `Authorization` header from standard input. The test container gets the tokens through the environment of the `docker` command, which names them without a value (`-e TOUCHMARK_E2E_ADMIN_TOKEN`). Only `--keep` writes them to a file: created with umask 077, and under Git Bash or Cygwin, where the file mode does not reach NTFS, cut down with `icacls` to the current user alone. The tests remove the token variables from their environment at start.

The script also sets `TOUCHMARK_E2E_REQUIRED=1`: the tests then fail instead of skipping when the forge's variables do not reach them.

## What the tests cover

- **`TestConformance`**: the platform contract suite `internal/platform/conformance` against `gitea.NewReader` and `gitea.NewWriter`.
  - Every subtest gets a fresh private organisation, set up by the admin:
    - repositories with files through the contents API;
    - executables, symlinks and submodules through a git push;
    - topics, archived repositories, forks;
    - pull requests by the writer and by `jdoe`, also from `jdoe`'s forks;
    - merges, closes and reopens.
  - Nested namespaces do not exist on these platforms, so those subtests skip.
  - Gitea moves an open pull request onto the default branch when its base branch is deleted (`RETARGET_CHILDREN_ON_MERGE`, on by default). An open pull request on a deleted base never arises there, so `PRs/base-deleted` skips on Gitea and reports it. `PRs/base-deleted-closed` covers the closed case, and runs on every platform.
  - `Renamed`: the old path of a renamed repository and the old login of a renamed user lead to them (the forges redirect with 301 and 307; the driver follows a redirect of a GET below the API base on the same host).
  - `EditPR/refused`: an edit the forge refuses (reopening a merged pull request, a missing base) writes no title, body or label.
- **`TestVisibility`**: the driver reports as `public` only what someone not signed in sees: a repository of a public organisation on a default server; with `--require-signin`, none.
- **`TestSHA256Repo`**: the driver reports a sha256 repository as such, by `Repo` and by `Resolve`, and reads its files with 64-digit blob ids.
- **`TestAGitPR`**: `jdoe` pushes a commit to `refs/for/main` with the topic `touchmark/agit` (AGit). The driver's `PRs` never lists that pull request as one from a branch, so a push for review cannot pass for someone else's pull request on a sync branch.
- **`TestAdminTokenRefused`**: the driver refuses a site admin's token. The token itself works (`Probe` reads the instance with it); `Self` and the writer's `Target` refuse it as `ClassInvalid`, naming `Sudo`.
- **`TestDistribute`**: `plan` and `distribute` through `cli.Main`, from a hub in a temporary directory, against seven new repositories in `acme`.
  - Targets come from an org entry with a topic and from an explicit repo entry. One repository with the topic is not opted in; one is opted in but has the sha256 object format, and is skipped; one is opted in but never selected.
  - Checked through the API:
    - the pull requests, their title, label, body and marker;
    - the commit authored by the writer with touchmark's trailers;
    - the files on the sync branch;
    - a second run writes nothing, and `plan` decides like `distribute`;
    - a person renames a pull request and takes its label off, and a new engine version runs: no run writes because of it, and a later content update keeps the title and labels (people own them, and the engine version is not compared);
    - a person's close is a decline, with an ack in the marker and one comment;
    - a close by the writer's own account is a self-close when the driver reads the closer from the timeline, a decline otherwise;
    - the driver's `PRs` for the sync branch matches the platform's own list: a declined pull request, two on one head, a fork's beside ours, with their closers;
    - a pack change updates the open pull request in place;
    - the hub changes its `id` with the old branch in `branch_aliases`: the open pull requests stay on the old branch, their bodies name the new id (one edit each), and the decline holds;
    - `jdoe` opens a pull request of their own from a target's sync branch: `blocked:branch-in-use`, no write;
    - `jdoe` merges a feature branch into another target's sync branch: `blocked:edited`, one edit that adds the paused block and the `recreate` control, no push;
    - a comment added to the opt-in file keeps a decline; an `ignore` of one of its paths lifts it, and a new pull request brings the rest on the renamed hub's branch;
    - the hub's files reach the default branch of a target with `autodetect_manual_merge` on: `closed:no-diff`, the closed marker and state in one edit, a comment, the branch deleted with a lease, and no merge recorded;
    - a target leaves `targets.yml`: the sweep closes its pull request as `closed:target-dropped`, with the closed marker and a comment, and leaves its branch (it carries `jdoe`'s merge);
    - `jdoe` fast-forwards a target's default branch to its sync branch: the forge records a manual merge, and touchmark writes nothing;
    - a pull request from `jdoe`'s fork on the sync branch's name, with a copy of the marker, is never touched;
    - no token appears in any output, report, pull request, comment or commit message.
  - Each step also checks which writes each target got, by the report's journal.
  - Before it checks that a second run writes nothing, the test waits for the server's own work after a push. The server writes the push event into the pull request's timeline from a queue, seconds after the push, and that changes the pull request's `updated_at`. For a person's pull request the comparison leaves `updated_at` out: touchmark never writes to one (its state, head, text, labels and comments tell), and the person's own push may land in its timeline late.
- **`TestDoctor`**: `doctor` through `cli.Main` with the writer's token, over a fresh organisation: a writable opted-in repository (access ok; rules unknown while no sync branch exists, since the writer reads the protection of existing branches only), a read-only one (access fails), one not opted in (skipped), one whose sync branch a rule `touchmark/*` without pushes protects (rules fail), and one whose pull requests on the sync branch carry another hub's marker with the same id and a person's copy of this hub's (markers warn). The hub is a repository the writer reads (`hub-hidden` warns), then writes (fails). The token's scopes come from `GET /api/v1/token` where the forge has it; `--hub-token` with the admin's token reads the hub's Actions secrets, and a write key there fails. No token reaches any output.
- **`TestProtectedBranch`**: branch protection `touchmark/*` without pushes, which the writer reads for existing branches only. On a target whose pull request is open, a pack change is blocked `blocked:rules:protected-branch` before any write, in a dry run and in `distribute`, with a warning naming the branch (the forge does not show the writer the rule's name), the pull request as it was. On a target protected before its sync branch exists, the first push meets the protection and is refused with the same reason, nothing written.
- **`TestFacts`**: forge behavior touchmark relies on that the documentation leaves open, checked through the API and git alone, without the driver:
  - flavor detection;
  - the head filter of the pull request list (and whether it matches forks), `poster`, `pulls/{base}/{head}`;
  - the closer in the timeline;
  - draft title prefixes;
  - labels by id and by name;
  - 409 on a duplicate pull request;
  - body sizes;
  - sha256 repositories;
  - what deleting a pull request's head or base does;
  - `autodetect_manual_merge`;
  - the issue search the stale-PR sweep uses;
  - git over HTTP with Basic credentials: which user name, which token;
  - what the writer sees of a rule that requires signed commits, and how an unsigned push is refused;
  - rate-limit headers;
  - `sudo` with an admin token;
  - `GET /api/v1/token`;
  - the pagination headers of each listing the driver reads: whether it sends `Link`, and whether `X-Total-Count` counts the listing or the page. The driver ends a listing by `X-Total-Count` only where it counts the listing, and its unit tests' fixtures copy these headers.

Each test prints what it learned as `FINDING` lines. The run ends with a summary of them; a run that `go test -timeout` is about to end prints the summary a minute before.

## CI

The job `e2e-forge` in `.github/workflows/ci.yml` runs the script once per image: the latest Gitea and Forgejo (28.1.0 and 16.0.5) on every pull request and push, all five images nightly and on `workflow_dispatch`. The lint job of every CI run vets the module with the tag `e2e` (`go vet -tags e2e ./...`) for Linux, macOS and Windows, so the tests always compile. `e2e-forge` itself is outside the required check `All checks passed`: it pulls the forges' images from their own registries, whose outage should not block a merge; read its result before merging.

## Without the script

The tests read the server from the environment. Without `TOUCHMARK_E2E_URL`, every test skips.

| Variable | Value |
|---|---|
| `TOUCHMARK_E2E_URL` | the server's root, `http://localhost:3000` |
| `TOUCHMARK_E2E_FLAVOR` | `gitea` or `forgejo` |
| `TOUCHMARK_E2E_IMAGE` | the image, for messages |
| `TOUCHMARK_E2E_ORG` | the seeded organisation, `acme` by default |
| `TOUCHMARK_E2E_REQUIRE_SIGNIN` | `1` when the server runs with `REQUIRE_SIGNIN_VIEW` |
| `TOUCHMARK_E2E_TEMPLATE`, `TOUCHMARK_E2E_HUB_ORG`, `TOUCHMARK_E2E_TOUCHMARK_IMAGE` | `--template`: the template's working tree, the organisation of hubs, the image under test; unset, `TestTemplate` skips |
| `TOUCHMARK_E2E_REQUIRED` | when set, a missing `TOUCHMARK_E2E_URL` fails the run |
| `TOUCHMARK_E2E_{ADMIN,READER,WRITER,PERSON}_LOGIN` | the accounts |
| `TOUCHMARK_E2E_{ADMIN,READER,WRITER,PERSON}_TOKEN` | their tokens |

## GitLab

`internal/e2e/gitlab` tests the gitlab driver and the commands that use it against GitLab CE in Docker, with a GitLab Runner for the CI experiments. The build tag `e2e` keeps the package out of the ordinary `go test ./...`. Its variables start with `TOUCHMARK_E2E_GITLAB_`, so `scripts/e2e/gitea.sh`, which runs `./internal/e2e ./internal/e2e/gitlab`, finds none and skips the package.

### Run

You need Docker with about 6 GB of memory for it and bash (Git Bash on Windows). GitLab alone takes about 4 GB: the script runs one GitLab container at a time and refuses to start while another GitLab container runs. Do not run the unit suite in Docker at the same time.

```sh
bash scripts/e2e/gitlab.sh gitlab/gitlab-ce:18.11.12-ce.0
bash scripts/e2e/gitlab.sh all                                  # CE 17.11.7, 18.11.12, 19.4.1, one after the other
bash scripts/e2e/gitlab.sh --run 'TestFacts|TestCI' gitlab/gitlab-ce:19.4.1-ce.0
bash scripts/e2e/gitlab.sh --only-seed gitlab/gitlab-ce:17.11.7-ce.0   # boot, seed, check, remove
bash scripts/e2e/gitlab.sh --only-seed --keep gitlab/gitlab-ce:18.11.12-ce.0
```

A cold GitLab boots in 3 to 6 minutes on a Linux host with 4 CPUs (expected; not yet measured on a hosted runner). Docker Desktop on Windows took 10 to 12 minutes for 17.11.7 and 19.4.1 on 2026-09-27, and 16 for 18.11.12 with other containers busy: raise the 15-minute wait with `TOUCHMARK_E2E_READY_TIMEOUT` (seconds) on a slow machine. The runner image matches the GitLab minor (`gitlab/gitlab-runner:alpine-v17.11.4`, `alpine-v18.11.4`, `alpine-v19.4.1`): the Alpine images have git 2.47 to 2.54, and the jobs that run `plan` need 2.45, which the Ubuntu images (git 2.43) lack. The script pulls the image when it is missing.

`--keep` leaves GitLab, the runner, the network, the volume with the binary and the env file with the tokens in place, publishes GitLab on a loopback port and prints how to rerun the tests and how to remove everything. `--only-seed` stops after the seeding and its checks; with `--keep` it gives a seeded instance to work against. Without `--keep`, the script removes all it created on exit, also after a failure or Ctrl-C: the containers with their anonymous volumes (the GitLab image declares `/etc/gitlab`, `/var/log/gitlab` and `/var/opt/gitlab`, about 0.5 GB per run; the runner image `/etc/gitlab-runner` and `/home/gitlab-runner`), the network, the volume with the binary and the env file. `gitea.sh` removes the forge's `/data` volume the same way.

### What the script does

1. Creates the network `touchmark-e2e-<rand>` and starts GitLab as `touchmark-e2e-gitlab`: `external_url http://localhost`, Puma in single mode, 10 Sidekiq threads, no Prometheus and exporters, registry, Pages or KAS, `--shm-size 256m`, `--memory 4608m`. `GITLAB_OMNIBUS_CONFIG` holds no secret: it stays in the container's configuration (`docker inspect`) for the container's life. Omnibus generates root's password, which nothing uses (root's token comes from a `gitlab-rails runner` script), and the script deletes `/etc/gitlab/initial_root_password` once GitLab is ready.
2. Builds a linux `touchmark` from the working tree in `golang:1.27` into the volume `touchmark-e2e-<rand>-bin` while GitLab boots.
3. Waits for `/-/readiness?all=1` and for the API (at most 15 minutes, printing progress and memory every 30 seconds).
4. Seeds, and checks through the API:

   | Account | How | Role in `acme` and `acme/sub` | Token scopes |
   |---|---|---|---|
   | `root` | the administrator; its token comes from one `gitlab-rails runner` script; for fixtures only | Owner | `api, read_user, read_repository, write_repository` |
   | reader | an instance service account with a personal access token, or the bot of a group access token of `acme` | Reporter | `read_api, read_repository` |
   | writer | the same, another account | Developer | `api, write_repository` |
   | `jdoe` | a person, with a personal access token | Owner | `api, read_user, read_repository, write_repository` |

   Which path an image takes, checked on 2026-09-27: CE 18.11.12 answers 404 to `GET /service_accounts` (there the API is EE code, `ee/lib/api/service_accounts.rb`, which the `gitlab-ce` image lacks), so its reader and writer are group access token bots (`group_<id>_bot_<hex>`); CE 19.4.1 has instance service accounts (`touchmark-reader`, `touchmark-writer`). The script logs the path, and the tests get it in `TOUCHMARK_E2E_GITLAB_ACCOUNTS`. The checks: every token acts as its account, only root is an administrator, the scopes by `GET /personal_access_tokens/self`, the roles by the reader's `GET /groups/:id/members/all/:user_id` in `acme` and `acme/sub`, and the reader cannot create a project.
5. Creates an instance runner with `POST /user/runners` (a `glrt-` token), starts `gitlab/gitlab-runner` (Alpine) of the same minor in GitLab's network namespace with the binary at `/opt/touchmark/touchmark`, registers it with the shell executor (the token reaches `gitlab-runner register` through the environment) and waits until GitLab reports it `online`.
6. Runs `go test -tags e2e -count=1 -race -v ./internal/e2e/gitlab/...` in `golang:1.27`, in GitLab's network namespace.

touchmark sends a credential over plain http only to loopback ([Tokens and git](threat-model.md#tokens-and-git)). So GitLab's `external_url` is `http://localhost`, and the tests and the runner share its network namespace: clone URLs in API answers and `CI_SERVER_URL` in jobs are the URL touchmark uses. GitLab's nginx listens on IPv4 only; Go, git and curl fall back from `::1` to `127.0.0.1`, busybox `wget` does not.

Secrets never appear on a command line: curl reads its `PRIVATE-TOKEN` header from standard input, the tokens come back from GitLab on standard output, and the test container reads them from a temporary env file. The env files are created with umask 077 and, under Git Bash or Cygwin, cut down with `icacls` to the current user alone. The tests remove the token variables from their environment at start. `TOUCHMARK_E2E_GITLAB_REQUIRED=1` makes the tests fail instead of skipping when the variables do not reach them.

### What the tests cover

- **`TestConformance`**: the platform contract suite `internal/platform/conformance` against `gitlab.NewReader` and `gitlab.NewWriter`. Every subtest gets a fresh subgroup `acme/conf-<rand>`, which inherits the roles of `acme`; nested namespaces are subgroups of it, forks come from a sibling subgroup that the reader can read (GitLab shows `forked_from_project` only to those who can read the source), and merge requests from a fork are the person's, from a fork in the person's namespace. GitLab cannot lower an inherited role on one project, so a read-only project lives in a separate top-level group: the reader and the writer are Reporters there when they are service accounts, and do not see it when they are group access token bots (whose bot can join its own group only). GitLab reserves some project paths (`files`, `create`, `edit`, ...: the routes below a project), so the fixture appends `-project` to those names of the suite. The members of a group get access to a new project in it from a Sidekiq job, seconds later on a busy instance: the fixtures wait until the accounts see each new project. `Renamed`: a renamed project's old path leads to it; GitLab finds users by their current username only, so the renamed-account half skips with a finding when an old login finds nobody. `ReadFiles`: the GraphQL batch read of one path across projects (a regular file, an executable, binary content, a file over the limit, a symlink, a submodule, a directory, a missing file, an empty project) agrees with `ReadFile`, and a path through a symlinked directory is never a file.
- **`TestAdminTokenRefused`**: the driver refuses root's token in `Self` and `Target` (it could act as anyone through Sudo); `Probe` works with it.
- **`TestDistribute`**: `plan` and `distribute` through `cli.Main` against projects of `acme` and `acme/sub` selected by a random topic (with `subgroups: true`) and by a repo entry:
  - merge requests by the writer with the title, label, body, marker and a commit with touchmark's trailers, and no body line that GitLab could run as a quick action;
  - a sha256 target skipped, when the instance makes sha256 projects;
  - a second run that writes nothing, after every step;
  - a person's title and a new engine version write nothing;
  - a person's close is a decline;
  - a pack change force-pushes the sync branch (rebuilt on `main`), and the merge request stays open;
  - a deleted source branch closes the merge request: a decline for touchmark, with no push afterwards;
  - the person deletes a sync branch right after touchmark's force push (subtest `branch-deleted-after-push`): with the person or nobody (`closed_by` null) as the closer a decline; with the writer, which GitLab names while it still processes the push, the subtest checks the known behavior (the close is taken for touchmark's own and the content proposed again) and skips with a finding that names it;
  - a merge request from the person's fork on the sync branch's name, with a copy of the marker, is never touched;
  - no token in any output, merge request, note or commit message.
- **`TestMigration`**: the move from a multi-gitter setup ([Migrate from multi-gitter](../guide/migrate.md)), built synthetically. A group access token of `acme/mig-<rand>` plays multi-gitter's account: its bot opens merge requests without a marker on `chore/sync-engineering-assets` with the hub's files; a person closes one of them. `migrate --from-multi-gitter` reads a multi-gitter configuration with the reader's token and prints `hub.yml`, `targets.yml` and `operations.yml`, which the hub takes as they are (plus its packs): the alias, the bot as a checked known author, `adopt_unmarked`. distribute adopts the open merge request (the same one, with the marker and touchmark's commit), opens a new one on `touchmark/<id>` next to the closed one (a closed merge request without a marker is no decline), writes nothing in a second run, and takes a person's close of the adopted merge request for a decline.
  That group access token is revoked before `migrate` runs: the bot of a revoked token must still be found and its merge request adopted.
- **`TestDoctor`**: `doctor` through `cli.Main` with the writer's token over a fresh subgroup: the token's expiry and scopes (`/personal_access_tokens/self`), a bot or service account (2fa ok); a Developer's access; protected branches over the sync branch, for Maintainers (rules fail) and for Developers without force pushes (warn); another hub's marker with the same id and a person's copy of this hub's (markers warn); a hub the writer does not see (`hub-hidden` ok), then one in its subgroup (fail). `--hub-token` with root's token reads the hub's variables and protected tags: a protected write key scoped to `touchmark-distribute` is ok, an unprotected one fails, a protected tag Developers may create fails.
- **`TestSetup`**: `touchmark setup gitlab` through `cli.Main` as a maintainer runs it, with the person's token: an Owner of the seeded group, whose fresh subgroup holds the targets (one project opted in), and an Owner of a separate top-level group where the hub lives (GitLab shows a group's variables, which the final check reads, to its Owners only; a Maintainer gets that check as `unknown` and exit code 3). The hub starts as GitLab makes projects (the default branch protected for Maintainers) and keeps a stale write key, a personal access token of the person that root mints, in an unprotected variable of every environment. The first run must end with no failed, manual or unknown step, and root then reads: the reader and the writer (service accounts where the instance lets the person create them, else group access tokens; the `FINDING` line says which) with Reporter and Developer in the group and no access to the hub; `TOUCHMARK_READ_TOKEN` masked and not protected with scope `*`; `TOUCHMARK_WRITE_TOKEN` protected, masked and hidden with scope `touchmark-distribute`, and the stale copy gone, its token revoked (`GET /personal_access_tokens/self` with it answers 401); the environment; `main` with push No one, merge Maintainers and no force push; no pipeline variables; `protect_merge_request_pipelines` false; the two schedules. A second run and a dry run have only `ok` steps and rotate no token (the active tokens are the same before and after). `doctor --hub-token` with the person's token grades the hub with no `fail` or `warn`. With the runner, a merge request (merged by the person: push to `main` is No one now) names the writer in `hub.yml` and adds a pipeline whose jobs on `main` must succeed: one without an environment runs `probe` (the write key is not visible there) and `plan` (the reader's token reads the group), one with `environment: {name: touchmark-distribute, action: prepare}` runs `doctor` with the writer's token. No token appears in any output or job log.
- **`TestProtectedBranch`**: wildcard protected branches, which the writer, a Developer, reads before it pushes. One target protects `touchmark/*` for Maintainers alone: a dry run and the first run block it `blocked:rules:protected-branch` before any write, with a warning naming the rule. Another lets Developers push but not force-push: the first run opens a merge request, and after a pack change the rebuilt branch needs a force push, which is refused, so the target is `blocked:rules:protected-branch` and its merge request stays as it was. A third has two rules on the sync branch, `touchmark/*` for Maintainers alone and the branch's own name for Developers with force pushes: GitLab applies the most permissive rule that matches a branch, so it is delivered and updated as if unprotected.
- **`TestFacts`**: GitLab behavior touchmark relies on that the documentation leaves open, through the API and git alone: 409 on a duplicate merge request; draft title prefixes and a `Draft:` commit; labels created by name and `add_labels`; quick actions in a person's description on create and update, against touchmark-shaped text; `closed_by` for a person and for a bot, and `merged_by`; a deleted source branch closes the merge request, who GitLab names as the closer (`closed_by` and `resource_state_events`), and what a reopen without the branch does; a force push keeps it open; how soon after a git push the merge request API takes the new branch (`push-then-create`, see below); `repository_object_format` and sha256 projects; git over HTTP with Basic `oauth2:<token>` and other user names; git with a group access token that has the scope `api` alone; descriptions of 200 000 bytes and around 1 MiB; the reader's `members/all` of the writer; rate-limit headers of a default instance; the bot of a revoked group access token, watched for 90 s (the user, and the author of its merge request); which users API view carries the `bot` field; what a fork of a project the reader cannot read shows; `GET /personal_access_tokens/self`; `push_rule` on CE; the pagination headers.

  `push-then-create` is why a merge request is opened again after a pause: GitLab checks a new merge request's source branch against the project's cached branch names, which the push's background job refreshes. On CE 19.4.1 under load a `POST /merge_requests` right after a push got `400 {"source_branch":["does not exist"]}` in 8 of 9 tries over three runs, and the merge request opened 1 to 10 s later; `HEAD /repository/branches/:branch` (the cached view) lagged `GET` of the same (Gitaly) by as much. When the branches API finds the branch, the driver's `CreatePR` returns `platform.ErrNotYet` (transient, with a `RetryAfter`), and the core sends the POST again after growing pauses, for at most 30 s.
- **`TestCI`**, on the runner:
  - `probe`: a hub project with three write-token variables, `TOUCHMARK_X_WRITE_TOKEN` (protected, masked, environment scope `touchmark-distribute`), `TOUCHMARK_Y_WRITE_TOKEN` (the same, unprotected) and `TOUCHMARK_Z_WRITE_TOKEN` (unprotected, scope `*`), and a pipeline that runs `touchmark probe` in a job with `environment: {name: touchmark-distribute, action: prepare}` and in one without, on merge request pipelines and on the protected default branch. It checks that the protected key reaches the environment job of the default branch and no job of a merge request pipeline, that the probe in a merge request pipeline fails naming the unprotected scoped key ([Isolation of the write key](threat-model.md#isolation-of-the-write-key)), that the probe catches the unprotected unscoped key in both jobs of a merge request pipeline (the job without an environment fails naming it), and that no job log shows a value.
  - `job-token`: target X's pipeline, started by the writer's push to a branch as a sync branch's is, reaches target Y with `CI_JOB_TOKEN`: reads (the project, a file, the merge requests, `git ls-remote`) and writes (a `git push` of a new branch, `POST /repository/branches`). Four rounds: Y's job token settings as GitLab creates them (the test fails if X reaches anything of Y then); Y's allowlist turned off (`PATCH /job_token_scope enabled=false`, as on projects created before the allowlist became the default); the same with Y's `ci_push_repository_for_job_token_allowed` on; the allowlist on again with X in it (threat T5 of the [threat model](threat-model.md#threats)).

Each test prints what it learned as `FINDING` lines, and the run ends with a summary of them.

### What the runs found

All tests passed on CE 17.11.7, 18.11.12 and 19.4.1 (Docker Desktop on Windows; 17.11 and 18.11 on 2026-09-28, 19.4.1 on 2026-09-29). `TestConformance/Renamed` skips everywhere: GitLab finds users by their current username only.

| Fact | 17.11.7 | 18.11.12 | 19.4.1 |
|---|---|---|---|
| `GET /service_accounts` (reader and writer) | 404: group access tokens | 404: group access tokens | 200: instance service accounts |
| Kind the driver reports for them | bot | bot | service account |
| Duplicate open MR on the same source and target | 409 "Another open merge request already exists for this source branch: !N" | same | same |
| Draft prefixes | `Draft:`, `draft:`, `[Draft]`, `(Draft)`; not `WIP:` | same | same |
| A pushed commit "Draft: …" | makes the MR a draft, title prefixed | same | same |
| Quick action in a person's description | runs on create and update, line removed | same | same |
| `/close` in a code span, fenced block, quote, HTML comment | not run, body kept byte for byte | same | same |
| Description of 200 000 B / 1 MiB / 1 MiB + 1 B | stored / stored / 201, cut to 1 MiB | same | same |
| POST MR right after a git push | 0 of 3 refused | 0 of 3 refused | 400 "source_branch does not exist" in 8 of 9 (three runs); opened 1 to 10 s later |
| Deleted source branch closes the MR | yes, 1 to 12 s | yes | yes |
| `closed_by` after a person deleted the branch | the person; the writer when the writer's own push was still being processed | same | same |
| `closed_by` after a person deleted a sync branch right after touchmark's force push (TestDistribute) | null once, the person once | the person | null once, the person once |
| Reopen without the source branch | 200, opens | 200, opens | 422, stays closed |
| Force push, also a reset to the target | MR stays open | same | same |
| Bot of a revoked group access token | active, found by username, still the MR's author 90 s later | same | same |
| Group access token with `api` alone | git ls-remote and push work | same | same |
| Git over HTTP | Basic with any user name and the token; Bearer refused; the reader's token cannot push | same | same |
| Developer reads `protected_branches` | 200 | 200 | 200 |
| Wildcard protected `touchmark/*` | create refused: `blocked:rules:protected-branch`; force push refused: the same | same | same |
| `GET /push_rule` on CE | 404 | 404 | 404 |
| SSH signature by touchmark with a key root registered for the writer | verified | verified | verified (service account) |
| sha256 projects | behind the feature flag `support_sha256_repositories` | same | same |
| Probe job with `environment: {name: touchmark-distribute, action: prepare}` in an MR pipeline from an unprotected branch | sees the unprotected scoped variable and the unscoped one, not the protected one; the probe fails | same | same |
| Job without an environment | sees the unscoped variable only; the probe fails naming it | same | same |
| New project's job token scope | `inbound_enabled: true` | same | same |
| `CI_JOB_TOKEN` of target X's pipeline (run as the writer) on target Y, Y as created | API 404, git refused; push and API write refused | project 404, files and MRs 403, git refused; push and API write refused | same as 18.11 |
| Instance enforces the allowlist (`enforce_ci_inbound_job_token_scope_enabled`) on a new instance | no: `PATCH /job_token_scope enabled=false` is 204 | yes: the PATCH is 400 "enforced for the instance" until an admin turns the setting off | same as 18.11 |
| The same, Y's allowlist off (a legacy project) | API 404, git ls-remote works; push and API write refused, also with `ci_push_repository_for_job_token_allowed` | files and MRs 200, git ls-remote works; push and API write refused, also with the push setting | same as 18.11 |
| The same, Y's allowlist on with X in it | API 404, git works; writes refused | files and MRs 200, git works; writes refused | same as 18.11 |
| Rate-limit headers on a default instance | none | none | none |
| `touchmark setup gitlab` (TestSetup, 18.11.12 only, 2026-10-02) | not run | group access tokens (no service accounts API); variables created with `masked_and_hidden` come back `hidden: true`; `PUT /projects/:id` takes `ci_pipeline_variables_minimum_override_role`, `restrict_user_defined_variables` and `protect_merge_request_pipelines`; the default branch's levels change only by protecting it again (Free); a second run rotates nothing; the pipeline's reader and writer jobs succeed | not run |

Not verifiable here: push rules and "Reject unsigned commits" (EE with a license), gitlab.com limits, fine-grained PATs.

### CI

The job `e2e-gitlab` in `.github/workflows/e2e-gitlab.yml` runs the script once per image (CE 17.11.7, 18.11.12, 19.4.1): nightly, on `workflow_dispatch` and on pull requests labelled `e2e-gitlab`, not on every push. The workflow listens to the `labeled` event for that, and is a workflow of its own so that a labeled run never reports the required check `All checks passed` of `ci.yml`. A labeled run's concurrency group carries the label's name: adding another label never cancels a running `e2e-gitlab` (the new run would skip the job); a new push, or `e2e-gitlab` added again, does. The job's limit is 90 minutes: about 10 to free the disk, pull, build and seed, at most 15 for the boot (`TOUCHMARK_E2E_READY_TIMEOUT`), and 45 for `go test` (`TOUCHMARK_E2E_TEST_TIMEOUT`), which then ends itself with its goroutine dump and the FINDINGS summary before the runner would kill the job. Boot and test times on a hosted runner are not measured yet: record them from the first run. CI's lint job vets the package with the tag `e2e` on every run.

### Without the script

The tests read GitLab from the environment. Without `TOUCHMARK_E2E_GITLAB_URL`, every test skips.

| Variable | Value |
|---|---|
| `TOUCHMARK_E2E_GITLAB_URL` | GitLab's `external_url`, `http://localhost` |
| `TOUCHMARK_E2E_GITLAB_IMAGE` | the image, for messages |
| `TOUCHMARK_E2E_GITLAB_GROUP`, `TOUCHMARK_E2E_GITLAB_SUBGROUP` | the seeded groups, `acme` and `acme/sub` |
| `TOUCHMARK_E2E_GITLAB_ACCOUNTS` | `service-account` or `group-access-token`: what the reader and the writer are |
| `TOUCHMARK_E2E_GITLAB_RUNNER_BIN` | touchmark's path in the runner's jobs; unset, `TestCI` skips |
| `TOUCHMARK_E2E_GITLAB_TEMPLATE`, `TOUCHMARK_E2E_GITLAB_HUB_GROUP`, `TOUCHMARK_E2E_GITLAB_TOUCHMARK_IMAGE` | `--template`: the template's working tree, the group whose image runner runs the hub's jobs, the image under test; unset, `TestTemplate` skips |
| `TOUCHMARK_E2E_GITLAB_REQUIRED` | when set, a missing `TOUCHMARK_E2E_GITLAB_URL` fails the run |
| `TOUCHMARK_E2E_GITLAB_{ROOT,READER,WRITER,PERSON}_LOGIN` | the accounts |
| `TOUCHMARK_E2E_GITLAB_{ROOT,READER,WRITER,PERSON}_TOKEN` | their tokens |

## The hub template

The hub template ([engineering-assets-template](https://github.com/bedrock-python/engineering-assets-template), a repository of its own) ships CI files for GitHub, GitLab, Gitea, Bitbucket and Azure that run touchmark from its release image. Its jobs are run for real, from the template's working tree, every job in the touchmark image under test, as that image's user:

```sh
# the image built from this working tree's Dockerfile
bash scripts/e2e/gitlab.sh --template ../engineering-assets-template --run TestTemplate gitlab/gitlab-ce:18.11.12-ce.0
bash scripts/e2e/gitea.sh --template ../engineering-assets-template --run TestTemplate docker.gitea.com/gitea:1.27.3
# a published image instead
bash scripts/e2e/gitlab.sh --template ../engineering-assets-template --touchmark-image ghcr.io/bedrock-python/touchmark@sha256:<digest> --run TestTemplate gitlab/gitlab-ce:18.11.12-ce.0
# the GitHub workflow: a dry run against the fake, Linux only (the Action needs a Linux runner)
docker run --rm --name touchmark-e2e-template-github -v "$PWD:/src:ro" -v "$PWD/../engineering-assets-template:/template:ro" -w /src \
  -e TOUCHMARK_E2E_TEMPLATE=/template golang:1.27 \
  sh -c 'git config --global --add safe.directory /src && go test -count=1 -run TestTemplateWorkflow ./internal/e2e/github/'
# the Bitbucket Pipelines file: its pipelines and steps, played without Bitbucket, on any OS
TOUCHMARK_E2E_TEMPLATE=../engineering-assets-template go test -count=1 -run TestTemplatePipelines ./internal/e2e/bitbucket/
# the Azure Pipelines files: their stages and steps, played without Azure DevOps, on any OS
TOUCHMARK_E2E_TEMPLATE=../engineering-assets-template go test -count=1 -run TestTemplateAzurePipelines ./internal/e2e/azure/
```

Without `--run`, `--template` adds `TestTemplate` to the whole suite. Without `--template`, `TestTemplate` skips, and so do `TestTemplateWorkflow`, `TestTemplatePipelines` and `TestTemplateAzurePipelines` without `TOUCHMARK_E2E_TEMPLATE` (`TestTemplatePipelines` also when the template has no `bitbucket-pipelines.yml`, `TestTemplateAzurePipelines` when it has no `azure-pipelines.yml`). `gitea.sh` runs Gitea's runner, so `--template` applies to the Gitea images; on a Forgejo image the script says so and `TestTemplate` skips.

### What `--template` adds (`scripts/e2e/template-lib.sh`)

1. The touchmark image: built from the working tree's `Dockerfile` (`BINARY=source`), or `--touchmark-image`, pulled. It is pushed to a registry (`registry:3.1.2`, by digest) that listens on `127.0.0.1:<random port>` in the Docker host's network namespace, as `localhost:<port>/touchmark:0.0.0-e2e@sha256:<digest>`: the `NAME:TAG@sha256:<digest>` form the template pins the release image in. The Docker daemon pulls from a registry on localhost without TLS.
2. A forwarder (`scripts/e2e/loopback.go`, built and run in `golang:1.27` with `--network host`) on `127.0.0.1` and `[::1]` of the Docker host's network namespace, at the forge's port (80, 3000), to the forge's address on the run's network. The runners start the job containers in that namespace (network mode `host`), so the jobs reach the forge at its external URL, `http://localhost` or `http://localhost:3000`, the only plain-http host touchmark sends a credential to. A job container cannot join the forge's network namespace instead: GitLab's Docker executor gives every container a hostname, and Docker refuses `--hostname` with `--network container:<name>` ("conflicting options: hostname and the network mode", checked on Docker 29.3). On Docker Desktop, that namespace is the Linux VM's, not the desktop's, and nothing is published on the desktop.
3. The runner:
   - GitLab: `touchmark-e2e-gitlab-image-runner`, the `gitlab/gitlab-runner` image of the GitLab minor with the Docker socket, registered as a group runner of the group `hubs` (the person is its Owner): Docker executor, network mode `host`, `disable_cache` (no volumes left behind), the image under test as its default image. The template's hub turns the instance runners off, so only this runner runs its jobs. The runner pulls its helper image from `registry.gitlab.com` on the first job.
   - Gitea: `touchmark-e2e-<rand>-actions`, Gitea's runner (`docker.gitea.com/runner:4.1.0`, by digest, formerly `act_runner`) with the Docker socket, registered for the instance with a token from `gitea actions generate-runner-token`: `container.network: host`, `docker_host: "-"` (jobs get no Docker socket), the cache server off. The organisation `hubs`, with the person as an owner, holds the hub.
4. The test container gets the template's working tree read-only at `/template` and `TOUCHMARK_E2E_GITLAB_TEMPLATE`, `…_HUB_GROUP`, `…_TOUCHMARK_IMAGE` (GitLab), or `TOUCHMARK_E2E_TEMPLATE`, `TOUCHMARK_E2E_HUB_ORG`, `TOUCHMARK_E2E_TOUCHMARK_IMAGE` (Gitea).

The cleanup removes the runner, the registry, the forwarder and the image tags, and any job container or job volume the runner left. The job containers are the runners' own (`runner-…` on GitLab, `GITEA-ACTIONS-TASK-…` on Gitea): their names are not the harness's `touchmark-e2e-*`.

### What the tests cover

- **`TestTemplate` on GitLab** (`internal/e2e/gitlab/template_test.go`), the template README's "On GitLab":
  1. the hub: the template's files in one commit (as GitLab's import of a repository: `[skip ci]`, no pipeline), with the one change a release makes, the image line;
  2. `touchmark setup gitlab` from the maintainer's clone, with the person's token (steps 2 to 4 of the README);
  3. the merge request that sets `id`, `writer` and `targets.yml` (step 5): its pipeline runs `check`, `plan` and `probe`, all pass, and the plan's report (an artifact of the merge request) would open a merge request in each of two opted-in targets;
  4. the merge: the default branch's pipeline runs `distribute` alone, which opens them, by the writer, on `touchmark/<id>`;
  5. CI Lint (`POST /projects/:id/ci/lint`) of the template's `.gitlab-ci.yml` as it ships: valid, no warning, the five jobs; a simulated pipeline of the default branch runs `distribute` alone;
  6. the two schedules setup made, played (`POST …/pipeline_schedules/:id/play`): "touchmark doctor" runs `doctor` alone, "touchmark distribute" runs `distribute` alone, which changes nothing. No log holds a token.
- **`TestTemplate` on Gitea** (`internal/e2e/template_test.go`), the template README's "On Gitea or Forgejo": the hub (the template in one `[skip ci]` commit, the job images pinned to the image under test), the reader's and writer's tokens as Actions secrets; the pull request that sets `id`, `writer` and `security.write_isolation: none` with the template's reason: `check` and `plan` pass (`plan` with `--strict`, as a pull request from the hub's own branch gets the secrets), `distribute` is skipped, and `plan --comment` leaves its report in a comment through the job token; the merge: `distribute` alone, pull requests by the writer in both targets; Run workflow (`POST …/actions/workflows/{file}/dispatches`) of the doctor workflow and of the main one. No job log holds a token.
- **`TestTemplateWorkflow`** (`internal/e2e/github/template_test.go`): a dry run of `.github/workflows/engineering-assets.yml` against `ghfake` as GHES, without GitHub and without act. The test plays the runner: each job's `if` (a small evaluator for the expressions the workflow uses: contexts, `==`, `!=`, `&&`, `||`, `!`, `always()`; strings compare without case, as GitHub's do), `needs`, the secrets and variables a job sees (the repository's, and the environment's when the job names one), the environment's deployment branch policy, each job's `GITHUB_TOKEN` (an App installed on the hub alone, minted with the job's `permissions`), `actions/checkout` (a pull request's merge commit), `run:` steps in bash with `GITHUB_OUTPUT`, and the touchmark Action: this tree's `scripts/action/run.sh` with the step's inputs (the defaults from `action.yml`), the version of a release and env, a `gh` on `PATH` that accepts the attestation of that release's digest, and a `docker` that resolves the release's tag to that digest and runs touchmark, built from this tree, by that digest, with only the environment and the user `run.sh` gives the container. The runs: the pull request that describes the hub (`check`, `plan` and its comment), its merge (`probe`, `distribute`), the doctor schedule, the daily schedule (nothing to change), Run workflow; Dependabot's pull request that bumps the Action and a pull request from a fork, which get no Actions secrets (a fork's `GITHUB_TOKEN` read-only too): `plan` runs offline, without `--strict`, and passes with its warning; then a write key leaked into a repository secret: the plan job's probe step fails, and `distribute` and `doctor` refuse (`TOUCHMARK_KEY_EXPOSED=true`).

- **`TestTemplatePipelines`** (`internal/e2e/bitbucket/template_test.go`): no live Bitbucket and no runner. It reads `bitbucket-pipelines.yml` and checks what keeps the write key on the default branch: the image pinned by digest and a full clone; the pipelines (a pull request: `probe`, `check`, `plan --strict --comment`; the push of `main` and `master` and the custom `distribute`: `probe`, then `distribute`; the custom `doctor`: `probe`, then `doctor`; no default or tag pipeline); only `distribute` and `doctor` with `deployment: touchmark-distribute`; no script that names a `TOUCHMARK_` variable, and none in a custom pipeline that names any variable (Run pipeline would ask for it). Then it plays each pipeline with the variables Bitbucket gives a step (the repository's to every step, the deployment's to its steps only) through touchmark's probe and guards: everything runs on the default branch of a hub that states its Premium restriction; a custom pipeline on another branch, and `platform` without the statement, are refused; a write key leaked into a repository variable stops every pipeline at its probe.
- **`TestTemplateAzurePipelines`** (`internal/e2e/azure/template_test.go`): no live Azure DevOps and no agent. It reads `azure-pipelines.yml` and `.azure-pipelines/touchmark.yml` and checks what keeps the write key on the default branch: one step template that runs the image pinned by digest with docker, as the agent's user, mapping the writer's variable only under `write`; every checkout with the whole history; the stages (`probe` first without the variable group; `check` and `plan --strict --comment` for a pull request; `distribute` and `doctor` as deployment jobs to the environment `touchmark-distribute` that link the group `touchmark-distribute`, which nothing else links). It then plays a pull request, pushes, both schedules and both manual runs, evaluating the stages' conditions with a small evaluator (`and`, `or`, `not`, `eq`, `ne`, `in`, `succeeded()`, `variables[…]`): the expected stages run and pass touchmark's guards for a hub that states its Branch control check, `platform` without the statement is refused, and a write key leaked into a pipeline variable stops every run at its probe.

### What the runs found (2026-10-02)

| Finding | Fixed |
|---|---|
| A new hub's first pull request, the one that names the writer, failed `plan --strict` with exit 3 on every platform: in a hub pull request plan reads the providers from the default branch, whose `hub.yml` (the template's) names no writer, so plan could not recognize its own pull requests, turned the sweep off, and `--strict` counts a sweep that did not run | `plan` takes the pull request's writer where the default branch names none, with a warning (`cli.planProviders`, `TestPlanHubPullRequestProviders/added_writer`) |
| GitLab's CI Lint simulates no job for a default branch whose tip is a `[skip ci]` commit (the import) | none needed: the test lints after the merge |
| GitLab's `docker inspect` failed now and then on a loaded machine, and `gitlab.sh` took the empty answer for a stopped GitLab | the readiness wait stops only on an answer that says not running |

What held, on GitLab CE 18.11.12 with the runner 18.11.4 and on Gitea 1.27.3 with its runner 4.1.0: the image as uid 65532 reads a checkout another user made (`safe.directory`) and writes the report files; GitLab's Docker executor runs the job script with the image's `sh` (`entrypoint: [""]`) and its helper uploads the artifacts; `GIT_STRATEGY: none` for the probe; the merge request pipeline gets the reader's variable but not the protected, environment-scoped write key (the probe passes); `distribute` on the protected default branch in the environment `touchmark-distribute`; the schedules told apart by description. Gitea's runner keeps the job container alive with `sleep`, gives the workspace to the image's user (`chown -R 65532:65532`), runs `run:` steps with the image's `sh`, resolves the YAML anchor of the clone step, and lets the job token (Basic auth, any user name) fetch the hub and comment on the pull request.

Not covered: GitLab's "Run pipeline" (source `web`, which the API cannot start: `POST /projects/:id/pipeline` is source `api`, which the template's `workflow:rules` filter out), Forgejo's runner, and the step summary on Gitea. The GitHub workflow runs on GitHub itself in the bedrock-python hub; its negative checks (item 17 below) are not covered.

## GitHub

There is no live GitHub sandbox: `internal/e2e/github` runs the GitHub driver (`internal/platform/github`) end to end against `ghfake` (`internal/platform/github/ghfake`), an HTTP fake of GitHub's REST API, GraphQL API and git smart HTTP on one local server, backed by real bare repositories. The package has no build tag and needs no network: it is part of the ordinary `go test ./...`. What the fake does and where each behavior comes from (docs, read-only observation, or assumed) is the table in `ghfake`'s package documentation. A test that depends on an assumed behavior proves only the fake; [what only a live GitHub confirms](#what-only-a-live-github-confirms) lists those behaviors and what the bedrock-python hub confirms on github.com.

### Run

```sh
go test ./internal/e2e/github/                            # conformance as github.com, shapes, batch reads
TOUCHMARK_HEAVY_TESTS=1 go test ./internal/e2e/github/    # also the conformance as GHES outside Linux
```

`TestDistribute` and `TestDistributeUnsigned` run `distribute`, which needs git 2.45 or later: on an older git they skip, and the Docker run of the unit suite (`golang:1.27`, git 2.47) covers them:

```sh
docker run --rm --name touchmark-e2e-unit-suite -v "$PWD:/src" -w /src golang:1.27 \
  sh -c 'git config --global --add safe.directory /src; go test -race -count=1 -timeout 45m ./...'
```

On Windows the package takes about 100 seconds (the fake starts git processes for most requests). In the Docker run of the whole suite with the race detector it took 4 to 7 minutes on 2026-09-29, beside the other packages. The conformance as GitHub Enterprise Server is a heavy test: it runs on Linux, elsewhere with `TOUCHMARK_HEAVY_TESTS=1`.

The fake's GraphQL schema is a subset of GitHub's, field for field. `TestSchemaSubset` checks it against the public schemas when they are downloaded:

```sh
curl -sSfLo fpt.graphql  https://docs.github.com/public/fpt/schema.docs.graphql
curl -sSfLo ghes.graphql https://docs.github.com/public/ghes-3.19/schema.docs-enterprise.graphql
GHFAKE_SCHEMA=$PWD/fpt.graphql GHFAKE_SCHEMA_GHES=$PWD/ghes.graphql go test -run TestSchemaSubset ./internal/platform/github/ghfake/
```

The driver's REST routes (the table in `internal/platform/github/contracts_test.go`, which the unit tests' server enforces on every request the driver sends) are checked the same way against github/rest-api-description:

```sh
rest=https://raw.githubusercontent.com/github/rest-api-description/main/descriptions
curl -sSfLo api.github.com.json "$rest/api.github.com/api.github.com.json"
curl -sSfLo ghes-3.19.json "$rest/ghes-3.19/ghes-3.19.json"
GHFAKE_REST_DESCRIPTION=$PWD/api.github.com.json GHFAKE_REST_DESCRIPTION_GHES=$PWD/ghes-3.19.json go test -run TestRESTContracts ./internal/platform/github/
```

CI's job `contracts` does both on every pull request and push, and fails when a download fails or a check skips.

### What the tests cover

- **`TestConformance`**: the platform contract suite `internal/platform/conformance` against `github.NewReader` and `github.NewWriter` over the fake, twice: as github.com (the fake's github.com flavor, the provider's host `github.com`, the API at the fake's root) and as GitHub Enterprise Server (`/api/v3`, the fake's own host, web commit signing off). The reader and the writer are GitHub Apps (`touchmark-read`: contents, pull requests and metadata read; `touchmark-write`: contents, pull requests and workflows write), installed on the organization `acme`; the writer's installation holds the repositories the suite may write (a read-only repository is one outside it, so `Target` answers not-found). `alice` is the other account, with a classic personal access token. Forks come from `octo-org` or from `alice`. Nested namespaces do not exist on GitHub (`Resolve/subgroups` skips), and GitHub frees an old login at once instead of redirecting it (the renamed-account half of `Renamed` skips). The fake's journal (`Violations`) must stay empty.
- **`TestDistribute`**: `plan` and `distribute` through `cli.Main` from a hub in a temporary directory, against the fake as a GitHub Enterprise Server with web commit signing (the command line derives `/api/v3` from `url`, so the drivers see GHES). App ids and keys are generated at run time and passed in `TOUCHMARK_GH_READ_APP_ID`/`_KEY` and `TOUCHMARK_GH_WRITE_APP_ID`/`_KEY`; the writer's installation starts without Workflows. Eleven repositories of `acme`:
  - pull requests by `touchmark-write[bot]` with the title, label, body, marker, and a commit by the bot with touchmark's trailers, as drafts (`pr.draft: true`), and a ready one in a private repository that refuses drafts;
  - a target whose default branch requires signed commits (a ruleset): the commit is made by GitHub through the API and verified, the writes are `push` (the stage ref), `api-commit`, `update-refs`, and no `refs/touchmark/` ref is left; a later update goes the same way and the pull request stays open;
  - a pack with a workflow while the installation lacks Workflows: `blocked:permission:workflows` before any write; after the owner grants Workflows, the pull request opens;
  - a rebuild that moves a sync branch across a person's workflow on the default branch without Workflows: `blocked:permission:workflows`, one body edit asking for Update branch; rebuilt once Workflows is granted;
  - a ruleset that forbids force pushes on `touchmark/**`: the first run opens, a pack change is `blocked:rules:non-fast-forward` with nothing written; one that restricts the creation of `touchmark/**`: the push is refused, `blocked:rules:ruleset`;
  - a person's close is a decline (the closer read from GraphQL's `ClosedEvent`), with an ack and one comment; the bot's own close is a self-close, and the content comes again in a new pull request;
  - a pack change updates the open pull request in place;
  - a target leaves `targets.yml`: the sweep (installation repositories and batched GraphQL) closes its pull request as `closed:target-dropped`;
  - a person's pull request from a fork on the sync branch's name, with a copy of the marker, is never touched;
  - after every run, a second one writes nothing (by the report and by the fake's state); every per-target token was minted for one repository and revoked, and only those of the two targets whose writes change workflows carry workflows write; every reading token of the writer is revoked when its run ends; the journal is empty (no token used on another repository, no write with a revoked token, no branch of an open pull request deleted or moved onto its base); no secret (App keys, the person's token, every token the fake minted) appears in any output, report, pull request, comment or commit message.
- **`TestDistributeUnsigned`**: the same hub on a GitHub Enterprise Server without web commit signing: the API commit comes back unsigned, the target is `blocked:cannot-sign`, the branch never moves to it, no pull request opens, and the stage ref is deleted. The next run tries again (two writes: the stage push and the API commit), since the platform may have started signing.
- **`TestBatchReader`**: the driver's GraphQL batch read of one path in many repositories agrees with `ReadFile` for a regular file, an executable, a binary file, a symlink, a submodule, a directory, a missing file, a file over the limit, an empty repository and a repository that does not exist. `plan` and `distribute` read the opt-in files of GitHub targets with it.
- **`TestShapes`**: `testdata/shapes.json` holds the shapes of github.com's answers to read-only requests on public repositories (2026-09-29), trimmed to the fields the driver reads and reduced to JSON types, with no value, login, name or id: a repository and a listing item, a pull request from a fork, a tree with one entry of each mode, a blob, contents of a file, a symlink and a submodule, `rules/branches` for an existing and a missing branch, `hash-algorithm`, a bot and a user, a label, a 404, a commit GitHub signed, `/meta`, the `Link` header of a pull request listing, `git/ref/heads/{branch}` of a branch (one ref) and of a missing one whose name starts another's (404, not a list of matches), and GraphQL's errors with HTTP 200 (a missing repository alias, a missing `Commit.file` path, an unknown field, an unused variable), `Commit.file` modes, an open pull request's node with every field the driver reads, and a bot's `ClosedEvent`. The test asks the fake the same and compares fields, types, pinned values and fields GitHub leaves out.
- **`TestGraphQLContracts`** (`internal/platform/github`): every fixed GraphQL document the driver sends validates against the schemas of github.com and GHES 3.19 (the fake's subsets, which `TestSchemaSubset` checks against GitHub's). `TestSchemaSubset` runs in CI (job `contracts`) with both schemas downloaded.
- **`TestRESTContracts`** (`internal/platform/github`): every REST route the driver's unit tests see it call is in its table of routes, and, with `GHFAKE_REST_DESCRIPTION` and `GHFAKE_REST_DESCRIPTION_GHES` naming downloaded copies of github/rest-api-description (`api.github.com.json`, `ghes-3.19.json`), every route of the table exists there with its method and every query parameter the driver sends is declared (CI job `contracts`).
- **`TestUserNamespace`, `TestOwnerRenamed`, `TestSuspendedInstallation`, `TestRulesetBypass`** (`driver_test.go`): a user's private repositories are resolved through the App's installation or the user's own token (another user's token gets the public ones, marked incomplete); an explicit target under a renamed owner is missing (the API answers 404 under the old name) while the sweep lists its pull request under the new name, and `decide.Sweep` leaves it open; a suspended installation listed first stops neither `Self` nor the sweep, which notes it; a ruleset with the writer's App on its bypass list does not block the writer's rebuild, while the reader, which cannot tell, reports it.
- **`touchmark setup github`** (`internal/setup`, `internal/cli`): `TestGitHubSetup` runs setup against the fake with a test that plays the person's browser: it opens setup's page on `127.0.0.1`, posts the manifest form to the fake's "new App" page (which confirms at once and redirects with a code, as GitHub does once the person confirms) and follows the redirect to setup's callback. Setup must make the environment `touchmark-distribute` with `main` as its only deployment branch, the two Apps with exactly the manifest's permissions, their ids in the repository's and the environment's variables, the ruleset, the private keys in files of the key directory (mode 0600 outside Windows) and the `gh secret set` commands; after the test sets the secrets as `gh` would (`SetSecret`), a second run and a dry run write nothing. `TestGitHubRepairs`, `TestGitHubFreePrivate`, `TestGitHubPreconditions`, `TestGitHubDryRunFresh`, `TestGitHubExternal`, `TestGitHubUserHub` and `TestManifestFlowPage` cover a repaired environment (its wait timer kept, an extra branch policy deleted, a write key in a repository secret failing the check), a private hub on GitHub Free, the refusals, the dry run, external isolation, a personal account's hub and the page's checks; `TestSetupGitHub` runs the command line. The GitLab side runs against an in-memory fake in `internal/setup` (`TestGitLab*`) and live in `TestSetup` above.
- **Conformance `Commit` and `Preflight`**: for a writer that offers them, the API commit through a stage ref with compare-and-swap (a stale lease and a lease on no branch are conflicts that move nothing; the commit leaves the branch at the returned commit of the tree asked for on the parent, deletes the stage ref, and the pull request on the branch stays open), and the rules of a branch that does not exist yet read with no error. The in-memory fake in git mode (GitHub flavor) runs them too.

### What the integration found (2026-09-29)

Running the driver against the fake, and the fake against github.com's answers, found these differences. All were in the fake; the driver and the core needed no change.

| Difference | Fixed |
|---|---|
| The fake had no `GET /meta` and no `X-GitHub-Enterprise-Version` header: the driver's `Probe` failed on every GHES | the fake: `/meta` (github.com without `installed_version`, as observed; GHES with it) and the header on every GHES answer |
| `git/trees/HEAD`, `contents?ref=HEAD` and `commits/HEAD` name the default branch on github.com (observed; `git/commits/HEAD` is 404); the fake answered 404, so `ReadFile` failed | the fake resolves `HEAD` |
| The fake's GraphQL subset lacked `PullRequest.baseRef`, `Commit.file(path:)` and `Mannequin`, which the driver's queries select: every pull request listing failed validation | added, checked against both schemas |
| `Commit.file(path:)` of a missing path, or one through a symlink, is null with a `NOT_FOUND` error on the field (observed); the fake gave null alone | the fake; the driver handled both already |
| GitHub refuses a declared variable the query does not use (`variableNotUsed`, observed); the fake accepted it | the fake; the driver's documents pass |
| On GHES the fake gave bots github.com's noreply domain, the driver `users.noreply.<host>` | the fake follows the driver (both assumed: GHES does not document the form) |
| `rules/branches` leaves `parameters` out for rules without parameters (`non_fast_forward`, `deletion`) | the fake already did |

Also observed read-only and matching the fake: `pulls?head=<branch>` without an owner is ignored silently (every pull request comes back), `head=<base owner>:<branch>` does not find a fork's pull request; `rules/branches` answers 200 for a branch that does not exist; `has_pull_requests` in repositories and listings; a symlink in `contents` shows the target's content under the link's name, a submodule has type `submodule` and `submodule_git_url`; tree entries of trees and submodules have no `size`, submodules no `url`; `ClosedEvent.actor` is `Bot` with a login without `[bot]` and a `databaseId`, a merge leaves a `ClosedEvent` too, `headRepository` is null once a fork is gone; `Link` rels come as prev, next, last, first, with URLs under `/repositories/<id>` and the request's query order; GraphQL answers `NOT_FOUND` per alias with the other aliases' data and HTTP 200.

To redo a check, ask the same of any public repository with `gh api` (GET only) or `gh api graphql` (queries only), reduce the answer to types with jq, and compare with `testdata/shapes.json`:

```sh
SHAPE='def shape: if type=="object" then with_entries(.value|=shape) elif type=="array" then (if length>0 then [.[0]|shape] else [] end) else type end; shape'
gh api repos/<owner>/<repo>/git/trees/HEAD --jq "$SHAPE"
gh api -i "repos/<owner>/<repo>/pulls?state=all&per_page=2&page=2" | grep -i '^link:'
gh api graphql -f query='query { a: repository(owner: "<owner>", name: "<repo>") { databaseId } b: repository(owner: "<owner>", name: "<missing>") { databaseId } }'
```

### What only a live GitHub confirms

What `ghfake` assumes and only a live GitHub settles, above all which writes count against GitHub's content-creation limits and when a push needs the Workflows permission. No sandbox organization runs these items, and none is planned. touchmark runs on github.com for real in the bedrock-python hub ([bedrock-python/engineering-assets](https://github.com/bedrock-python/engineering-assets)), whose runs since 2026-10-07 take the positive paths of items 15 to 17 and 21: per-target tokens, the App's bot as the author of the sync commits, the environment `touchmark-distribute` that only `master` may use, with `probe` and `doctor` reading it, the step summary, and the targets listed through the installation. The rest stays assumed. Each item says what to observe and record; a finding corrects the fake, and the driver where it assumed wrong.

The items name three GitHub Apps:

| App | Permissions | Used by |
|---|---|---|
| READ | contents, pull requests: read (metadata read) | plan, the reader |
| WRITE | contents, pull requests, workflows: write (metadata read); installable without workflows | distribute, the writer |
| FIXTURE | administration, contents, pull requests, workflows: write | setup only: repositories, rulesets, people's actions through a person's token |

and some need a person's account with a fine-grained personal access token, and a second organization for forks.

1. **Content-creation accounting**. GitHub limits content creation to 80 a minute and 500 an hour and does not say which writes count. With WRITE's per-target token on a scratch repository, make 81 writes of one kind within a minute, one kind per run, and see whether the 81st is refused ("You have exceeded a secondary rate limit", 403 or 429, `retry-after`): create a pull request, edit its body, close and reopen it, comment, create a label, add labels (`POST issues/{n}/labels`), `POST /git/commits`, the `updateRefs` mutation, `POST`/`PATCH`/`DELETE git/refs`, a git push of a new branch, a force push, a push to `refs/touchmark/…`, a token mint and `DELETE /installation/token`. Record which kinds count, and correct the fake's `limits.go` (its counted kinds are partly assumed) and the write estimates of `plan`.
2. **When Workflows is needed**. WRITE installed without workflows, pushing with its per-target token: (a) a new branch that adds `.github/workflows/x.yml`; (b) a new branch without workflow changes from a default branch that has workflows; (c) a push to `refs/touchmark/<fp16>/stage` whose tree changes a workflow; (d) a force push that moves a sync branch onto a new base carrying a person's workflow change (a rebuild across others' changes; public reports say it needs Workflows); (e) the branch deleted and pushed afresh from that base (the path that recreates a sync branch, unverified); (f) `POST /git/commits` with a tree that changes a workflow, then `updateRefs` moving the branch to it. Record allowed or refused and the exact message (git's stderr, GraphQL's error type) for each; repeat with workflows granted (all allowed). The fake judges a ref write by its diff from the old tip (from the default branch for a new ref): it refuses (a), (c), (d) and the `updateRefs` of (f), and allows (b) and (e) (assumed).
3. **Secondary limits**. Bursts of reads with one installation token until refused: REST (900 points a minute), GraphQL (2 000 points a minute), and 100 concurrent requests. Record the status (403 or 429), `retry-after`, `x-ratelimit-*`, the body's message, and whether GraphQL answers HTTP 200 with an error or 403; compare with the driver's classification (`errors.go`) and the fake's `limits.go`.
4. **Stage-ref API commit**. On a target whose default branch requires signatures (a ruleset): `distribute` pushes the stage ref, `POST /git/commits` without author, committer or signature, and `updateRefs` with `force: true`. Record `verification` (verified, reason `valid`), the author (the bot and its noreply address) and the committer (GitHub), that the pull request stays open after `updateRefs` and what its timeline shows, and that no `refs/touchmark/` ref is left. Negative, with FIXTURE: moving the head onto its base closes the pull request.
5. **`updateRefs` with a non-branch ref deletion** (unverified): one `updateRefs` call that moves `refs/heads/<b>` (`beforeOid` H) and deletes `refs/touchmark/<fp16>/stage` (`afterOid` zeros). Record whether GitHub accepts it atomically; if not, its error, and which fallback works: `DELETE /git/refs/touchmark/…` or a git push that deletes with a lease. The driver falls back by itself (`splitStage`); the report's ops tell which path ran (`delete-ref`).
6. **Hidden refs**. Whether `refs/touchmark/*` is advertised by `git ls-remote` and fetched by clones, whether a push there starts a workflow `on: push` without a branch filter in the target, and whether it needs Workflows when its tree changes a workflow (item 2c).
7. **Lease refusals**. `updateRefs` with a stale `beforeOid`: GraphQL's error type and message (the fake answers `STALE_DATA`, assumed); a git push with a stale `--force-with-lease`: the porcelain line gitx parses.
8. **Who closed**. The last `ClosedEvent.actor` when a person closes, when WRITE's bot closes with its token, when a person merges (`mergedBy` too), when a person deletes the head branch, and when a push moves the head onto its base: `__typename`, login (without `[bot]` for a bot) and `databaseId` equal to the REST id. The fake names the deleter and the pusher (assumed).
9. **Rulesets**. On targets: `required_signatures` on the default branch; `non_fast_forward` on `touchmark/**`; `creation` restricted on `touchmark/**`; each with WRITE in the ruleset's bypass list and without. Record what `rules/branches/{b}` returns to WRITE (the docs say it lists every active rule whoever may bypass it, and so does the fake), what `GET /repos/{o}/{r}/rulesets/{id}` says in `current_user_can_bypass` for WRITE's *reading* installation token (the writer's `Preflight` drops the sync branch's rules of a ruleset that answers `always` or `exempt`, assuming the reading token acts as the App as its per-target token does), and whether the pushes then succeed with the per-target token. Also: an organization ruleset read through the repository endpoint (`includes_parents` defaults to true), and READ's view (metadata only: `rules/branches` and `GET /rulesets/{id}` readable?).
10. **A ruleset pairing `deletion` with `non_fast_forward`**, as GitHub's ruleset form preselects "Restrict deletions" and "Block force pushes": with no open pull request and a rewritable sync branch, touchmark would delete the branch and create it again (`StepRecreateBranch`); it reads `deletion` from `rules/branches` (`Rules.NoDelete`) and blocks the target before any write (`blocked:rules:non-fast-forward`), the plan too. Confirm that GitHub refuses the delete push (GH013 "Cannot delete this protected branch"?), and that `restrict creations` on `touchmark/**` refuses the first push (the report says `blocked:rules:ruleset` at run time; `plan` predicts `opened`, since creation is not read upfront).
11. **Classic branch protection**. Protect a target's default branch the classic way with "Require signed commits". As WRITE (no Administration): `GET /branches/{b}` (`protected`, `protection`), `GET /branches/{b}/protection` (403 or 404?), `rules/branches` (empty?), `doctor`'s `rules` finding; then `distribute` with `sign: auto` (the unsigned sync commit is pushed; can the pull request be merged?) and with `sign: always` (the API commit). Record what touchmark sees before any write.
12. **Update branch with rebase, and with a merge after a Workflows block**. A person presses Update branch (rebase) on touchmark's pull request; the next run: what GitHub's rebased commit carries (committer GitHub, author the bot?), and how touchmark classifies the branch (expected: no write, or a rebuild). Then the recovery the pull request body promises: WRITE without workflows, a person's workflow change on the default branch, a pack change blocks the rebuild (`blocked:permission:workflows`, the body asks for Update branch); the person presses Update branch (merge); the next run must push the pack change on top of the merge without Workflows (no workflow diff from the branch's head) and update the pull request. `TestDistribute` step 5 grants Workflows instead of pressing the button; the fake does not model Update branch.
13. **Drafts**. A private repository of the Free organization: `POST /pulls` with `draft: true` → 422 and its message; the driver opens a ready pull request instead. The fake's message is assumed.
14. **Body limit**. Bodies of 65 536 and 65 537 ASCII characters, and of 65 536 two-byte characters: accepted or refused, and the message; whether GitHub counts code points, UTF-16 units or bytes. touchmark's budget is 58 000 bytes.
15. **Tokens**. `DELETE /installation/token` → 204, and the token then fails at once on REST, GraphQL and git; a per-target token (`repository_ids=[id]`) on another repository: REST, GraphQL and git answers; the length of github.com's installation tokens (about 520 characters since 2026-04-27) and that every output masks them; the mint's 422 messages for a permission the installation lacks and for a repository outside it (the driver maps them to permission and not-found); whether an installation token of an organization sees its public repositories outside the selection.
16. **The App's identity**. `GET /app` (the slug), `GET /users/<slug>%5Bbot%5D` (the id), the commit email `<id>+<slug>[bot]@users.noreply.github.com` attributes pushed commits to the bot; `GET /user` with an installation token answers 403 "Resource not accessible by integration" (`Self` refuses such a token with that message); on GHE.com and GHES the noreply domain (assumed `users.noreply.<host>`).
17. **The hub in GitHub Actions**. A hub repository with the environment `touchmark-distribute` restricted to the default branch and the write key as its secret: a job of another branch naming the environment is refused; `touchmark probe` fails with exit 2 when the key is also a repository secret; the hub channel reads the environment and its deployment branch policies with `GITHUB_TOKEN` and `permissions: actions: read` (the shapes were checked anonymously on public repositories only); the step summary and annotations; a public hub skips private targets. distribute's environment check fails closed: an environment set to "Protected branches only", or one the token cannot read, stops it with exit 2; confirm that a hub protecting its default branch only with a ruleset lets every branch deploy to a "Protected branches only" environment (the docs say so for a repository without branch protection rules).
18. **git history**. `git fetch --shallow-since` (`deepen-since`) against github.com with a per-target token: the history the core reads to classify the sync branch.
19. **Renames and transfers.** A renamed and a transferred target: GET follows the 301 to `/repositories/<id>` with an installation token; git on the old path. A target transferred from an owner without the App to one with it: `GET /repos/{old owner}/{repo}/installation` with the JWT follows the redirect (the driver's fallback when the owner has no installation). An organization renamed while `targets.yml` names `repo: <old>/<name>`: API requests under the old name answer 404 (docs), the target is `target-missing`, and the sweep must leave its pull request, listed under the new name, open (`decide.Sweep` skips repositories named like an unresolved target).
20. **Pull request edge cases the fake assumes**: reopening after the head branch was deleted or force-pushed (the message); `PATCH /pulls/{n}` with title, body, state and base when the base is invalid (all or nothing?); the head moved behind its base; the base deleted (retargeted or closed); labels added by name that do not exist yet (created?).
21. **The sweep at scale**. `GET /installation/repositories` with the reader's token and `nodes(ids:)` over 50 repositories per request: complete, and the node ids of repositories in GitHub's current format. A suspended installation: `suspended_at` in `GET /app/installations`, the mint's refusal message (the driver matches "suspended"), and that `Self` and lookups still work through another installation. A user's namespace: `GET /installation/repositories` with the user's installation token lists the user's private repositories (`GET /users/{u}/repos` lists public ones only, per the docs), and `GET /user/repos?affiliation=owner` with the user's own token.
22. **GitHub Enterprise Server** (optional: CI checks GHES by its API contracts): on a GHES 3.19 or later instance, `/meta` `installed_version`, the header, 40-character tokens, no `/hash-algorithm` (the driver asks up to four repositories before it knows, then stops), and API commits unsigned with web commit signing off and verified with it on (`TestDistributeUnsigned` and `TestDistribute` on the fake).
23. **GHE.com** (optional): `api.<subdomain>.ghe.com` and its GraphQL endpoint, flavor detection by the `.ghe.com` suffix.
24. **The scenarios every platform runs**, with the live fixture, as `internal/e2e/gitlab` runs them for GitLab: a fork's pull request with the branch's name and a copy of the marker is not touchmark's (`TestDistribute` covers it on the fake); someone else's open pull request on the sync branch gives `branch-in-use`; a merge of a branch other than the base into the sync branch gives `blocked:edited`; a hub rolled back to a declined version after a merge opens a pull request; a change of the hub's `id` keeps the pull request and the memory (the old branch as an alias); an `ignore` edit in the opt-in file lifts a decline and a comment edit does not; a title edit or taking a pull request out of draft causes no write; a new engine version does not rewrite bodies. On the fake these run in the core's tests (`internal/distribute`, property and sequence tests) and, for GitHub, only partly in `TestDistribute`.
25. **Message fidelity of `POST /git/commits`**. The message of the commit GitHub makes must come back byte for byte (the trailing newline, the blank line before the trailers, the trailers): after an answer lost, the core compares the branch's head message with the one it sent (`commitApplied`), and a normalized message would turn every lost-answer reconcile into a conflict. Record the message of `GET /git/commits/{sha}` and of `git cat-file` for a message with and without a trailing newline, with CRLF, and with trailing blank lines.
26. **The writes an API commit costs**. Count the content-creating requests per target on the fake's and the live journal (`POST /git/commits` twice when the first answer is unsigned, `updateRefs`, the split path's second `updateRefs` and `DELETE /git/refs`), and compare with the report's `writes` and the estimate `writesAPICommit` (3).
27. **`touchmark setup github`**, with an organization owner in a real browser: GitHub's "new App" page takes the manifest from setup's page and redirects to `http://127.0.0.1:<port>/callback` with the code and the state (the fake assumes it accepts a loopback `redirect_url` and an inactive webhook whose URL is the hub's); the App gets exactly `default_permissions` (metadata read included, nothing more); the conversion's answer has `id`, `slug`, `pem`, `client_id`, `client_secret` and `webhook_secret`. Then, with the organization owner's token: `PUT …/environments/touchmark-distribute` with custom branch policies on a public hub, and what a private hub on the Free plan answers (the fake answers 422, assumed); whether a `PUT` without `reviewers` and `wait_timer` clears them (the fake clears them, so setup sends them back); a duplicate deployment branch policy (303 in the docs, 422 in the fake); `POST …/actions/variables` for an existing name (409 in the docs); `POST …/rulesets` on a Free private hub (the fake answers 403 "Upgrade to GitHub Pro…", from reports); `gh secret set … --env touchmark-distribute < writer.pem` and the secrets listing setup reads afterwards; and `doctor --hub-token` grading the result ok.
