Acme keeps its shared engineering files (agent instructions, editor and CI settings) in one hub.
Review this like any other change; questions go to the platform team's channel.

touchmark syncs packs `agents`, `claude` and `base` from the hub `acme-eng` ([`github.com/acme/engineering-assets`](https://redirect.github.com/acme/engineering-assets)) at commit [`3f2c1ab9d8e7`](https://redirect.github.com/acme/engineering-assets/commit/3f2c1ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3).

The hub subscribed this repository, which has no `.engineering-assets.yml`. To add packs or keep files out of the sync, add `.engineering-assets.yml` with `packs` or `ignore`; to stop these pull requests, add it with `enabled: false`.

### Changes

| Change | File | Pack |
|---|---|---|
| create | `AGENTS.md` | `agents` |
| create | `prompts/review.md` | `agents` |

---

If this pull request is closed without merging, touchmark remembers it and does not propose the same changes again; a new one comes when the hub changes these files. Editing `packs` or `ignore` in `.engineering-assets.yml` resets that memory, and `ignore` opts a file out for good. touchmark rewrites this description, so please comment instead of editing it.

<!-- touchmark:v1 hub=acme-eng fp=b7461799636b11ec stream=sync key=sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58 data=H4sIAAAAAAAC/5SQ4YrCMBCEX0Xmd/Rsq1bzKiJls9m0wTQpTRTkuHc/So87/96/nWGG+dhPPKErhVxmoREa+RUZCsPDQIN4lK3EHgpugkbvy/AwO07jR1vVzeF4as9QsMLeiu2oQKNxNVdkLvYsrTvR0Ry4sbVUbk8Xc+bWnuToDtRAgVMsEkvHaRz9P7uGskADCmkqPq6nxN7Hxd7v6t0eChPxPUNfQb3EknFT4IFiL4v5JxaCKUgRaEchi0LxJUiXZaHiIc2iN8tnNuuCzD72G8pZSl5Ykn2tAIGMhLz2rngLb3/CNwXi++/MLM90F/umeRYq0rk0Q8dHCF/fAQAA//9AxDKXogEAAA== -->