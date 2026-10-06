# Security Policy

## Supported versions

| Version | Supported |
|---------|-----------|
| the latest release | ✅ fixes ship as the next patch or minor |
| older | ❌ upgrade to the latest release first |

The binaries, the container image and the GitHub Action share one version number, so a fix
reaches all three in the same release.

## Reporting a vulnerability

**Please do not report security vulnerabilities via public GitHub Issues.**

Report it privately through GitHub, by
[opening a draft security advisory](https://github.com/bedrock-python/touchmark/security/advisories/new),
or send an email to **shalaevad.alexey@gmail.com**. Either way, include:

- Description of the vulnerability
- Steps to reproduce, ideally with a hub and targets anyone can recreate
- Potential impact and affected versions

We aim to acknowledge reports within **48 hours** and provide a fix within **7 days**
for critical issues.

Once the fix is released, we will credit you in the release notes unless you prefer
to remain anonymous.

## What we consider a vulnerability

touchmark holds write access to many repositories at once. Anything that breaks one of the
promises of the [security model](https://bedrock-python.github.io/touchmark/concepts/security/)
or a control of the [threat model](https://bedrock-python.github.io/touchmark/project/threat-model/)
is a vulnerability, for example:

- touchmark overwrites or deletes a file it cannot prove it shipped;
- a credential reaches a log, a report, a pull request, a commit, a branch name or git's
  command line or configuration;
- the write key becomes usable from a job that a branch other than the hub's default branch
  can start, without `doctor` or the isolation probe noticing;
- content from a target repository makes touchmark run a program, follow a symlink out of
  the repository, or write outside its working directories;
- touchmark changes, closes or reuses a pull request or branch that it did not create;
- a release archive, the image or the image the Action runs can be swapped without failing
  the verification below.

A hub configured with `security.write_isolation: none`, or a maintainer of a hub misusing
their own write key, is outside this scope: the hub's documentation describes those risks.

## Verifying what you run

Every release is built and published by this repository's `publish.yml` workflow on GitHub
Actions, which signs and attests what it publishes with its own identity, keyless:

- **the image** `ghcr.io/bedrock-python/touchmark` carries a cosign signature, an SBOM and a
  build provenance attestation, all bound to its digest;
- **the archives** are listed in `checksums.txt`, which is signed with cosign, and each has
  an SBOM and a build provenance attestation;
- **the GitHub Action** at tag `vX.Y.Z` resolves the image `X.Y.Z` to a digest, checks that
  digest's build provenance with `gh attestation verify`, and runs the image by that digest
  only. An image this workflow did not build fails the check, and the step with it.

The same checks by hand:

```bash
# the image: built by this repository's publish workflow
gh attestation verify oci://ghcr.io/bedrock-python/touchmark:X.Y.Z \
  --repo bedrock-python/touchmark \
  --signer-workflow bedrock-python/touchmark/.github/workflows/publish.yml

# an archive
gh attestation verify touchmark_X.Y.Z_linux_amd64.tar.gz --repo bedrock-python/touchmark
```

[Releases](https://bedrock-python.github.io/touchmark/project/release/) lists every
artifact, the cosign commands and what each check proves.
