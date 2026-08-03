# CLAUDE.md

How to get this repository running and how to verify a change. Everything else lives in `docs/`; see
the map at the bottom.

`rag-api` is a containerized Go RAG service that ingests documents and answers questions about them
over HTTP, returning a grounded answer with structured citations: `{ answer, sources[] }`. Two
endpoints. `POST /ingest` chunks a document, embeds each chunk on Bedrock, and stores the vectors in
pgvector. `POST /query` embeds the question, retrieves in two stages (vector search, then a reranker),
and has a Claude model write an answer citing only what it was given. A demo page at `GET /` is
embedded in the binary and exercises both in a browser.

## Run it

Needs Docker for the local Postgres, and AWS credentials in a region with Bedrock model access
granted for three models: Titan Text Embeddings V2, Cohere Rerank v3.5, and a Claude model.

```bash
docker compose up -d           # Postgres + pgvector on :5432, schema applied by the init hook
export AWS_REGION=us-east-1
go run ./cmd/server            # listens on :8080
```

```bash
curl -X POST localhost:8080/ingest -H 'Content-Type: application/json' \
  -d '{"document_id":"doc-1","text":"pgvector stores embeddings inside Postgres."}'

curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"question":"Where does pgvector store embeddings?"}'
# { "answer": "...", "sources": [ { "content": "...", "document_id": "doc-1" } ] }
```

`make help` lists the task runner targets. The verbs (`up`, `run`, `test`, `lint`, `deploy`,
`destroy`) are the same in every repo in this portfolio.

## Verify a change

What CI runs, and what should pass before any commit:

```bash
go build ./... && go vet ./... && go test ./...
```

The tests need no database and no cloud access. `internal/service` runs against a fake `VectorStore`
and chunking is tested as pure logic, which is the payoff of the interface boundaries in
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).

Narrower loops while working:

```bash
go test ./internal/service                      # one package
go test -run TestStructuredChunker ./internal/service   # one test
```

Retrieval and answer quality are measured, not argued. The harness drives the real service, so start
the stack first:

```bash
make eval-load      # ingest the pinned corpus through the real POST /ingest
make eval-recall    # recall@k on the 35 labelled questions
make eval-judge     # validate the judge, then grade faithfulness and correctness
```

Take a baseline before changing chunking or retrieval and re-measure after. Results, controls, and the
rejected and retracted findings are in [`eval/README.md`](eval/README.md).

The deployed artifact is a multi-stage build to distroless:

```bash
docker build -t rag-api:local .
```

## Things that will waste your time

- **A native Postgres on `:5432` silently shadows the container.** The app connects to the wrong
  database and `docker compose ps` still looks healthy. Check with `ss -lntp | grep 5432` before
  blaming the code.
- **Bedrock access is opt in per account, and `/health` will not tell you it is missing.** The health
  check touches neither Bedrock nor the database, so a green probe coexists with `AccessDenied` on
  every request. The real check is an `/ingest` then `/query` pair.
- **Re-ingest after changing any `CHUNK_*` variable.** Chunking happens at ingest time, so a restart
  alone leaves the old chunks in the database. The active configuration is logged at startup, which is
  what lets a set of eval numbers be traced back to the settings that produced it.
- **Never quote a judge score from a run that skipped `-validate`.** It grades deliberately broken
  answers and exits non-zero if it cannot separate them from good ones. Quote the `-paraphrase`
  numbers, and remember that with 35 questions one question is 2.9 points, so a sub-3-point delta is
  noise rather than a result.
- **A green `go test ./...` skipped the store.** `internal/store/postgres_test.go` skips unless
  `TEST_DATABASE_URL` points at a throwaway database.
- **`infra/bootstrap/` must be applied before `infra/`.** The app stack looks the ECR repository up by
  name with a `data` source. `bootstrap` is free and stays up; `infra/` bills by the hour and gets
  destroyed after each session. `make destroy` also discards the ingested corpus, which is deliberate
  because `make eval-load` regenerates it.
- **Deploys go out as `:latest`, so suspect a stale tag first.** The classic failure is the running
  task being an older image than the code, which crash loops trying to reach `localhost`. Rebuild and
  re-push before concluding the configuration is wrong. CI also tags by git SHA.
- **`terraform destroy` can hang on an orphaned load balancer** that Express Mode leaves behind, which
  blocks the internet gateway and therefore the whole VPC. The fix and the rest of the failure modes
  are in [docs/OPERATIONS.md](docs/OPERATIONS.md).
- **If you forked this, two identities are still mine.** The `module` line in `go.mod` must track your
  repository URL or `go get` fails on the mismatch, and the OIDC `sub` claim that `infra/bootstrap`
  matches carries my org and repo IDs.
- **Before writing docs, read [docs/CONVENTIONS.md](docs/CONVENTIONS.md).** It carries the accuracy
  guards, and each one is there because it was gotten wrong.

## Where everything is

| Doc | Scope |
|---|---|
| [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) | The two request paths, why retrieval is two stages, the interface boundaries, how evaluation plugs in |
| [docs/API.md](docs/API.md) | Endpoint reference, request shapes, status codes, chunking behavior at ingest, what `sources[]` guarantees |
| [docs/LOCAL_DEV.md](docs/LOCAL_DEV.md) | Local run, full environment variable reference, development commands, the evaluation harness |
| [docs/DEPLOYMENT.md](docs/DEPLOYMENT.md) | What gets provisioned on AWS, both Terraform stacks, step by step deploy, teardown, troubleshooting |
| [docs/OPERATIONS.md](docs/OPERATIONS.md) | Verifying a deploy, cost, failure modes and the command that diagnoses each, honest constraints |
| [docs/CONVENTIONS.md](docs/CONVENTIONS.md) | Documentation rules, the README spine, accuracy guards, generated artifacts |
| [eval/README.md](eval/README.md) | The measurement record: adopted, rejected, and retracted findings, with their controls |

| Path | Contents |
|---|---|
| `cmd/server/` | The composition root. The only package that names a concrete store or Bedrock client. |
| `internal/handler/` | HTTP parsing, status codes, and the embedded demo page. No RAG logic. |
| `internal/service/` | Chunking, embedding, retrieval, reranking, generation, and the interfaces they satisfy. |
| `internal/store/` | The `VectorStore` interface and its pgvector implementation. The only package that knows SQL. `schema.sql` is embedded and applied idempotently on startup, so cloud RDS needs no migration step. |
| `eval/` | Pinned corpus, 35 labelled questions, and three commands: `cmd/load`, `cmd/run` (recall@k), `cmd/judge` (answer quality). |
| `migrations/` | The same schema, for the docker-compose init hook. |
| `infra/` | Terraform. `infra/bootstrap/` is free and persistent; `infra/` is billable. |
| `.github/workflows/` | `ci.yml` builds, vets, and tests. `deploy.yml` pushes to ECR over OIDC on `main`. |
