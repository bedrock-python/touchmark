touchmark noted that this was closed without merging: it remembers these changes and will not propose them again.

- A new one comes when the hub changes these files.
- Editing `packs` or `ignore` in `.engineering-assets.yml`, or changing one of these files here, lifts this decline.
- To have the same changes proposed again, reopen this or tick **Propose this content again** in its description. The hub's maintainers can also add a `forget_declines` entry with `pr: 12` to `.touchmark/operations.yml`.

To keep your own versions of these files for good, add them to `ignore` in `.engineering-assets.yml`:

```yaml
ignore:
  - "[ ]lead.md"
  - "\x40team/notes.md"
  - "AGENTS.md"
  - "docs/[[]draft].md"
  - "prompts/review.md"
  - "quote\"back\\slash.md"
```