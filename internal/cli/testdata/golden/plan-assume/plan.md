### touchmark plan

Hub `acme-eng` (`github.com/712345678`) at `$COMMIT` · touchmark dev

| Provider | Host | Read | Write | Resolve |
|---|---|---|---|---|
| `gh` | `github.com` | `acme-read[bot]` | `acme-write[bot]`: not checked | resolve complete |
| `corp` | `gitlab.example.com` | `tm-reader` | `tm-writer`: not checked | resolve complete |

**\-\-assume\-opt\-in:** every target counts as opted in, for this report only; 1 target without an opt\-in file is planned as if it had an empty one

| Outcome | Targets |
|---|---:|
| open | 1 |
| unchanged | 1 |

<details><summary>open (1)</summary>

- `gh:acme/web`

</details>

<details><summary>unchanged (1)</summary>

- `gh:acme/api` [\#1](https://github.com/acme/api/pull/1)

</details>

#### Warnings

- hub head not checked\: a local run cannot read the tip of the hub's default branch

**Cost:** gh ≈ 4 writes \(&lt;1 min\) · corp ≈ 0 writes \(&lt;1 min\) · 1 run
