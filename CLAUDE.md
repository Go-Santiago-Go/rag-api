# CLAUDE.md

Guidance for working in this repository.

## What this is

A containerized **Go RAG (Retrieval-Augmented Generation) service** that ingests documents and
answers questions about them over HTTP, returning grounded answers with structured citations:
`{ answer, sources[] }`. Two endpoints, one service:

- `POST /ingest` makes a document searchable: chunk it, embed each chunk (Bedrock), store vectors.
- `POST /query` answers a question: embed it, similarity-search chunks, have the LLM write a cited
  answer, return `{ answer, sources[] }`.

It is a deliberately plain HTTP microservice that knows nothing about agents. Project 2 (a separate
Strands agent) consumes `/query` as a tool. The structured `sources[]` response is the contract that
keeps that boundary clean.

**Stack:** Go, pgvector/Postgres, AWS Bedrock (Titan v2 embeddings plus a Claude model for
generation), S3 (raw doc storage), Docker, Terraform, GitHub Actions to ECR, ECS Express Mode on
Fargate.

**Canonical name:** `rag-api`. Use it for the repo and the README title. Do not introduce alternate
names.

The Go module path is `github.com/go-santiago-go/rag-api` and must track the repository URL. Go
resolves a module by fetching its repo and then checking the `module` line agrees; if the two
disagree, `go get` and `go install` fail on the mismatch.

One deliberate exception: **AWS resource names stay `go-rag-api-*`.** They are infrastructure
identifiers rather than the repo name, renaming them forces Terraform to destroy and recreate the
resources, and the ECR repository name is a contract shared between `infra/bootstrap`, the `data`
lookup in `infra/`, and the CI push.

## Status: built and deployed (Phase 8 complete)

The full service is implemented and has been deployed to AWS end to end: the `/ingest` and `/query`
loop runs locally via `docker compose up`, tests pass in CI, and the container has been deployed on
ECS Express Mode behind a live public URL that returns grounded `{ answer, sources[] }`. A separate
downstream Strands agent (Project 2) consumes `/query` as a tool and is not built yet.

Because AWS resources are torn down after each session to avoid cost (`terraform destroy`), the
public URL is regenerated per deploy rather than kept always-on. `DEPLOYMENT.md` is the full
clone-and-deploy walkthrough.

## Layout

```
cmd/server/main.go         # entrypoint: wires dependencies, starts the server. Thin.
internal/handler/          # HTTP handlers: parse request, call service, write response. No logic here.
internal/service/          # RAG logic: chunk, embed, search, generate.
internal/store/            # VectorStore interface plus pgvector implementation. Only place that knows SQL.
internal/store/schema.sql  # canonical schema, embedded in the binary and applied idempotently on startup.
eval/                      # evaluation: pinned corpus, labelled questions, recall@k and answer-quality harnesses.
eval/README.md             # results, controls and caveats. Read before changing retrieval or chunking.
migrations/001_init.sql    # same schema, for the local docker-compose init hook.
infra/                     # Terraform: the billable app stack (VPC, RDS, S3, ECS Express service).
infra/bootstrap/           # Terraform: the free, persistent stack (ECR repo, GitHub OIDC CI role).
docker-compose.yml         # local pgvector (pgvector/pgvector:pg16).
Dockerfile                 # multi-stage; distroless final image.
DEPLOYMENT.md              # how to clone and deploy to AWS.
content/                   # gitignored personal writeups and drafts; not part of the shipped repo.
```

Use Go's conventional layout: `cmd/` for the `main` entrypoint, `internal/` for app code the
compiler forbids other modules from importing.

## Commands

```bash
go run ./cmd/server          # run the service (listens on :8080)
go build ./...               # build everything
go vet ./...                 # static checks (also runs in CI)
go test ./...                # tests (also runs in CI)
docker compose up -d         # start local Postgres plus pgvector
curl localhost:8080/health   # health check
```

CI (`.github/workflows/ci.yml`) runs `go build`, `go vet`, and `go test` on push and PR.

## Key conventions and design

- **Dependency inversion at the boundaries** is the core idea. Service logic depends on the
  `VectorStore` interface (and embedder/generator interfaces), never on pgvector or Bedrock
  directly. Concrete implementations are plugged in at `main`. This is what lets tests use a fake
  store with no database, and lets pgvector be swapped without touching RAG logic.
