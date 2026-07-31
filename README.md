# rag-api

[![ci](https://github.com/Go-Santiago-Go/rag-api/actions/workflows/ci.yml/badge.svg)](https://github.com/Go-Santiago-Go/rag-api/actions/workflows/ci.yml)
[![deploy](https://github.com/Go-Santiago-Go/rag-api/actions/workflows/deploy.yml/badge.svg)](https://github.com/Go-Santiago-Go/rag-api/actions/workflows/deploy.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

**A production-shaped Retrieval Augmented Generation (RAG) service in Go, deployed on AWS.** It runs
the full RAG pipeline over HTTP (chunking, embedding, vector search, and grounded generation) and
answers questions with citations back to the source text: `{ answer, sources[] }`.

Retrieval uses Amazon Bedrock embeddings and pgvector similarity search, with storage and model calls
behind interfaces so the pipeline is unit tested with zero cloud access. The platform side is fully
infrastructure as code: a multi-tier VPC in Terraform, keyless CI/CD to ECR over GitHub OIDC, and a
distroless container on ECS Fargate. Built to be consumed by a human, a browser, or an agent.

Two endpoints, one service:

- `POST /ingest` makes a document searchable: chunk the text, embed each chunk with Bedrock (Titan
  v2), and persist the vectors in pgvector.
- `POST /query` answers a question: embed the question with the same model, run a cosine similarity
  search for a shortlist of chunks, rerank that shortlist with a cross-encoder, and have a Claude
  model write an answer grounded in the survivors, returned
  as `{ answer, sources[] }`.

> **Deployed:** the full service runs on AWS via ECS Express Mode. `terraform
> apply` provisions the stack and outputs a public HTTPS URL; a `/query` against it returns grounded
> `{ answer, sources[] }`, verified end to end (Bedrock embeddings + generation, pgvector in RDS).
> The stack is torn down with `terraform destroy` after each session to avoid cost, so the URL is
> regenerated per deploy rather than kept always-on.

## Architecture

The service, end to end:

```mermaid
flowchart LR
    U[Client] -->|POST /ingest| S[Go service]
    U -->|POST /query| S
    S -->|embed + generate| B[AWS Bedrock]
    S -->|store / search vectors| P[(pgvector)]
    S -->|raw docs| O[(S3)]
```

A document is split into chunks on its markdown section boundaries, each chunk is embedded with
Bedrock, and the vectors are stored in pgvector.
A query is embedded with the same model, then answered in two retrieval stages: vector similarity
search returns a shortlist of 20 candidates, and a cross-encoder reranks those and keeps the best 5.
Those 5 are handed to an LLM that writes an answer grounded in them, returned with the source chunks
it used.

The two stages exist because they fail differently. Similarity search is fast enough to scan the
whole corpus but encodes each chunk without knowing what will be asked, so its ranking is coarse. A
cross-encoder reads the question and a passage together and judges that specific pair far better, but
nothing about it can be precomputed, so it can only afford to look at a shortlist. Cheap and
approximate narrows the field; expensive and accurate orders what survives.

### Deployment (AWS)

The container runs on **Amazon ECS Express Mode**: from an image plus three IAM roles, Express Mode
provisions the Fargate service, an internet-facing load balancer with TLS, autoscaling, health
checks, and the security-group wiring between the load balancer and the tasks, and hands back a
public `*.ecs.<region>.on.aws` URL. The data tier stays private: RDS Postgres has no public endpoint
and accepts connections only from the app's security group.

```mermaid
flowchart TB
    user(["Client / Agent · Internet"])

    subgraph vpc["Amazon VPC · 10.0.0.0/16 · Region us-east-1"]
        direction TB
        igw{{"Internet Gateway"}}

        subgraph web["🌐  WEB TIER · public subnets"]
            direction LR
            alb(["Application Load Balancer<br/>internet-facing"])
            nat{{"NAT Gateway"}}
        end

        subgraph app["⚙️  APP TIER · private subnets"]
            direction LR
            taskA["ECS Fargate task<br/>go-rag-api :8080"]
            taskB["ECS Fargate task<br/>go-rag-api :8080"]
        end

        subgraph data["🗄️  DATABASE TIER · private subnets"]
            direction LR
            rds[("RDS PostgreSQL 16<br/>pgvector · primary")]
            standby[("standby slot · parked<br/>Multi-AZ in prod")]
        end
    end

    subgraph svc["Regional AWS services"]
        direction LR
        bedrock["Amazon Bedrock<br/>Titan v2 · Claude Haiku"]
        ecr[("Amazon ECR")]
        secrets["Secrets Manager<br/>RDS password"]
    end

    user ==>|"HTTPS 443"| igw
    igw ==> alb
    alb ==>|"8080"| taskA
    alb ==>|"8080"| taskB
    taskA ==>|"5432"| rds
    taskB ==>|"5432"| rds
    rds -.->|"replication (prod)"| standby
    taskA -.->|"egress"| nat
    nat -.-> bedrock
    nat -.-> ecr
    nat -.-> secrets

    classDef ext fill:#232f3e,stroke:#ff9900,color:#ffffff;
    classDef net fill:#e7f0fb,stroke:#1a73e8,color:#0b3d91;
    classDef compute fill:#fdecd2,stroke:#ff9900,color:#7a4f01;
    classDef db fill:#e7f4ea,stroke:#2e7d32,color:#1b5e20;
    class bedrock,ecr,secrets ext;
    class igw,alb,nat net;
    class taskA,taskB compute;
    class rds,standby db;

    style vpc fill:#f6f2fb,stroke:#7d3ac1,stroke-width:2px,color:#4a1d7a;
    style web fill:#eaf6ea,stroke:#2e7d32,stroke-width:3px,color:#1b5e20;
    style app fill:#e9f0fb,stroke:#1a73e8,stroke-width:3px,color:#0b3d91;
    style data fill:#e6f4f4,stroke:#0f766e,stroke-width:3px,color:#0f4c47;
    style svc fill:#fbfbfb,stroke:#bbbbbb,stroke-dasharray:2 2,color:#444444;

    linkStyle 0,1,2,3,4,5 stroke:#ff9900,stroke-width:2px;
```

The orange path traces a request down the tiers: **Client → Internet Gateway → Application Load
Balancer (web tier) → ECS task (app tier) → RDS (data tier)**, each hop crossing a security group
that trusts only the tier above. The dashed lines are the app's outbound calls, which leave the
private subnets through the NAT gateway: Bedrock for embeddings and generation, ECR for the image,
and Secrets Manager for the DB password, which is injected into the container at launch (the app
assembles its connection from `PG*` env vars, so the password is never in the image or Terraform
state).

**One honest trade-off (what the diagram idealizes).** The diagram shows the intended three-tier
design. In practice, ECS Express Mode uses a single subnet set for both the load balancer and the
tasks, so you cannot keep the tasks in the private app tier while the load balancer stays public. To
get a public URL the tasks actually run in the public subnets; they have public IPs but stay
unreachable from the internet because their security group has no inbound rule except the load
balancer's. So the private app subnets and NAT gateway are provisioned but off the real request path.
Keeping tasks fully private would mean dropping to a hand-rolled `aws_ecs_service`.

Single-AZ RDS keeps this demo cheap; production would run Multi-AZ RDS and VPC endpoints for the AWS
services. Everything is torn down with `terraform destroy` after each session so nothing bills
overnight.

The `infra/` directory holds two Terraform stacks, split by lifetime:

- **`infra/bootstrap/`** provisions the free, long-lived pieces: the ECR repository and the GitHub
  OIDC CI role. Apply it once and leave it up, so CI can push images at any time and images survive
  the app stack's teardown.
- **`infra/`** provisions the billable app stack (VPC, RDS, S3, and the ECS Express service). It
  looks the ECR repository up by name, so the bootstrap stack must be applied first. This is the
  stack you destroy after each session.

```bash
# Once: the persistent stack (free: ECR repository + CI role)
cd infra/bootstrap && terraform init && terraform apply

# Each session: the billable app stack (about 10 to 15 min; RDS ~5 min, then
# Express Mode waits for health checks)
cd infra && terraform init && terraform apply
terraform output service_url   # the live public URL
terraform destroy              # tear the app stack down when done
```

Credentials come from your AWS CLI configuration. The always-on costs while up are the RDS instance,
the NAT gateway, and the Express load balancer, so a `destroy` after each session keeps the bill at
pennies. The database password is never handed to Terraform: RDS generates it and stores it in
Secrets Manager (`manage_master_user_password`), so it never lands in state.

For a step by step clone and deploy walkthrough (Bedrock model access, both stacks, pushing an image,
testing the URL, and teardown), see [DEPLOYMENT.md](DEPLOYMENT.md).

## Design decisions

Every choice below optimizes for one constraint: the simplest component that satisfies the
requirement, reaching for managed or heavyweight services only where the workload genuinely
demands them. The decisions that are not load-bearing sit behind interfaces, so they can change
later without disturbing the core.

| Decision | Choice | Why | Also considered |
|---|---|---|---|
| Vector storage | pgvector / Postgres | One datastore, standard SQL, free and reproducible locally, swappable behind an interface | OpenSearch Serverless, S3 Vectors |
| API style | REST / JSON | Consumers are a human, a browser, and one agent tool; no streaming requirement yet | gRPC |
| Service shape | Single Go service | Smallest thing that ships; no premature split into a separate ingestion service | Separate Python ingestion service |
| Query response | `{ answer, sources[] }` | Structured citations make the demo verifiable and give a downstream agent clean data to reason over | Prose-only answers |
| Text extraction | Local extraction | Free and offline; reach for a managed service only if the workload needs it | AWS Textract |
| Compute | ECS Express Mode on Fargate | Managed networking, load balancing, and scaling from a container image; App Runner is closed to new customers | Full ECS Fargate |
| Retrieval | Dense search + cross-encoder rerank | Measured: reranking moved the answering passage into first place for 7 more questions out of 35, and reached dense retrieval's recall@5 using 3 chunks instead of 5 | Hybrid BM25 + RRF, dense only |
| Chunking | Structure-aware on markdown headings, 800 runes | Chunks are returned verbatim as citations, and fixed-size splitting left 70% of them starting mid-sentence; recall is at parity with fixed-size at the same ceiling | Fixed-size, semantic (embedding-distance) chunking |

The pattern under all of it is **dependency inversion at the boundaries**: the RAG logic depends on
`VectorStore`, `Embedder`, `Chunker`, `Reranker` and `Generator` interfaces, and the concrete pieces
(pgvector, Bedrock) are plugged in at `main`. That is what lets the service be tested with a fake
store and no database, and lets pgvector be swapped without touching the RAG logic.

`Chunker` earns its place for a second reason. Chunking strategy is the biggest lever on retrieval
quality and the one most worth experimenting with, so a new strategy is a new implementation rather
than an edit to the ingest pipeline, and two strategies can be compared without either one being
disturbed.

## Status

Built local-first, then deployed to AWS. Everything below is done and verified end to end:

- [x] HTTP server with `/health`
- [x] Postgres + pgvector running locally in Docker
- [x] `VectorStore` interface with a pgvector implementation
- [x] `POST /ingest`: document chunked, embedded, and stored
- [x] `POST /query`: grounded answer with structured sources
- [x] Meaningful tests running in CI (build, vet, test)
- [x] Terraform for a three-tier VPC, private RDS, S3, and IAM
- [x] Containerized with a distroless image; CI builds and pushes to ECR via OIDC
- [x] Deployed on ECS Express Mode with a live public URL, `/ingest` and `/query` verified in the cloud
- [x] Retrieval evaluation harness: pinned corpus, labelled question set, recall@k baseline
- [x] Cross-encoder reranking, adopted on measured evidence rather than by default
- [x] `Chunker` interface with fixed-size and structure-aware strategies, compared against controls

## Evaluation

Retrieval quality is measured, not assumed. [`eval/`](eval/) holds a pinned 45 document corpus, 35
questions hand-labelled with the passage that answers each one, and a harness reporting recall@k.

Passage-level recall@5 on the harder of the two question phrasings, 35 questions, showing what each
change bought:

| configuration | recall@5 |
|---|---|
| fixed-size chunking, dense retrieval (starting point) | 57.1% |
| \+ cross-encoder reranking | 62.9% |
| \+ 800-rune chunks | 74.3% |
| \+ structure-aware chunking (current default) | **77.1%** |

Every step was measured before being adopted, and the harness was as useful for what it ruled *out*:

- **Document-level labels were discarded.** They scored 97.1% recall@5 against a 100% ceiling, so the
  entire dynamic range was one question. A metric that cannot fail cannot inform.
- **Hybrid BM25 search was deprioritized.** Lexical matching only helps when the query contains the
  answer's rare token, which is the case dense retrieval already handles near-saturated. Where the
  failures actually are, the query holds no rare token either.
- **A finding was retracted.** An early result showed identifier questions outscoring conceptual ones,
  until a control revealed the questions had been written containing their own answers' distinctive
  tokens.
- **Chunk size mattered more than chunking strategy.** The largest single gain, +11.4 points, came
  from changing a constant. Structure-aware splitting cut severed chunk boundaries from 70% to 4.8%
  and moved recall barely at all. It is the default anyway, because chunks are returned verbatim as
  `sources[]` and a citation beginning mid-word is a defect the harness cannot see.

See [`eval/README.md`](eval/README.md) for the full results, the controls, and the caveats.

## Stack

- **Go** for the service (standard library `net/http`, no framework).
- **pgvector / Postgres** for vector storage.
- **AWS Bedrock** for embeddings (Titan v2), reranking (Cohere Rerank v3.5), and answer generation (Claude).
- **S3** for raw document storage.
- **Docker** to containerize, **Terraform** for infrastructure, **GitHub Actions** for CI/CD to ECR.
- **ECS Express Mode on Fargate** to run it.

## Local development

The fastest path is local: Postgres runs in Docker and the Go service runs natively. The whole RAG
loop works on your laptop in a few commands.

**Prerequisites.** [Go 1.26+](https://go.dev/doc/install), [Docker](https://docs.docker.com/get-docker/),
and AWS credentials with **Bedrock access**. The service embeds and generates through Amazon Bedrock,
so even the local run is not fully offline: configure the AWS CLI (`aws configure`) and enable
[model access](https://docs.aws.amazon.com/bedrock/latest/userguide/model-access.html) for Titan
Text Embeddings V2 and a Claude model in your region.

```bash
# 1. Clone
git clone https://github.com/Go-Santiago-Go/rag-api.git
cd rag-api

# 2. Start Postgres + pgvector (the schema auto-applies on first boot)
docker compose up -d

# 3. Run the service. It reads AWS credentials from your environment / ~/.aws,
#    connects to the local database, and listens on :8080.
go run ./cmd/server

# 4. In another terminal: ingest a document, then ask about it.
curl -X POST localhost:8080/ingest -H 'Content-Type: application/json' \
  -d '{"document_id":"doc-1","text":"pgvector stores embeddings inside Postgres."}'

curl -s -X POST localhost:8080/query -H 'Content-Type: application/json' \
  -d '{"question":"Where does pgvector store embeddings?"}'
# { "answer": "...", "sources": [ { "content": "...", "document_id": "doc-1" } ] }
```

Development commands:

```bash
go build ./...   # build everything
go vet ./...     # static checks (also runs in CI)
go test ./...    # tests (also runs in CI)
```

CI runs `go build`, `go vet`, and `go test` on every push and pull request, with the Go version
sourced from `go.mod` so it lives in one place. To run the same service on AWS behind a public URL,
see [DEPLOYMENT.md](DEPLOYMENT.md).

## Endpoints

### `POST /ingest`

Makes a document searchable: split the text into chunks, embed each chunk with Bedrock Titan v2, and
store the vectors in pgvector. Runs synchronously and returns `201 Created` once every chunk is
stored.

Chunking defaults to structure-aware splitting on markdown headings with an 800-rune ceiling, and is
configurable with `CHUNK_STRATEGY` (`structured` or `fixed`), `CHUNK_SIZE` and `CHUNK_OVERLAP`. The
chosen configuration is logged at startup, so any set of evaluation numbers traces back to the
settings that produced it. Sections over the ceiling fall back to fixed-size splitting with the
heading repeated on each piece; sections under half the ceiling merge with what follows.

The service resolves its database connection in three ways, most specific first: `DATABASE_URL` if
set (local dev / `docker compose`), otherwise the standard `PG*` vars (`PGHOST`, `PGUSER`,
`PGPASSWORD`, `PGDATABASE`, `PGSSLMODE`) which pgx reads directly (this is the cloud path, where ECS
injects `PGPASSWORD` from Secrets Manager), and otherwise a local default. It also applies the schema
idempotently on startup, so a fresh RDS database becomes usable with no separate migration step. It
calls Bedrock, so the machine running it needs AWS credentials with Bedrock access and the Titan v2
model enabled in the region.

```bash
docker compose up -d      # start local Postgres + pgvector; schema auto-applies on first boot
go run ./cmd/server       # start the service on :8080

curl -i -X POST localhost:8080/ingest \
  -H 'Content-Type: application/json' \
  -d '{"document_id":"doc-1","text":"pgvector stores embeddings inside Postgres."}'
# HTTP/1.1 201 Created
```

Request body: `{ "document_id": string, "text": string }`. Both fields are required; a malformed or
incomplete body returns `400`, and a Bedrock or database failure returns `500`.

### `POST /query`

Answers a question about the ingested corpus: embed the question with the same Titan v2 model, retrieve
the 20 nearest chunks from pgvector by vector similarity, rerank those with a cross-encoder and keep
the best 5, then have a Claude model write an answer constrained to them. Returns
`{ answer, sources[] }`, where each source is a chunk that backed the answer. Sources come back in
the reranker's order, most relevant first; the relevance score itself is used internally to order
them and is not part of the response. Generation goes
through the Bedrock Converse API, so the running machine needs the Claude model and Cohere Rerank v3.5
enabled in the region (a one-time Anthropic use-case form per account gates first use of Claude).

```bash
curl -s -X POST localhost:8080/query \
  -H 'Content-Type: application/json' \
  -d '{"question":"Where does pgvector store embeddings?"}'
# { "answer": "pgvector stores embeddings inside Postgres.",
#   "sources": [ { "content": "...", "document_id": "doc-1" } ] }
```

Request body: `{ "question": string }`. The question is required; a malformed body or empty question
returns `400`, and a Bedrock or database failure returns `500`. The `sources[]` array is the contract
that makes an answer auditable and gives a downstream agent structured data instead of prose.

## Writeups

Build notes and explanations for the decisions behind this project are posted on LinkedIn:
[christian-santiago-dev](https://www.linkedin.com/in/christian-santiago-dev/).

## License

Released under the [MIT License](LICENSE).
