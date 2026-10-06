A bot closed this without merging for the third time in a row, so touchmark now remembers it as declined, like a person's decision. To keep a stale bot from closing touchmark's changes, exempt the labels `engineering-assets` and `sync` in its settings.

touchmark noted that this was closed without merging: it remembers these changes and will not propose them again.

- A new one comes when the hub changes these files.
- Editing `packs` or `ignore` in `.engineering-assets.yml`, or changing one of these files here, lifts this decline.
- To have the same changes proposed again, reopen this or tick **Propose this content again** in its description. The hub's maintainers can also add a `forget_declines` entry with `pr: 19` to `.touchmark/operations.yml`.

To keep your own versions of these files for good, add them to `ignore` in `.engineering-assets.yml`:

```yaml
ignore:
  - "AGENTS.md"
  - "prompts/review.md"
```