- **Handlers orchestrate, services do the work.** Handlers parse HTTP and return status codes; all
  business logic lives in `internal/service` so it stays unit-testable.
- **Same embedding model on both sides.** Documents and questions must be embedded with the same
  Bedrock model, since vectors from different models aren't comparable.
- **Vector column dimension must match the embedding model** (e.g. `vector(1024)` for Titan v2).
- **`/query` returns `{ answer, sources[] }`**, not prose. Structured sources are the contract for
  the human demo and for Project 2's agent.
- **Modern stdlib.** Use `net/http` with Go 1.22+ method-based routing
  (`mux.HandleFunc("GET /health", …)`) and `log/slog` for structured logs. No web framework; stdlib
  is sufficient.
- **Testing.** Test pure logic (chunking) directly; test `/query` with a fake `VectorStore`
  implementation rather than a real DB. Prefer table-driven tests.
- **Retrieval changes are measured, not argued.** `eval/` holds a pinned corpus, 35 hand-labelled
  questions and a recall@k harness. Take a baseline before a change and re-measure after, and add a
  control when a change varies more than one thing at once. Two adopted changes (reranking,
  chunking) and two rejected ones (hybrid search, document-level labels) all came from this, as did
  one retracted finding. 35 questions means one question is 2.9 points, so small deltas are noise.
- **A judge is validated before it is trusted.** `eval/cmd/judge` grades answer faithfulness and
  correctness, and `-validate` grades deliberately broken answers first to prove it can separate them
  from good ones. Run validation before quoting any score: a grader that scores everything highly is
  indistinguishable from a service that answers everything well.
- **Comments explain the *why*, not the *what*.** Document exported identifiers per Go convention.

## Deployment (AWS) notes

Non-obvious realities of the deployed setup, learned in Phase 8:

- **Two Terraform stacks, split by lifetime.** `infra/bootstrap/` holds the free, long-lived pieces
  (ECR repository, GitHub OIDC CI role) and stays up. `infra/` is the billable app stack (VPC, RDS,
  S3, ECS Express service) and is destroyed after each session. The app stack looks the ECR repo up
  by name with a `data` source, so `infra/bootstrap` must be applied first.
- **DB connection resolves per environment.** Locally the app uses `DATABASE_URL` (docker-compose).
  In the cloud there is no `DATABASE_URL`; the app reads the standard `PG*` vars, and ECS injects
  `PGPASSWORD` from Secrets Manager at task launch. The password never enters the image or Terraform
  state (RDS owns it via `manage_master_user_password`).
- **The app self-migrates on startup.** `internal/store/schema.sql` is embedded and applied
  idempotently (`IF NOT EXISTS`) because cloud RDS has no init hook and the distroless image has no
  `psql`. Safe to run every boot.
- **ECS Express Mode couples LB and task subnet placement into one subnet set.** To get a public URL
  the tasks run in the *public* subnets (locked down by the app SG, which has no inbound rule except
  the LB rule Express adds itself). The private app subnets and NAT from the three-tier design are
  provisioned but off the request path. Dropping to hand-rolled `aws_ecs_service` is the escape hatch
  if tasks must be fully private.
- **Deploy by `:latest`, and beware a stale tag.** The classic failure is the running container
  being an older image than the code, which crash-loops on `localhost`. Rebuild and re-push before
  concluding config is wrong. CI also tags by git SHA for traceability.
- **Teardown gotcha.** `terraform destroy` can hang: Express Mode deletes its gateway resource but
  orphans its ALB, whose ENIs hold public IPs that block the internet gateway (and the whole VPC)
  from deleting. If destroy times out on the Express service or the IGW, manually delete the orphaned
  ALB (`ecs-express-gateway-alb-*`) and its `ecs-gateway-tg-*` target groups, then re-run destroy.
  Also: pipe destroy output carefully; `terraform destroy | tee | tail` returns tail's exit code and
  hides Terraform's real failure.

## Local-first, cloud-last

Build and prove the whole loop on the laptop (Docker) through Phase 6 before any AWS work. Keep AWS
cheap and run `terraform destroy` after each session so nothing bills overnight.

## Related instructions

`CLAUDE.local.md` (git-ignored, not part of the shipped project) holds local working notes and
conventions. Read it if present; it governs *how* to work in this repo.
