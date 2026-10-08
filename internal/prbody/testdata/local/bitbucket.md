Acme keeps its shared engineering files (agent instructions, editor and CI settings) in one hub.
Review this like any other change; questions go to the platform team's channel.

touchmark syncs packs `agents`, `claude` and `base` from the hub `acme-eng` at commit `3f2c1ab9d8e7`.

### Changes

| Change | File | Pack |
|---|---|---|
| update | `AGENTS.md` | `agents` |

<details>
<summary>3 files here differ from the hub and stay as they are</summary>

touchmark does not update files changed in this repository. To take the hub's version of one, run `touchmark apply --adopt <path>`. To stop seeing it here, add it to `ignore` in `.engineering-assets.yml`.

- `.github/CODEOWNERS`
- `CONTRIBUTING.md`
- `docs/guide.md`

</details>

---

If this pull request is closed without merging, touchmark remembers it and does not propose the same changes again; a new one comes when the hub changes these files. Editing `packs` or `ignore` in `.engineering-assets.yml` resets that memory, and `ignore` opts a file out for good. To have the same changes proposed again sooner, the hub's maintainers can add a `forget_declines` entry with the number of this pull request to `.touchmark/operations.yml`. touchmark rewrites this description, so please comment instead of editing it.

[touchmark]: # "touchmark:v1 hub=acme-eng fp=b7461799636b11ec stream=sync key=sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58 data=H4sIAAAAAAAC/5SQ4YrCMBCEX0Xmd/Rsq1bzKiJls9m0wTQpTRTkuHc/So87/96/nWGG+dhPPKErhVxmoREa+RUZCsPDQIN4lK3EHgpugkbvy/AwO07jR1vVzeF4as9QsMLeiu2oQKNxNVdkLvYsrTvR0Ry4sbVUbk8Xc+bWnuToDtRAgVMsEkvHaRz9P7uGskADCmkqPq6nxN7Hxd7v6t0eChPxPUNfQb3EknFT4IFiL4v5JxaCKUgRaEchi0LxJUiXZaHiIc2iN8tnNuuCzD72G8pZSl5Ykn2tAIGMhLz2rngLb3/CNwXi++/MLM90F/umeRYq0rk0Q8dHCF/fAQAA//9AxDKXogEAAA=="