# Stories

One file per unit of work, so anything can be picked up or handed over without
re-deriving the context. Empty right now: everything opened so far has shipped.

## Convention

- File name: `NNN-short-slug.md`, `NNN` never reused. Next free number: **015**.
- Front matter at the top of every file, `status` first.
- `status` is one of:

| status | meaning |
|---|---|
| `todo` | agreed, not started |
| `in-progress` | someone is on it right now |
| `review` | code written, awaiting verification |
| `done` | merged and verified |
| `blocked` | cannot proceed, `blocked-by` says why |
| `dropped` | deliberately not doing it, with the reason |

Keep **Context**, **Acceptance** and **Notes** sections. Notes is where a
half-finished attempt records what it learned, so the next person does not
repeat it.

Close a story by deleting it once it has shipped. Anything worth remembering
belongs in a comment beside the code it explains, not in a file nobody will open
again — which is where the reasoning from the first fourteen stories now lives.

```bash
grep -h '^status:' docs/stories/*.md | sort | uniq -c
```
