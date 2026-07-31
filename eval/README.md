# Retrieval evaluation harness

Measures how well `go-rag-api` retrieves the right document for a question, so that changes to
chunking and search can be compared against a baseline instead of assessed by eye.

**Status:** built, and the dense-only baseline has been taken. See [Baseline](#baseline) below.

## Layout

```
eval/
  corpus/            45 Kubernetes documentation files, vendored and pinned
    PROVENANCE.md    upstream commit, license, why this subset
    prepare.sh       the Hugo to markdown transformation, committed for audit
  cmd/load/          ingests the corpus into a running service
  cmd/run/           (planned) asks the golden questions, reports recall@k
  golden.json        35 questions labelled with the document that answers them
```

## The golden set

35 questions, each written after reading the source document and each answerable from exactly one
document in the corpus. Coverage is 32 of the 45 documents, spread across all four sections.

```json
{
  "id": "stor-09",
  "question": "Which kubelet metric reports that a volume is in an abnormal condition?",
  "expected_document_id": "storage/volume-health-monitoring",
  "type": "identifier",
  "notes": "kubelet_volume_stats_health_status_abnormal"
}
```

`notes` records where the answer lives so a label can be audited without re-reading the corpus.

`type` is the part that does real work. Questions are split into two kinds:

- **`identifier`** (17): the answer hinges on an exact rare token, such as `foregroundDeletion`,
  `node.k8s.io`, `ndots:5`, or `kubelet_volume_stats_health_status_abnormal`.
- **`conceptual`** (18): the answer is phrased in ordinary language with no distinctive token to
  match on, such as "are a pod's outbound connections restricted before any policy is applied."

This split exists because dense and sparse retrieval are expected to fail in opposite directions.
Embeddings are weakest on rare exact tokens, which is precisely where BM25 is strongest. Reporting
recall separately for the two types turns "hybrid search improved recall by four points" into a
statement with a mechanism behind it: if the gain lands entirely in the `identifier` bucket, the
explanation is confirmed, and if it lands in the `conceptual` bucket then something other than the
assumed reason is happening.

A single aggregate number could not tell those apart, and the split costs one field in a JSON file.

## Running it

```bash
docker compose up -d                 # local pgvector
go run ./cmd/server                  # the service, on :8080
go run ./eval/cmd/load               # ~4 minutes, 1,126 embedding calls
```

`load` truncates the `chunks` table before it starts. Pass `-reset=false` to append instead, though
there is rarely a good reason to.

## The three parts, and the contract between them

**The corpus** is fixed. 45 documents, committed to the repo rather than fetched, pinned to upstream
commit `e95679c`. A metric is only comparable against unchanged inputs: if the documents move
between two runs, the difference in recall tells you nothing, because you cannot separate "retrieval
improved" from "the corpus changed."

**The golden set** is about 30 questions, each labelled with the document ID that answers it. The
labels are written by hand after reading the source, which is why the corpus is small enough to
read.

**The metric** is recall@k. Of the questions asked, what fraction had the correct document somewhere
in the top k retrieved chunks.

The contract binding them is the document ID. `load` derives it from the file's path relative to the
corpus root, minus the extension, with forward slashes: `eval/corpus/architecture/cgroups.md`
becomes `architecture/cgroups`. `golden.json` labels against that exact string. It is a natural key,
derived deterministically from location, rather than a surrogate key generated at insert time,
because a generated ID would change on every reload and orphan every hand-written label.

## Design decisions

**Labels are document-level, not chunk-level.** The instinct is to label the specific chunk that
answers each question, which is more precise. Chunk IDs are a function of the chunking strategy, so
the moment you switch from fixed-size to structure-aware chunking every boundary moves and every
label becomes garbage. The golden set would break at exactly the experiment it exists to support.
Document IDs survive re-chunking. So do distinctive substrings, if more resolution is needed later.

**Retrieval is measured below HTTP.** `cmd/run` calls `store.Search` directly rather than posting to
`/query`. `/query` also generates an answer, and generation cannot affect recall@k. Leaving it in
the loop would put a nondeterministic, billed, network-dependent component inside a measurement it
provably cannot influence, so every surprising number would have an extra suspect in it. Nothing is
faked: it is the real Bedrock embedder and the real pgvector search. Only the measurement boundary
moved inward.

**Ingestion is measured through HTTP.** The asymmetry is deliberate. Ingestion runs once per
configuration rather than once per question, and everything it touches is under measurement, since
chunking decides what can be retrieved at all. There is no dead weight to route around, so it goes
through the real endpoint.

**Loading fails fast.** A partially loaded corpus still answers questions. It answers them against a
corpus nobody can describe, and every number measured afterward is quietly wrong. One failed
document stops the run.

**`Reset` is not on the `VectorStore` interface.** It lives on the concrete `*Postgres`, so the eval
harness can empty the corpus and the service, which only ever holds the interface, cannot. The
narrow interface is what makes "delete everything" unreachable from the request path.

## Baseline

Dense retrieval, fixed-size chunking (500 runes, 50 overlap), Titan v2 embeddings, 35 questions.
Measured 2026-07-30 against corpus `e95679c`. Corpus load took 2m50s for 1,126 chunks; the eval
itself runs in about 6 seconds.

| metric | population | n | recall@1 | recall@3 | recall@5 | recall@10 | recall@20 |
|---|---|---|---|---|---|---|---|
| document | overall | 35 | 85.7% | 97.1% | 97.1% | 97.1% | 100.0% |
| document | conceptual | 18 | 83.3% | 94.4% | 94.4% | 94.4% | 100.0% |
| document | identifier | 17 | 88.2% | 100.0% | 100.0% | 100.0% | 100.0% |
| passage | overall | 35 | 42.9% | 71.4% | 77.1% | 85.7% | 91.4% |
| passage | conceptual | 18 | 27.8% | 61.1% | 66.7% | 72.2% | 83.3% |
| passage | identifier | 17 | 58.8% | 82.4% | 88.2% | 100.0% | 100.0% |

**Document-level recall is saturated and should not be used to compare configurations.** At 97.1%
recall@5 against a 100% ceiling, the entire dynamic range is one question. 45 topically disjoint
Kubernetes documents make "which document is this about" too easy a task to measure anything.
Passage-level recall is the metric to track. Document-level is retained because it is what the
earlier runs reported and because the contrast between the two rows is the argument for the change.

**The two metrics are not comparable to each other.** They have different floors, 11.1% versus 0.4%,
and answer different questions. Each is only comparable to future runs of itself.

Two findings that shape what comes next:

- **Reranker headroom is 14.3 points at passage level** (91.4% at k=20 against 77.1% at k=5), versus
  2.9 points at document level. The document-level number would have ruled out a reranker entirely.
- **Dense retrieval is stronger on `identifier` questions than `conceptual` ones**, 88.2% against
  66.7% at k=5. That is the opposite of the prediction that motivated hybrid search. BM25 matches
  shared vocabulary, so its best case is the bucket that is already saturated and its worst case is
  the paraphrase bucket where the failures actually are. Caveat: the identifier questions contain
  their rare token verbatim, which is a confound built into the golden set and a candidate for
  revision.

Three questions never retrieve their answering passage even at k=20: `net-06`, `net-07`, `stor-06`.
Every label has been verified to exist intact within a single stored chunk, so these are genuine
retrieval failures rather than unachievable labels.

## Reading the results

`cmd/run` will search at k=20 and report recall@1, @3, @5, @10 and @20 from the same ranked list, at
no extra cost.

The full curve matters more than any single value:

- **A random retriever scores 11.1% recall@5** on 45 documents, since 5/45 is the chance of the
  right document appearing by luck. A baseline near that number means the harness is broken, not
  that retrieval is bad. This is the first sanity check to run.
- **`recall@20 - recall@5` is the headroom available to a reranker.** A reranker can only promote
  documents the retriever already surfaced, so that gap is the absolute ceiling on what one could
  buy. If the gap is small, a reranker is not the next thing to build.
- **If recall@20 is also low**, the correct documents are not being found at all. That is a
  retrieval problem rather than a ranking problem, and it points at hybrid search rather than a
  reranker.

## What this does not measure

- **Answer quality.** Recall@5 says the right document was retrieved. It says nothing about whether
  the model then used it, ignored it, or contradicted it. Faithfulness and answer correctness are
  separate metrics needing a different harness.
- **Passage precision.** Document-level labels cannot distinguish a retriever that returns the exact
  answering paragraph from one that returns four mediocre chunks out of the right file. This is a
  known blind spot, and it happens to be blind to exactly what a reranker improves.
- **The deployed service.** Measuring below HTTP means a route typo, a handler dropping a field, or
  a wrong `topK` in `main` would not show up here. One smoke request against `/query` covers that,
  and it is a different test.
- **Scale.** 45 documents and ~1,126 chunks is laptop-sized. pgvector does a sequential scan at this
  size and is fast enough that no index is warranted, so nothing here says anything about behaviour
  at ten million chunks.

## Sample size

About 30 questions means one question flipping is 3.3 percentage points. A four point difference
between two configurations is one or two questions and should be read as noise, not a finding. This
is the main limitation of the whole setup and it is a labelling budget problem, not a design flaw
that better code would fix.
