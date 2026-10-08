Acme keeps its shared engineering files (agent instructions, editor and CI settings) in one hub.
Review this like any other change; questions go to the platform team's channel.

touchmark syncs packs `agents`, `claude` and `base` from the hub `acme-eng` ([`github.com/acme/engineering-assets`](https://redirect.github.com/acme/engineering-assets)) at commit [`3f2c1ab9d8e7`](https://redirect.github.com/acme/engineering-assets/commit/3f2c1ab9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f4a3).

### ⚠ Sensitive paths

These files can run code in CI or change how tools and agents work in this repository. Review them first.

- `.github/workflows/docs.yml`: update, pack `ci`
- `.pre-commit-config.yaml`: create, pack `base`
- `bin/setup`: create, executable, pack `base`

### Changes

| Change | File | Pack |
|---|---|---|
| ⚠ update | `.github/workflows/docs.yml` | `ci` |
| ⚠ create | `.pre-commit-config.yaml` | `base` |
| ⚠ create, executable | `bin/setup` | `base` |
| delete | `docs/handbook/chapter-02/page-002.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-014.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-026.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-038.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-050.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-062.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-074.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-086.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-098.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-110.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-122.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-134.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-146.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-158.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-170.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-182.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-194.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-206.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-218.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-230.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-242.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-254.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-266.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-278.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-290.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-302.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-314.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-326.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-338.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-350.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-362.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-374.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-386.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-398.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-410.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-422.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-434.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-446.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-458.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-470.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-482.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-494.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-506.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-518.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-530.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-542.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-554.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-566.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-578.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-590.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-602.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-614.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-626.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-638.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-650.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-662.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-674.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-686.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-698.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-710.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-722.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-734.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-746.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-758.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-770.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-782.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-794.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-806.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-818.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-830.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-842.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-854.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-866.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-878.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-890.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-902.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-914.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-926.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-938.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-950.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-962.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-974.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-986.md` | `handbook` |
| delete | `docs/handbook/chapter-02/page-998.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-006.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-018.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-030.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-042.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-054.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-066.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-078.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-090.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-102.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-114.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-126.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-138.md` | `handbook` |
| delete | `docs/handbook/chapter-06/page-150.md` | `handbook` |

…and 903 more.

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
- `docs/handbook/chapter-04/local-040.md`
- `docs/handbook/chapter-04/local-052.md`
- `docs/handbook/chapter-04/local-064.md`
- `docs/handbook/chapter-04/local-076.md`
- `docs/handbook/chapter-04/local-088.md`
- `docs/handbook/chapter-04/local-100.md`
- `docs/handbook/chapter-04/local-112.md`
- `docs/handbook/chapter-04/local-124.md`
- `docs/handbook/chapter-04/local-136.md`
- `docs/handbook/chapter-04/local-148.md`
- `docs/handbook/chapter-05/local-005.md`
- `docs/handbook/chapter-05/local-017.md`
- `docs/handbook/chapter-05/local-029.md`
- `docs/handbook/chapter-05/local-041.md`
- `docs/handbook/chapter-05/local-053.md`
- `docs/handbook/chapter-05/local-065.md`
- `docs/handbook/chapter-05/local-077.md`
- `docs/handbook/chapter-05/local-089.md`
- `docs/handbook/chapter-05/local-101.md`
- `docs/handbook/chapter-05/local-113.md`
- `docs/handbook/chapter-05/local-125.md`
- `docs/handbook/chapter-05/local-137.md`
- `docs/handbook/chapter-05/local-149.md`
- `docs/handbook/chapter-06/local-006.md`
- `docs/handbook/chapter-06/local-018.md`
- `docs/handbook/chapter-06/local-030.md`
- `docs/handbook/chapter-06/local-042.md`
- `docs/handbook/chapter-06/local-054.md`
- `docs/handbook/chapter-06/local-066.md`
- `docs/handbook/chapter-06/local-078.md`
- `docs/handbook/chapter-06/local-090.md`
- `docs/handbook/chapter-06/local-102.md`
- `docs/handbook/chapter-06/local-114.md`
- `docs/handbook/chapter-06/local-126.md`
- `docs/handbook/chapter-06/local-138.md`
- `docs/handbook/chapter-07/local-007.md`
- `docs/handbook/chapter-07/local-019.md`
- `docs/handbook/chapter-07/local-031.md`
- `docs/handbook/chapter-07/local-043.md`
- `docs/handbook/chapter-07/local-055.md`
- `docs/handbook/chapter-07/local-067.md`
- `docs/handbook/chapter-07/local-079.md`
- `docs/handbook/chapter-07/local-091.md`
- `docs/handbook/chapter-07/local-103.md`
- `docs/handbook/chapter-07/local-115.md`

…and 50 more.

</details>

---

If this pull request is closed without merging, touchmark remembers it and does not propose the same changes again; a new one comes when the hub changes these files. Editing `packs` or `ignore` in `.engineering-assets.yml` resets that memory, and `ignore` opts a file out for good. touchmark rewrites this description, so please comment instead of editing it.

[touchmark]: # "touchmark:v1 hub=acme-eng fp=b7461799636b11ec stream=sync key=sha256:6b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b586b1f0c3a9e2d4b58 data=H4sIAAAAAAAC/5SQ4YrCMBCEX0Xmd/Rsq1bzKiJls9m0wTQpTRTkuHc/So87/96/nWGG+dhPPKErhVxmoREa+RUZCsPDQIN4lK3EHgpugkbvy/AwO07jR1vVzeF4as9QsMLeiu2oQKNxNVdkLvYsrTvR0Ry4sbVUbk8Xc+bWnuToDtRAgVMsEkvHaRz9P7uGskADCmkqPq6nxN7Hxd7v6t0eChPxPUNfQb3EknFT4IFiL4v5JxaCKUgRaEchi0LxJUiXZaHiIc2iN8tnNuuCzD72G8pZSl5Ykn2tAIGMhLz2rngLb3/CNwXi++/MLM90F/umeRYq0rk0Q8dHCF/fAQAA//9AxDKXogEAAA=="