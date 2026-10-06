# touchmark

**Keep shared files in sync across many repositories through pull requests, without
overwriting what a team has made its own.**

touchmark is the engine behind **engineering-assets hubs**. A hub is a repository your
organisation creates from
[the hub template](https://github.com/bedrock-python/engineering-assets-template). It holds
*packs* of shared files (agent instructions, review prompts, merge request templates,
shared CI files) and a list of target repositories. Whenever a pack changes, touchmark
opens a pull request (a merge request on GitLab) in every target that has opted in. A
person reviews it and merges it.

Three ways in, one version number:

- **GitHub Action** — `uses: bedrock-python/touchmark@<commit> # vX.Y.Z` in the hub's
  workflow. The hub template ships the workflow. See [Run it in CI](guide/ci.md).
- **Container image** — `ghcr.io/bedrock-python/touchmark:<version>`: the same binary with
  git, for GitLab CI, Gitea and Forgejo Actions, or `docker run` on your machine.
- **Binary** — an archive for Linux, macOS or Windows from the
  [releases](https://github.com/bedrock-python/touchmark/releases), or
  `go install github.com/bedrock-python/touchmark/cmd/touchmark@latest`. See
  [Installation](getting-started/installation.md).

!!! agents "Setting up a hub with an AI assistant?"

    Hand it **[one page](agents.md)** instead of this site. It carries every command and
    configuration key, the rules that break a hub when they are broken, the mistakes
    models actually make, and a map of which page to fetch for everything it leaves out.
    Every page here is also served as raw Markdown at its own URL — `/agents.md`,
    `/concepts/ownership.md` — and the **Copy page** button at the top of each one hands
    it straight to a chat window.

```sh
cd ~/src/billing                      # a checkout of a target repository
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

In the hub's CI the same decision becomes a pull request in every target, and a report
of what each one would receive on every pull request to the hub.

## Why you might want it

- **It only touches files it can prove it shipped.** touchmark manages a file only while
  its content is, byte for byte, a version the hub has shipped at that path. Change one
  line and the file is the repository's own: touchmark never overwrites or deletes it
  again. See [ownership by provenance](concepts/ownership.md).
- **It keeps no state in your repositories.** No lock files, no markers or generated
  headers in your files. Ownership comes from the hub's git history, and the memory of
  declined changes lives in the pull requests themselves. See
  [memory of declined pull requests](concepts/memory.md).
- **It reviews, never merges.** Every change arrives as a pull request that a person
  merges. There is no auto-merge option.
- **It keeps the write key where an attacker cannot reach it.** A reader plans on hub
  pull requests, a different writer delivers from the default branch only, and a probe
  fails the run when a branch could see the write key. See the
  [security model](concepts/security.md).
- **One hub, several platforms.** GitHub (github.com, GHE.com, GitHub Enterprise Server),
  GitLab (gitlab.com and self-managed), Gitea and Forgejo, at once from one hub.

## Where to start

<div class="grid cards" markdown>

- **Getting started**

    Create a hub from the template on GitHub, GitLab, Gitea or Forgejo, and opt the
    first repository in.

    [Start here →](getting-started/installation.md)

- **Concepts**

    How touchmark decides: ownership, packs and opt-in, the sync branch, declined pull
    requests, the accounts.

    [How it works →](concepts/overview.md)

- **How-to guides**

    Set up a platform with one command, write packs, run it in CI, migrate from
    multi-gitter, troubleshoot.

    [Guides →](guide/setup.md)

- **Reference**

    Every command and flag, every key of every configuration file, the reports and the
    exit codes.

    [Commands →](reference/commands.md)

- **For AI agents**

    One page holding the commands, the configuration, the rules that break a hub when
    broken, and a map of everything else — to hand to a coding assistant instead of the
    site.

    [Agent context →](agents.md)

</div>

## At a glance

| | |
|---|---|
| Platforms | GitHub (github.com, GHE.com, GHES 3.19+), GitLab 17.0+, Gitea 1.26+, Forgejo 15+ |
| Runs as | a GitHub Action, a container image, a single static binary |
| Needs | git 2.31+ for `check`, `status`, `apply`; git 2.45+ for `plan` and `distribute` (the image brings its own) |
| Configuration | `hub.yml`, `targets.yml`, `.touchmark/operations.yml` in the hub; `.engineering-assets.yml` in a target |
| Safety | ownership by content, never by name; no state in targets; reader ≠ writer; no auto-merge |
| Source | [github.com/bedrock-python/touchmark](https://github.com/bedrock-python/touchmark) |
| License | Apache 2.0; the hub template is MIT-0 |
