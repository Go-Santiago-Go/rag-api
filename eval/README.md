# Retrieval evaluation harness

Measures how well `go-rag-api` retrieves the right document for a question, so that changes to
chunking and search can be compared against a baseline instead of assessed by eye.

**Status:** built. Three changes measured against it so far: cross-encoder reranking, question
phrasing, and chunking strategy. Two were adopted, one finding was retracted. See
[Results](#results).

Answer quality is measured separately by [`cmd/judge`](#answer-quality), because recall proves the
right passage came back and says nothing about what the model did with it.

## Layout

```
eval/
  corpus/            45 Kubernetes documentation files, vendored and pinned
    PROVENANCE.md    upstream commit, license, why this subset
    prepare.sh       the Hugo to markdown transformation, committed for audit
  cmd/load/          ingests the corpus into a running service
  cmd/run/           asks the golden questions, reports recall@k
  cmd/judge/         runs the real query pipeline, grades the answers
  internal/golden/   the label rules, shared so run and judge cannot disagree
  internal/judge/    the grading rubric and reply parser
  golden.json        35 questions labelled with the passage that answers them
```

## The golden set

35 questions, each written after reading the source document and each answerable from exactly one
document in the corpus. Coverage is 32 of the 45 documents, spread across all four sections.

```json
{
  "id": "stor-09",
  "question": "Which kubelet metric reports that a volume is in an abnormal condition?",
  "paraphrase": "How would monitoring tell me that a mounted disk underneath a workload has developed a problem?",
  "expected_document_id": "storage/volume-health-monitoring",
  "expected_substring": "kubelet_volume_stats_health_status_abnormal",
  "type": "identifier",
  "notes": "kubelet_volume_stats_health_status_abnormal"
}
```

`expected_substring` is verbatim text from the answering passage, and it is what makes the metric
passage-level rather than document-level. `notes` records where the answer lives so a label can be
audited without re-reading the corpus.

`paraphrase` asks the same question without naming the rare token the label matches on, and is set
only on identifier questions. It exists because the first baseline reported identifier questions
outscoring conceptual ones, which turned out to be an artifact: ten of the seventeen contained their
own answer's distinctive token, so the run was partly measuring string overlap. Running both phrasings
separates the two. See [Phrasing](#phrasing) below.

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
go run ./eval/cmd/load               # ~3 minutes, 1,126 embedding calls

go run ./eval/cmd/run                # the baseline: dense retrieval, original phrasing
go run ./eval/cmd/run -rerank        # add the cross-encoder second stage
go run ./eval/cmd/run -paraphrase    # ask identifier questions without their rare token
go run ./eval/cmd/run -verbose       # per-question ranks, for diagnosing a single failure

go run ./eval/cmd/judge -validate -n 5   # prove the judge discriminates, ~45s. Run this first.
go run ./eval/cmd/judge                  # grade all 35 answers, ~2m30s
go run ./eval/cmd/judge -paraphrase      # the harder phrasing
```

`load` truncates the `chunks` table before it starts. Pass `-reset=false` to append instead, though
there is rarely a good reason to. `run` takes about 6 seconds dense and 12 with reranking, and the
flags compose, so `-rerank -paraphrase` is the hardest of the four configurations.

`judge` is far slower than `run` because it generates a real answer and then grades it, two model
calls per question against `run`'s one embedding call. It needs no running server: like `run` it
builds the pipeline in process, so `cmd/server` does not have to be up.

Comparing chunking strategies means re-ingesting the corpus under each one, which is a server restart
rather than a flag:

```bash
CHUNK_STRATEGY=fixed CHUNK_SIZE=500 CHUNK_OVERLAP=50 go run ./cmd/server
go run ./eval/cmd/load && go run ./eval/cmd/run -rerank -paraphrase
```

**Restart the server by building the binary, not with `go run`.** `go run` compiles to a temporary
binary and runs it as a child process, so killing the `go run` parent orphans a child still holding
:8080. The next configuration then fails to bind, exits, and the eval silently measures against the
previous server. This cost a full sweep: three configurations returned byte-identical numbers and
identical chunk counts, which is the only reason it was caught. A readiness probe against `/health`
does not help, because it tests liveness and any server satisfies it. Check identity instead: the
`chunking strategy=...` line the server logs at startup.

## The three parts, and the contract between them

**The corpus** is fixed. 45 documents, committed to the repo rather than fetched, pinned to upstream
commit `e95679c`. A metric is only comparable against unchanged inputs: if the documents move
between two runs, the difference in recall tells you nothing, because you cannot separate "retrieval
improved" from "the corpus changed."

**The golden set** is 35 questions, each labelled with the document ID that answers it and a verbatim
substring from the answering passage. The labels are written by hand after reading the source, which
is why the corpus is small enough to read.

**The metric** is recall@k. Of the questions asked, what fraction had the answer somewhere in the top
k retrieved chunks. It is computed at two granularities: a document hit needs the right file, a
passage hit needs the right file *and* the labelled substring inside that chunk.

The contract binding them is the document ID. `load` derives it from the file's path relative to the
corpus root, minus the extension, with forward slashes: `eval/corpus/architecture/cgroups.md`
becomes `architecture/cgroups`. `golden.json` labels against that exact string. It is a natural key,
derived deterministically from location, rather than a surrogate key generated at insert time,
because a generated ID would change on every reload and orphan every hand-written label.

## Design decisions

**Labels are substrings, not chunk IDs.** The instinct is to label the specific chunk that answers
each question, which is the most precise option available. Chunk IDs are a function of the chunking
strategy, so the moment you switch from fixed-size to structure-aware chunking every boundary moves
and every label becomes garbage. The golden set would break at exactly the experiment it exists to
support.

Document IDs and verbatim substrings both survive re-chunking, which is why the labels are one of
each. The first version used document IDs alone and saturated at 97.1%; the substring is what restored
enough dynamic range for the metric to be informative. Neither identifies a chunk, so both keep
working when the chunker changes.

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

## Results

Titan v2 embeddings, 35 questions, corpus `e95679c`. Measured 2026-07-30 (dense baseline) and
2026-07-31 (reranking, then chunking).

The tables immediately below hold chunking fixed at the original 500 runes so that reranking is the
only variable. [Chunking](#chunking) then varies chunking with retrieval held fixed. The service
default has since moved to structure-aware chunking at 800 runes, so these are the historical
baseline rather than current production numbers.

Passage-level recall, the metric to track:

| phrasing | retrieval | recall@1 | recall@3 | recall@5 | recall@10 | recall@20 |
|---|---|---|---|---|---|---|
| original | dense | 42.9% | 71.4% | 77.1% | 85.7% | 91.4% |
| original | dense+rerank | 71.4% | 77.1% | 82.9% | 91.4% | 91.4% |
| paraphrased | dense | 22.9% | 48.6% | 57.1% | 68.6% | 74.3% |
| paraphrased | dense+rerank | 42.9% | 57.1% | 62.9% | 74.3% | 74.3% |

Document-level recall, retained for contrast but saturated:

| phrasing | retrieval | recall@1 | recall@3 | recall@5 | recall@10 | recall@20 |
|---|---|---|---|---|---|---|
| original | dense | 85.7% | 97.1% | 97.1% | 97.1% | 100.0% |
| original | dense+rerank | 97.1% | 100.0% | 100.0% | 100.0% | 100.0% |
| paraphrased | dense | 68.6% | 82.9% | 85.7% | 91.4% | 94.3% |
| paraphrased | dense+rerank | 85.7% | 88.6% | 88.6% | 94.3% | 94.3% |

**Document-level recall is saturated and should not be used to compare configurations.** At 97.1%
recall@5 against a 100% ceiling, the entire dynamic range is one question. 45 topically disjoint
Kubernetes documents make "which document is this about" too easy a task to measure anything.
Document-level is retained because it is what the first run reported and because the contrast between
the two tables is the argument for the change.

**The two metrics are not comparable to each other.** They have different floors, 11.1% versus 0.4%,
and answer different questions. Each is only comparable to future runs of itself.

**recall@20 is unchanged by reranking, and that is a correctness check rather than a disappointment.**
`cmd/run` reranks all 20 candidates and keeps all 20, so the set is identical and only its order
moves. Every difference at a shallower cutoff is therefore attributable to reordering alone. A shifted
recall@20 would mean the harness is wrong.

### What reranking bought

+5.8 points at k=5 in both phrasings, and +20 to +28.5 points at k=1. The gain concentrating at rank 1
is the expected signature: a reranker cannot find anything new, it can only put the right thing first.

The k=5 number is two questions out of 35, which by the [sample size](#sample-size) caveat below is
not by itself a result. The k=1 number is seven questions, which is.

It is also not uniformly positive. On the paraphrased run 12 questions improved and 4 got worse; at
the k=5 boundary specifically, 4 crossed in and 2 crossed out. `stor-08` went from rank 15 to rank 1,
and `cont-03` went from rank 1 to rank 7. A cross-encoder is a second opinion, not an oracle.

**The result that justifies the cost is not recall, it is prompt size.** Reranked recall@3 equals dense
recall@5 (77.1%). The same recall is available from 3 chunks instead of 5, a 40% cut in generation
input tokens, which is the expensive part of a request. Reranking adds roughly 170ms per query.

### Phrasing

The paraphrased run exists because the first baseline produced a finding that did not survive
scrutiny: identifier questions scored 88.2% at passage k=5 against conceptual's 66.7%, which was read
as dense retrieval handling rare tokens well. Ten of the seventeen identifier questions contained
their answer's distinctive token verbatim, so the run was partly measuring string overlap.

Asking the same 17 questions without the token drops identifier recall@5 from 88.2% to 47.1%, a 41
point collapse. Conceptual rows are byte-identical across the two phrasings, which is the control
working: the only thing that moved is the thing that changed.

**So the original finding is retracted.** Identifiers are not a strength; they went from 21 points
ahead of conceptual to 20 points behind.

**The conclusion it supported survives on different reasoning.** BM25 matches rare terms shared
between query and document, so it can only help when the query contains the rare token. That is the
88.2% case, where dense retrieval is already near saturated. In the 47.1% case, where the failures
live, the query contains no rare token either, so BM25 has nothing to match on. Lexical retrieval is
strongest exactly where it is least needed. This is why hybrid search sits behind reranking in
priority.

**Both phrasings overshoot, in opposite directions.** The original names the label token, which real
users often would not. The paraphrase strips not just the label token but also ordinary domain
vocabulary: `cont-06` and `stor-04` now miss at the document level because the questions avoid
"kubelet" and "snapshot", words any real user would say. Treat 88.2% as a ceiling and 47.1% as a
floor. The direction of every conclusion above holds at both ends.

### Chunking

Retrieval is held at dense+rerank while chunking varies. Each configuration is a full corpus
re-ingest, driven through the real `/ingest` endpoint with `CHUNK_STRATEGY`, `CHUNK_SIZE` and
`CHUNK_OVERLAP`.

The investigation started from a defect visible in the stored data rather than from a hypothesis. Of
the 1,126 chunks the original configuration produced, 1,040 (92%) had at least one severed edge and
786 (70%) began with a lowercase letter, meaning mid-sentence. The chunk holding the answer to
`net-06` began `"ons may be established"` and ended `"all outbound conne"`: a fragment bounded by two
broken words, which is what the embedder saw, and what a caller would have been shown as a citation.

Section sizes in the corpus set the ceiling. 563 markdown sections, median 631 runes, p99 3,669. At a
500-rune ceiling 61% of sections would overflow into the fixed-size fallback, so the strategy would
mostly degenerate into what it was replacing. At 800 only 40% overflow.

| config | chunks | mean body runes | body starts mid-sentence |
|---|---|---|---|
| fixed-500 | 1,126 | ~490 | 69.8% |
| fixed-800 | 709 | 778 | 66.1% |
| structured-800 | 942 | 517 | **4.8%** |

The measure is the chunk *body*, with any heading line stripped. Measuring the whole chunk reports 0%
for the structured strategy and is worthless, because every structured chunk begins with `###`. The
heading masks whether the prose beneath it starts mid-word, which is the thing that matters. This
error was caught by reading an actual `/query` response, not by the harness.

Passage recall@5:

| config | dense, original | dense, paraphrased | dense+rerank, paraphrased |
|---|---|---|---|
| fixed-500 | 77.1% | 57.1% | 62.9% |
| fixed-800 | 82.9% | **65.7%** | 74.3% |
| structured-800 | 82.9% | 60.0% | **77.1%** |

**fixed-800 exists to make the comparison interpretable.** Structure-aware chunking at 800 against
fixed chunking at 500 varies two things at once. Without the middle row a win could not be attributed
to either.

**Chunk size is the dominant variable.** 500 to 800 runes is +8.6 points dense and +11.4 reranked,
3 to 4 questions, outside the noise floor. The largest measured gain in the project came from changing
a constant.

**That gain must be discounted, though not dismissed.** 800-rune chunks mean 709 chunks rather than
1,126, and at a fixed k fewer, larger chunks are each likelier to contain the label. Part of the
improvement is the ruler.

**Structure-awareness did not move recall.** Against fixed-800 at a matched ceiling it ties on the
original phrasing (82.9%), is worse dense on the paraphrased set (65.7% to 60.0%, two questions) and
better reranked (74.3% to 77.1%, one question). Every one of those deltas is at or inside the noise
floor.

**And the matched-ceiling comparison is not clean either.** fixed-800 and structured-800 share a
ceiling but realize different mean sizes, 778 against 624, because a structure-aware chunker decides
its own size distribution. Strategy and size cannot be fully separated, because the strategy is partly
a size decision.

So the honest result is that the mechanism worked and the outcome did not follow. Severed boundaries
went from 66% to 4.8%, and recall barely moved.

**Structure-aware chunking is nevertheless the default**, on a ground this harness does not measure.
`sources[]` is the service's contract, and chunks are returned to callers verbatim. Under fixed-size
chunking a citation can literally be `"ons may be established ... all outbound conne"`. Under
structure-aware chunking a citation is a heading plus whole paragraphs. That is a user-facing defect
fixed at recall parity, and recall parity is the whole of the claim being made for it.

Two residual defects, both from the same cause. A single paragraph larger than the ceiling has no
internal boundary to cut on, so it falls through to the rune walk: that is the remaining 4.8% of
mid-word starts, and it also leaves 10 chunks (1.1%) holding an unbalanced code fence, from code
blocks over 800 runes. The fix would be to let an atomic block exceed the ceiling, which trades a
measured invariant for a cosmetic one and has not been made.

### Known failures

Under the current default (structured-800) with `-paraphrase -rerank`, five questions never retrieve
their answering passage even at k=20: `cont-04`, `cont-06`, `net-02`, `net-03`, `net-08`.

Under the original fixed-500 configuration the list was nine: `arch-01`, `cont-04`, `cont-06`,
`net-01`, `net-02`, `net-06`, `net-07`, `stor-04`, `stor-06`. Six of those became reachable and two
new ones (`net-03`, `net-08`) broke, for a net gain of four questions at k=20. This is the one place
chunking clearly helped, and it is the failure mode reranking provably cannot touch, since promotion
cannot reach a chunk that was never retrieved. Note it tracks the size change rather than the
strategy: fixed-800 reaches the same 85.7% at k=20.

Every label has been verified twice, once to confirm the substring exists in the document it is
labelled against, and once to confirm it survives intact inside a single stored chunk. A label split
across a chunk boundary would be unachievable and indistinguishable in the output from a retrieval
failure. Both checks pass, so these are genuine failures rather than broken labels.

## Reading the results

`cmd/run` searches at k=20 and reports recall@1, @3, @5, @10 and @20 from the same ranked list, at no
extra cost.

The full curve matters more than any single value:

- **A random retriever scores 11.1% recall@5** on 45 documents, since 5/45 is the chance of the
  right document appearing by luck. Passage-level the floor is 0.4%, 5 of 1,126 chunks. A result near
  either floor means the harness is broken, not that retrieval is bad. This is the first sanity check
  to run.
- **`recall@20 - recall@5` is the headroom available to a reranker.** A reranker can only promote
  chunks the retriever already surfaced, so that gap is the absolute ceiling on what one could buy.
  If the gap is small, a reranker is not the next thing to build. This is what killed the reranker on
  the document metric (2.9 points) and revived it on the passage metric (14.3 points), from the same
  run.
- **If recall@20 is also low**, the answer is not being found at all. That is a retrieval problem
  rather than a ranking problem, and no reranker helps, because promotion cannot reach something that
  was never in the list. Nine questions are in that state under the hardest configuration, and they
  are the argument for looking at chunking next.

## Answer quality

`cmd/judge` measures the half `cmd/run` cannot see. It runs the real `/query` pipeline in process,
then grades each answer with a second model on two axes, scored 0 to 2:

- **Faithfulness.** Is every claim in the answer supported by the sources it was given? This is the
  hallucination detector. Graded against the sources only; the reference fact is withheld from this
  axis, so an answer that is true in the world but unsupported by these passages scores badly.
- **Correctness.** Does the answer state the labelled fact? A refusal scores 0 here and 2 on
  faithfulness, which is the combination that identifies honest abstention.

There is deliberately no citation axis. `sources[]` is the five reranked chunks, always, whatever the
model does; the model never selects citations, so there is nothing per-claim to validate.

**The scale is three points, not five.** Five point rubrics collapse onto 4 and stop discriminating,
which is the same failure as a retrieval baseline that scores 97% because the task was too easy.

**The judge is a different model from the generator.** Answers come from Claude Haiku 4.5, grading
from Claude Sonnet 4.6. A model grading its own output invites self-preference bias.

### Validating the judge

Run this before trusting any score:

```bash
go run ./eval/cmd/judge -validate -n 5
```

It grades deliberately broken answers and asserts the judge can tell them apart. Each variant is
built to move one axis and leave the other alone, which distinguishes a judge that is reading from
one that merely dislikes odd input.

| variant | faithfulness | correctness |
|---|---|---|
| clean | 2.00 | 2.00 |
| swapped sources (right answer, another question's passages) | **0.20** | 2.00 |
| fabrication appended (one invented flag) | **1.00** | 2.00 |
| refusal ("I don't know") | 2.00 | **0.00** |

Swapped sources drove faithfulness to 0.20 while correctness held at 2.00. That is the axis
isolation working: the answer still states the fact, it simply is not supported by what it was shown.
The command exits non-zero if any separation check fails, so it is a gate rather than another table
to eyeball.

### Results

Corpus `e95679c`, structured-800 chunking, reranking on, measured 2026-07-31.

| phrasing | segment | n | faithfulness | correctness |
|---|---|---|---|---|
| original | retrieved | 32 | 2.00 | 1.94 |
| original | not retrieved | 3 | 2.00 | 0.67 |
| original | **overall** | 35 | **2.00** | **1.83** |
| paraphrased | retrieved | 27 | 2.00 | 1.89 |
| paraphrased | not retrieved | 8 | 2.00 | 0.25 |
| paraphrased | **overall** | 35 | **2.00** | **1.51** |

**Zero unfaithful answers across 70 gradings.** Faithfulness was 2 on every question in both runs,
including all 8 paraphrased questions where retrieval failed outright. Handed nothing useful, the
model declined rather than inventing an answer, which is exactly what the grounding prompt asks for
and exactly what recall can never show.

Faithfulness being pinned at 2.00 would normally be the saturation alarm. It is not, and only because
the validation above ran first: the same judge scored fabrication at 1.00 and swapped sources at
0.20, so it demonstrably can fail an answer. Ordering is what makes the ceiling readable as a result
instead of a broken ruler.

**Correctness tracks retrieval, as it should.** Paraphrased correctness falls from 1.89 to 0.25 when
the answering passage is missing. The 27 of 35 retrieved here is 77.1%, matching the structured-800
reranked figure from `cmd/run` exactly, which is a useful cross-check: two independently written
harnesses agreeing to the decimal.

### Caveats on these numbers

- **One "not retrieved" question scored 2 anyway**, in both runs. The labelled substring was absent
  from the returned sources while some other passage still answered the question. That is a label
  being stricter than reality, not a model being lucky, and it means the "not retrieved" segment is a
  slight underestimate of what the service can actually answer.
- **Five of 35 verdicts came back with no reason attached.** They are counted and reported, because a
  score that cannot be spot-checked cannot be audited. They cluster on refusals and on zeros.
- **One retrieved question scored 0 on correctness** in the paraphrased run where the answer arguably
  deserved a 1: it stated the right value and then hedged it into an alternative. Judge variance at
  the boundary between 0 and 1 is real, and at 35 questions one verdict is 2.9 points.
- **The judge is not itself validated against human labels.** `-validate` proves it separates good
  answers from broken ones. It does not prove its scale agrees with a person's.

## What this does not measure

- **Whether a hit is genuinely useful.** A substring label is a proxy for relevance, not relevance
  itself. Requiring the right document as well removes most of the risk that an incidental mention
  counts as an answer. It does not remove all of it.
- **Real user phrasing.** The two phrasings bracket it rather than sample it. Every question was
  written by the same person who read the source, which is a bias no amount of care removes.
- **The deployed service.** Measuring below HTTP means a route typo, a handler dropping a field, or
  a wrong `topK` in `main` would not show up here. One smoke request against `/query` covers that,
  and it is a different test.
- **Scale.** 45 documents and ~1,126 chunks is laptop-sized. pgvector does a sequential scan at this
  size and is fast enough that no index is warranted, so nothing here says anything about behaviour
  at ten million chunks.

## Sample size

35 questions means one question flipping is 2.9 percentage points. A six point difference between two
configurations is two questions and should be read as noise, not a finding. This is the main
limitation of the whole setup and it is a labelling budget problem, not a design flaw that better code
would fix.

It is also the reason the per-type split earns its keep. The 41 point collapse in identifier recall
between phrasings, and the 20 to 28 point rise at rank 1 from reranking, are large enough to survive
the noise floor. The 5.8 point change at k=5 is not, and is reported as two questions rather than as a
result.
