# Write packs

A pack is a directory in `packs/`, laid out like the target repository. Write it in a
branch of the hub, try it on a checkout of a target, and let the hub's pull request show
what every target would receive.

## Add a pack

```sh
cd ~/src/engineering-assets
git switch -c feat/review-prompt
mkdir -p packs/review/.agents/prompts
$EDITOR packs/review/.agents/prompts/review.md
```

Describe it, and its dependencies, in `hub.yml`:

```yaml
packs:
  review:
    description: The review checklist agents follow
    requires: [agents]
```

Assign it in `targets.yml`, to everyone or to some:

```yaml
targets:
  - org: acme
    topics: [python]
    packs: [review]
```

## Try it before you commit

`--worktree` reads packs and configuration from the hub's working tree instead of its
last commit:

```sh
touchmark check --hub . --worktree
cd ~/src/billing
touchmark status --hub ~/src/engineering-assets --worktree
touchmark apply --hub ~/src/engineering-assets --worktree --dry-run
```

`distribute` ships only what is committed on the default branch, and refuses
`--worktree`.

## Open the pull request

Commit, push, and open a pull request in the hub. `check` validates it; `plan` reports,
for every target whose packs it changes, what it would receive, and lists the changes by
path across the targets with the sensitive ones first:

```text
This hub pull request changes, across the targets it affects
  update  .claude/settings.json      SENSITIVE  15 targets
  add     .agents/prompts/review.md             12 targets
```

A pull request that changes only `packs/<p>/` plans the targets that get `p`. One that
changes `hub.yml`, `targets.yml`, `.touchmark/`, `schemas/` or a file `hub.yml` names
plans every target; `plan --all` does too.

## Rules a pack follows

- **Every file is delivered.** Describe a pack in `hub.yml`, not in a README inside it.
- **At least 64 bytes per file.** Smaller content can't prove where it came from, and
  `check` rejects it.
- **Regular files only**, mode 100644 or 100755: no symlinks, no submodules. An
  executable file is listed under ⚠ in every pull request.
- **No case collisions.** Two paths that differ only by case, or a name that is a file in
  one pack and a directory in another, break on Windows and macOS checkouts; `check`
  rejects them. The same path in two packs is an override: the later pack wins.
- **Reserved files.** Never ship the opt-in file (`.engineering-assets.yml`), or a
  top-level `.lfsconfig`, or anything under `.git`.
- **Seed files.** A file a team is expected to fill in, like a project profile, ships as a
  skeleton. Once edited, it belongs to the repository.
- **Project facts stay out.** Keep tracker keys, branch patterns, the pull request
  language and build commands out of shared files; they belong in the project profile.

## Sensitive paths

Changes to these paths are listed under **⚠ Sensitive paths** at the top of every sync
pull request and in `plan`'s report, and never cut:

```text
.github/workflows/**   .github/actions/**   .github/dependabot.yml   .github/skills/**
.gitlab-ci.yml         .gitlab/**/*.yml     .gitea/**                .forgejo/**
.claude/settings*.json .claude/hooks/**     .claude/agents/**        .claude/skills/**
.claude/commands/**    .agents/skills/**    .mcp.json                **/CODEOWNERS
.gitattributes         renovate.json*       .pre-commit-config.yaml  .devcontainer/**
.vscode/tasks.json     any executable file
```

Add your own with `sensitive_paths` in `hub.yml`:

```yaml
sensitive_paths:
  - deploy/**
```

Agent skills are on the list because a skill's `allowed-tools` lets the agent use those
tools without asking, and a `hooks` block in its front matter registers hooks.

## Rename, retire, drop

- **Rename** a pack: move the directory and list the old name under `formerly`, or its
  files look `local` everywhere.
- **Retire a file**: delete it from the pack. Targets that never changed it get a pull
  request deleting it.
- **Stop assigning a pack**: its files stay in the targets, listed as `orphaned`.

See [Packs and opt-in](../concepts/packs.md) and
[Ownership by provenance](../concepts/ownership.md).
