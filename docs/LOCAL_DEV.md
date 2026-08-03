# Local development

The fastest path is local: Postgres runs in Docker and the Go service runs natively. The whole RAG
loop works on your laptop in a few commands, with no AWS infrastructure to provision and nothing
billing by the hour. It does call Bedrock to embed, rerank, and generate, so the local run is not
fully offline.

**Prerequisites.** [Go 1.26+](https://go.dev/doc/install),
[Docker](https://docs.docker.com/get-docker/), and AWS credentials configured (`aws configure`) with
[model access](https://docs.aws.amazon.com/bedrock/latest/userguide/model-access.html) enabled for
Titan Text Embeddings V2, Cohere Rerank v3.5, and a Claude model in your region. Bedrock is opt in per
account, and until access is granted the service starts fine and fails on the first `/ingest` with
`AccessDenied`.
## Run it

```bash
# 1. Clone
git clone https://github.com/Go-Santiago-Go/rag-api.git
cd rag-api

# 2. Start Postgres + pgvector (the schema auto-applies on first boot)
docker compose up -d

# 3. Run the service. It reads AWS credentials from your environment / ~/.aws,
#    connects to the local database, and listens on :8080.
go run ./cmd/server
```

Then open <http://localhost:8080> for the browser demo, or drive it from the command line:

```bash
curl -X POST localhost:8080/ingest -H 'Content-Type: application/json' \
  -d '{"document_id":"doc-1","text":"pgvector stores embeddings inside Postgres."}'

curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"question":"Where does pgvector store embeddings?"}'
# { "answer": "...", "sources": [ { "content": "...", "document_id": "doc-1" } ] }
```

## Configuration

Every variable the service reads, mirroring [`.env.example`](../.env.example). `make run` sources a
sibling `.env` if one exists, so overrides need no exporting.

| variable | default | what it does |
|---|---|---|
| `DATABASE_URL` | local compose DSN | Full Postgres connection string. Takes precedence over the `PG*` vars. |
| `PGHOST` `PGUSER` `PGPASSWORD` `PGDATABASE` `PGSSLMODE` | unset | Read directly by pgx when `DATABASE_URL` is unset. The cloud path: ECS injects `PGPASSWORD` from Secrets Manager at task launch. |
| `AWS_REGION` | from the SDK config chain | Region for Bedrock. Must have model access granted. |
| `CHUNK_STRATEGY` | `structured` | `structured` (split on markdown headings) or `fixed`. |
| `CHUNK_SIZE` | `800` | Maximum chunk size in runes. |
| `CHUNK_OVERLAP` | `80` | Overlap between fixed-size chunks, in runes. |
| `TEST_DATABASE_URL` | unset | Throwaway database for the pgvector store test, which is skipped when unset. |

The listen port is fixed at `:8080`.

**Connection settings resolve most-specific-first**, which is what lets one binary serve both
environments: `DATABASE_URL` locally, the `PG*` vars in the cloud where no such single string exists.
Credentials are never read from a variable here. The AWS SDK walks its standard chain, so the same
image uses your local credentials on a laptop and the task role on Fargate.

**Re-ingest after changing any `CHUNK_*` variable.** Chunking happens at ingest time, so a restart
alone leaves the old chunks in the database. The chosen configuration is logged at startup, which is
what lets a set of evaluation numbers be traced back to the settings that produced it.

## Development commands

```bash
go build ./...   # build everything
go vet ./...     # static checks (also runs in CI)
go test ./...    # tests (also runs in CI)
```

CI runs `go build`, `go vet`, and `go test` on every push and pull request, with the Go version
sourced from `go.mod` so it lives in one place.

The tests need no database and no cloud access. `internal/service` is tested against a fake
`VectorStore`, and chunking is tested as pure logic, which is the practical payoff of the interface
boundaries described in
[ARCHITECTURE.md](ARCHITECTURE.md#five-interfaces-and-the-query-path-depends-on-nothing-else).

## Running the evaluation harness

`eval/` holds a pinned corpus, 35 hand-labelled questions, a recall@k harness, and an answer-quality
judge. Take a baseline before changing retrieval or chunking, then re-measure after.

The harness talks to a running service, so start the stack first.

```bash
docker compose up -d
go run ./cmd/server          # in another terminal

# Load the pinned corpus through the real /ingest path
go run ./eval/cmd/load

# Retrieval: recall@k against the labelled passages
go run ./eval/cmd/run -rerank -paraphrase
go run ./eval/cmd/run -rerank -paraphrase -verbose   # per-question ranks

# Answer quality: validate the judge first, then grade
go run ./eval/cmd/judge -validate
go run ./eval/cmd/judge -paraphrase
```

**Always run `-validate` before quoting a score.** It grades deliberately broken answers and exits
non-zero if the judge cannot separate them from good ones. A grader that scores everything highly is
indistinguishable from a service that answers everything well.

`-paraphrase` asks the identifier questions in their token-free phrasing. It is the harder and more
honest of the two question sets, because the alternative phrasing leaks the answer's distinctive
tokens into the query. Quote paraphrased numbers.

Full results, controls, and caveats are in [`eval/README.md`](../eval/README.md).

## A local gotcha

If the service cannot reach the database but `docker compose ps` looks healthy, check for a native
Postgres already listening on `:5432`. It shadows the container, and the app connects to the wrong
database with no obvious error.

```bash
ss -lntp | grep 5432
```

To deploy the same service to AWS behind a public URL, see [DEPLOYMENT.md](DEPLOYMENT.md). The
endpoint reference is in [API.md](API.md).
