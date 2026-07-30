# Corpus provenance

The evaluation corpus is a fixed subset of the Kubernetes documentation, vendored into this repo
rather than fetched at run time.

## Why it is committed

Retrieval metrics are only comparable against a fixed corpus. If the documents changed between the
dense-only baseline and a later hybrid-search run, the difference in recall@k would be
uninterpretable: you could not tell whether retrieval improved or the corpus moved underneath it.
Committing the corpus and pinning the upstream commit is what makes the before and after numbers
mean something.

## Source

| | |
|---|---|
| Repository | [kubernetes/website](https://github.com/kubernetes/website) |
| Commit | `e95679cfa58a843e90bf8575d8b0db548dae452b` |
| Retrieved | 2026-07-30 |
| Path | `content/en/docs/concepts/` |
| Sections | `architecture`, `containers`, `services-networking`, `storage` |
| Result | 45 documents, ~499 KB |

## License and attribution

The Kubernetes documentation is licensed under
[Creative Commons Attribution 4.0 International](https://creativecommons.org/licenses/by/4.0/).
The full license text is in `LICENSE` in this directory.

> Portions of this corpus are adapted from the Kubernetes documentation, copyright The Kubernetes
> Authors, licensed under CC BY 4.0. The text has been modified: Hugo template syntax was removed
> and YAML front matter was replaced with a markdown heading. See `prepare.sh` for the exact
> transformation.

CC BY 4.0 permits redistribution and modification provided attribution is given and changes are
indicated. Both conditions are met by this file.

## Why this subset

Two constraints drove the selection.

**Document count sets the floor of the metric.** Golden questions are labelled with the document
that answers them, so with only a handful of documents a top-5 retrieval would contain the right
one by chance. At 45 documents, a random retriever scores roughly 11% recall@5, which leaves real
room for the number to move.

**Structure has to exist for the chunking comparison to be meaningful.** The structure-aware chunker
splits on markdown headings, so it can only outperform fixed-size chunking on documents that
actually have them. This subset averages roughly 12 headings per document.

The full `concepts` tree is about 2.2 MB across 176 files, which is more than the experiment needs
and slower to re-embed on every configuration change.

## Reproducing

```bash
git clone --filter=blob:none --sparse --depth 1 https://github.com/kubernetes/website.git
cd website && git sparse-checkout set content/en/docs/concepts
cd - && ./eval/corpus/prepare.sh ./website
```

Note that a fresh clone tracks upstream `main`, so reproducing exactly requires checking out the
commit pinned above. `prepare.sh` is committed for auditability; it is not part of the eval run.
