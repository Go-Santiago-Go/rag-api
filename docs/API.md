# API reference

Two endpoints carry the product, `POST /ingest` and `POST /query`, plus a browser demo and a liveness
probe. Everything is JSON over plain HTTP. There is no authentication on any endpoint, which is a
deliberate scoping call for a stack that is destroyed after each session and is called out in the
README's known gaps.

| Endpoint | Purpose |
|---|---|
| `POST /ingest` | Chunk a document, embed each chunk, store the vectors. Makes it searchable. |
| `POST /query` | Embed a question, retrieve, rerank, generate a grounded answer with citations. |
| `GET /` | The embedded browser demo of the query path. |
| `GET /health` | Liveness only. Touches neither Bedrock nor the database. |

## `POST /ingest`

Chunks the text, embeds each chunk with Titan Text Embeddings V2, and stores the vectors in pgvector.

```bash
curl -i -X POST localhost:8080/ingest -H 'Content-Type: application/json' \
  -d '{"document_id":"doc-1","text":"pgvector stores embeddings inside Postgres."}'
# HTTP/1.1 201 Created
```

### Request

```json
{ "document_id": "string", "text": "string" }
```

Both fields are required. `document_id` is the caller's own identifier and is echoed back on every
chunk that came from this document, so it is what a consumer groups citations by. Re-ingesting the same
`document_id` adds chunks rather than replacing them; there is no upsert.

### Status codes

| Code | Meaning |
|---|---|
| `201` | Every chunk was embedded and stored |
| `400` | Malformed body, or a missing `document_id` or `text` |
| `500` | Bedrock or database failure |

**The call is synchronous and returns only once every chunk is stored.** Nothing is queued and
nothing is batched, so wall-clock time grows linearly with document length: one Bedrock embedding
call per chunk, in sequence. That is fine for the pinned evaluation corpus and it is the first thing
that would change under real load.

### Chunking behavior

Chunking happens here, at ingest time, which is why changing any chunking variable means re-ingesting
rather than restarting. The default is structure-aware splitting on markdown headings with an
800-rune ceiling:

- A section that fits under the ceiling becomes one chunk.
- A section over the ceiling falls back to fixed-size splitting, with the heading repeated on each
  piece so a citation still says what it is about.
- A section under a minimum merges with what follows, so a lone heading is never its own chunk.

Set `CHUNK_STRATEGY=fixed` for plain fixed-size splitting with overlap. Both strategies implement the
same `Chunker` interface, which is what let them be measured against each other rather than argued
about. The chosen configuration is logged at startup, so any set of evaluation numbers traces back to
the settings that produced it. Full variable reference in [LOCAL_DEV.md](LOCAL_DEV.md#configuration).

## `POST /query`

Embeds the question with the same Titan v2 model used at ingest, retrieves the 20 nearest chunks by
cosine distance, reranks them with Cohere Rerank v3.5, keeps the best 5, and has Claude write an
answer constrained to those 5.

```bash
curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"question":"Where does pgvector store embeddings?"}'
```

### Request

```json
{ "question": "string" }
```

Required, and rejected when empty. There is no `top_k`, no filter, and no conversation history: the
service is stateless and every query is independent. Retrieval depth is a server-side constant so that
an evaluation number means something, since a caller who could vary `top_k` per request would make
recall@5 unmeasurable.

### Response

```json
{
  "answer": "pgvector stores embeddings inside Postgres.",
  "sources": [
    { "content": "pgvector stores embeddings inside Postgres.", "document_id": "doc-1" }
  ]
}
```

### Status codes

| Code | Meaning |
|---|---|
| `200` | An answer was generated, including when the model declines for lack of grounding |
| `400` | Malformed body, or an empty `question` |
| `500` | Bedrock or database failure |

**A question the corpus cannot answer is still a `200`.** Retrieval always returns its nearest
neighbours, however far away they are, so there is no empty-result path. What happens instead is that
the model is handed passages that do not contain the answer and declines rather than inventing one,
which is the behavior the answer-quality harness measures: across the 8 questions where retrieval
failed outright, faithfulness stayed at 2.00.

### What `sources[]` guarantees

The array is ordered by the reranker, most relevant first. The relevance score orders them internally
and is deliberately not part of the response, because a number that looks like a confidence value
invites a downstream consumer to threshold on it, and a cross-encoder score is not calibrated for
that.

**The passages returned are the same slice that built the prompt.** Not a re-query, not a second
retrieval pass, not a reconstruction. That identity is the whole point of the endpoint: a caller can
verify an answer against exactly what produced it, and the claim is enforced by construction in
`internal/service/query.go` rather than by convention.

This is also the contract that keeps the service free of agent concerns. A downstream agent gets
structured data to reason over instead of prose it would have to parse.

## `GET /`

The browser demo, served from a page compiled into the binary with `//go:embed`, which is what lets
it ship in a distroless image with no filesystem to copy assets into. It calls `POST /query` like any
other client, so it cannot display anything the API does not return.

It is registered as `GET /{$}` rather than `GET /`. In Go's `net/http` mux a bare `GET /` is a
catch-all: it would answer every unmatched path with the demo page instead of a `404`. The `{$}`
anchors the pattern to the site root exactly.

## `GET /health`

Liveness only. Returns `{"status": "ok"}` as soon as the process is serving.

It deliberately touches neither Bedrock nor the database, which means **a green health check coexists
with missing model access.** That is the intended split: the probe answers "is this process alive"
for the load balancer, not "is every dependency reachable." The real end-to-end check is an `/ingest`
followed by a `/query`, which is what [OPERATIONS.md](OPERATIONS.md#verifying-a-deploy-is-actually-healthy)
uses to verify a deploy.
