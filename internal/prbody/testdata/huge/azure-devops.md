Acme keeps its shared engineering files (agent instructions, editor and CI settings) in one hub.
Review this like any other change; questions go to the platform team's channel.

touchmark syncs packs `agents`, `claude` and `base` from the hub `acme-eng` ([`github.com/acme/engineering-assets`](https://redirect.github.com/acme/engineering-assets)) at commit [`3f2c1ab9d8e7`](https://redirect.github.com/acme/engineering-assets/commit/3f2c1ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3).

### ⚠ Sensitive paths

These files can run code in CI or change how tools and agents work in this repository. Review them first.

- `.github/workflows/docs.yml`: update, pack `ci`
- `.pre-commit-config.yaml`: create, pack `base`
- `bin/setup`: create, executable, pack `base`

### Changes

1003 changes, too many to list here.

<details>
<summary>150 files here differ from the hub and stay as they are</summary>

touchmark does not update files changed in this repository. To take the hub's version of one, run `touchmark apply --adopt <path>`. To stop seeing it here, add it to `ignore` in `.engineering-assets.yml`.

- `docs/handbook/chapter-00/local-000.md`
- `docs/handbook/chapter-00/local-012.md`
- `docs/handbook/chapter-00/local-024.md`
- `docs/handbook/chapter-00/local-036.md`
- `docs/handbook/chapter-00/local-048.md`
- `docs/handbook/chapter-00/local-060.md`
- `docs/handbook/chapter-00/local-072.md`
- `docs/handbook/chapter-00/local-084.md`
- `docs/handbook/chapter-00/local-096.md`
- `docs/handbook/chapter-00/local-108.md`
- `docs/handbook/chapter-00/local-120.md`
- `docs/handbook/chapter-00/local-132.md`
- `docs/handbook/chapter-00/local-144.md`
- `docs/handbook/chapter-01/local-001.md`
- `docs/handbook/chapter-01/local-013.md`
- `docs/handbook/chapter-01/local-025.md`
- `docs/handbook/chapter-01/local-037.md`
- `docs/handbook/chapter-01/local-049.md`
- `docs/handbook/chapter-01/local-061.md`
- `docs/handbook/chapter-01/local-073.md`
- `docs/handbook/chapter-01/local-085.md`
- `docs/handbook/chapter-01/local-097.md`
- `docs/handbook/chapter-01/local-109.md`
- `docs/handbook/chapter-01/local-121.md`
- `docs/handbook/chapter-01/local-133.md`
- `docs/handbook/chapter-01/local-145.md`
- `docs/handbook/chapter-02/local-002.md`
- `docs/handbook/chapter-02/local-014.md`
- `docs/handbook/chapter-02/local-026.md`
- `docs/handbook/chapter-02/local-038.md`
- `docs/handbook/chapter-02/local-050.md`
- `docs/handbook/chapter-02/local-062.md`
- `docs/handbook/chapter-02/local-074.md`
- `docs/handbook/chapter-02/local-086.md`
- `docs/handbook/chapter-02/local-098.md`
- `docs/handbook/chapter-02/local-110.md`
- `docs/handbook/chapter-02/local-122.md`
- `docs/handbook/chapter-02/local-134.md`
- `docs/handbook/chapter-02/local-146.md`
- `docs/handbook/chapter-03/local-003.md`
- `docs/handbook/chapter-03/local-015.md`
- `docs/handbook/chapter-03/local-027.md`
- `docs/handbook/chapter-03/local-039.md`
- `docs/handbook/chapter-03/local-051.md`
- `docs/handbook/chapter-03/local-063.md`
- `docs/handbook/chapter-03/local-075.md`
- `docs/handbook/chapter-03/local-087.md`
- `docs/handbook/chapter-03/local-099.md`
- `docs/handbook/chapter-03/local-111.md`
- `docs/handbook/chapter-03/local-123.md`
- `docs/handbook/chapter-03/local-135.md`
- `docs/handbook/chapter-03/local-147.md`
- `docs/handbook/chapter-04/local-004.md`
- `docs/handbook/chapter-04/local-016.md`
- `docs/handbook/chapter-04/local-028.md`

…and 95 more.

</details>

---

If this pull request is closed without merging, touchmark remembers it and does not propose the same changes again; a new one comes when the hub changes these files. Editing `packs` or `ignore` in `.engineering-assets.yml` resets that memory, and `ignore` opts a file out for good. To have the same changes proposed again sooner, the hub's maintainers can add a `forget_declines` entry with the number of this pull request to `.touchmark/operations.yml`. touchmark rewrites this description, so please comment instead of editing it.

<!-- touchmark:v1 hub=acme-eng fp=b7461799636b11ec stream=sync key=sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58 data=H4sIAAAAAAAC/5SQ4YrCMBCEX0Xmd/Rsq1bzKiJls9m0wTQpTRTkuHc/So87/96/nWGG+dhPPKErhVxmoREa+RUZCsPDQIN4lK3EHgpugkbvy/AwO07jR1vVzeF4as9QsMLeiu2oQKNxNVdkLvYsrTvR0Ry4sbVUbk8Xc+bWnuToDtRAgVMsEkvHaRz9P7uGskADCmkqPq6nxN7Hxd7v6t0eChPxPUNfQb3EknFT4IFiL4v5JxaCKUgRaEchi0LxJUiXZaHiIc2iN8tnNuuCzD72G8pZSl5Ykn2tAIGMhLz2rngLb3/CNwXi++/MLM90F/umeRYq0rk0Q8dHCF/fAQAA//9AxDKXogEAAA== -->