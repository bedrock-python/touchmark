# operations.yml

`.touchmark/operations.yml` in the hub: one-off operations that override a safeguard. It
goes through review like the packs, and only `distribute` on the default branch carries
it out. `touchmark schema operations` prints the JSON Schema.

```yaml
version: 1
recreate:                  # rebuild a paused sync branch; others' commits on it are dropped
  - target: gh:acme/api
    head: 4b1d9e0c8f5a3b2e1d0c9b8a7f6e5d4c3b2a1f0e   # acts while the branch head is this commit
forget_declines:           # propose again what a target declined
  - target: gh:acme/docs
    pr: 44                 # touchmark marks #44 revoked: acts once
allow_mass_close: { max: 400, until: 2026-12-31 }
adopt_unmarked: { until: 2026-12-31 }   # take over a multi-gitter setup's pull requests
```

A `target` is `[PROVIDER:]PATH`, the provider being the `id` of an entry of `providers` in
`hub.yml` (`gh` here). A hub with one provider, the single-provider shorthand included,
writes the path alone.

Every entry limits itself, so a forgotten one does nothing: a branch head that moved, a
decline already revoked, a date that passed. The exception is a `forget_declines` entry
for a Bitbucket Cloud, Bitbucket Data Center or Azure DevOps target: touchmark never edits a declined pull
request there, so it is never marked revoked, and the entry acts while it is present (see
[Memory of declined pull requests](../concepts/memory.md#on-bitbucket-cloud)). `check`
warns about entries past their date.
`plan` on a hub pull request shows each entry's effect: `applies`, `none`, `expired` or
`unknown`. In CI the matching flags of `distribute` exit 2. See
[One-off operations](../guide/operations.md).

## Keys

<!-- generated: schema operations -->

| Key | Type | Description |
|---|---|---|
| `version` | `1` | Format version. Only 1 exists; when absent, 1 is assumed with a warning. |
| `recreate` | list of objects | Rebuild a paused sync branch from scratch. Commits others added to it are dropped. An entry acts only while the branch head is the given commit. |
| `recreate[].target` | string, up to 512 characters, required | One target repository: &lt;provider&gt;:&lt;path&gt; or &lt;path&gt;, as touchmark prints it. |
| `recreate[].head` | string, required | Full commit id of the sync branch head to replace: 40 or 64 lowercase hex digits. |
| `forget_declines` | list of objects | Propose again the content of a pull request the target closed without merging. touchmark marks that pull request revoked, so the entry acts once; on Bitbucket Cloud, Bitbucket Data Center and Azure DevOps, where touchmark never edits a declined pull request, the entry acts while it is present. |
| `forget_declines[].target` | string, up to 512 characters, required | One target repository: &lt;provider&gt;:&lt;path&gt; or &lt;path&gt;, as touchmark prints it. |
| `forget_declines[].pr` | integer ≥ 1, required | Number of the declined pull request. |
| `allow_mass_close` | object | Lift the guard against closing many pull requests in one run, up to max closes, until the date. |
| `allow_mass_close.max` | integer ≥ 1, required | How many pull requests a run may close. |
| `allow_mass_close.until` | string, required | Last day the entry is active, YYYY-MM-DD, in UTC. |
| `adopt_unmarked` | object | Take over the open pull requests without a marker that a multi-gitter setup opened (from known_authors, on branch_aliases), until the date. |
| `adopt_unmarked.until` | string, required | Last day the entry is active, YYYY-MM-DD, in UTC. |

<!-- end generated -->
