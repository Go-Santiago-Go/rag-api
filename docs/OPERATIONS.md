# Operations

Running the service, checking that it actually works, what it costs while it does, and what breaks.

Most of what follows was hit for real during deployment. The pattern worth internalizing: **read what
the system actually reported before theorizing about config.** Every entry below has a command that
turns a guess into an observation.

## Running the stack

Locally, two commands and no cloud resources:

```bash
make up      # Postgres + pgvector in Docker; schema auto-applies on first boot
make run     # the service on :8080
make down    # stop it (add VOLUMES=1 to also drop the data)
```

On AWS, the billable stack and the URL it hands back:

```bash
make deploy  # terraform apply in infra/ (about 10 to 15 minutes; RDS is most of it)
make url     # print the live service URL
make destroy # tear it down. Run this after every session.
```

`make help` lists the rest. The full deploy walkthrough, including the one-time bootstrap stack, is in
[DEPLOYMENT.md](DEPLOYMENT.md).

## Verifying a deploy is actually healthy

```bash
URL=$(cd infra && terraform output -raw service_url)

curl "$URL/health"        # {"status": "ok"}

curl -X POST "$URL/ingest" -H 'Content-Type: application/json' \
  -d '{"document_id":"doc-1","text":"Acme offers a full refund within 30 days of purchase."}'

curl -s -X POST "$URL/query" -H 'Content-Type: application/json' \
  -d '{"question":"What is the refund window?"}'
```

`/health` passing only proves the container started. It does not touch Bedrock or run a query, so a
green health check coexists with missing model access. The `/ingest` then `/query` pair is the real
check, because it exercises embeddings, pgvector, reranking, and generation in one pass.

## Cost

The app stack bills by the hour while it is up. The three meters are the RDS instance, the NAT
gateway, and the Express Mode load balancer.

A short session is a few cents to about a dollar. `terraform destroy` in `infra/` after each session
keeps the bill at pennies. The bootstrap stack (ECR, the CI role) is free and is meant to stay up.

There is no autoscale-to-zero here. If you forget the destroy, it bills overnight. Set a
[budget alert](https://docs.aws.amazon.com/cost-management/latest/userguide/budgets-create.html) at a
few dollars.

## Deploy-time failure modes

### Tasks crash loop with `AccessDenied` from Bedrock

Model access is not granted, or you deployed to a region without it. Bedrock is opt in per account.
Grant Titan Text Embeddings V2, Cohere Rerank v3.5, and a Claude model in the region you deployed to.

First use of Claude is additionally gated by a one-time Anthropic use-case form per account.

### Tasks crash loop trying to connect to `localhost`

**This is the classic one, and the cause is almost never the config.** The image in ECR is stale:
older than the code that reads the `PG*` vars, so it falls through to the local default
(`cmd/server/main.go:96`).

Rebuild and re-push `:latest` before concluding anything about configuration. CI tags images by git
SHA alongside `:latest` (`.github/workflows/deploy.yml:43`), so you can check which commit is
actually running rather than assuming `:latest` is current:

```bash
aws ecr describe-images --repository-name go-rag-api \
  --query 'sort_by(imageDetails,&imagePushedAt)[-1].[imageTags,imagePushedAt]'
```

Forcing the service to pick up a new `:latest` is done through Express Mode, not the classic
`aws ecs update-service` path: this stack has no `aws_ecs_cluster` resource, because
`aws_ecs_express_gateway_service` (`infra/ecs.tf:81`) manages its own. `terraform apply` after the
push is the reliable way to roll the service.

### The URL is unreachable and `access_type` is `PRIVATE`

The service landed in private subnets. Express Mode needs the public subnets for an
internet-facing load balancer, because it uses a single subnet set for both the load balancer and the
tasks. This repo already places them correctly in `infra/ecs.tf`; see the trade-off note in
[DEPLOYMENT.md](DEPLOYMENT.md#three-constraints-worth-knowing-up-front).

### `apply` errors that a resource already exists

An earlier run left something behind. Reconcile with `terraform plan`, then import or delete the
stray resource.

## CI cannot assume its role

Symptom: the deploy workflow fails with `Not authorized to perform sts:AssumeRoleWithWebIdentity`.

Do not guess at the trust policy. Read the claim AWS actually received:

```bash
aws cloudtrail lookup-events \
  --lookup-attributes AttributeKey=EventName,AttributeValue=AssumeRoleWithWebIdentity \
  --max-results 1 --query 'Events[].CloudTrailEvent' --output text
```

The `sub` claim carries immutable numeric IDs rather than names:

```
repo:Go-Santiago-Go@85260356/rag-api@1281557182:ref:refs/heads/main
```

`infra/bootstrap` matches on those IDs and wildcards the names, so renaming the org or the repo
cannot break CD. If you fork this repo, those IDs are yours to change.

## Teardown hangs on the Express service or the internet gateway

`terraform destroy` can stall. The cause is an orphan: Express Mode deletes its own gateway resource
but leaves its ALB behind, and that ALB's ENIs hold public IPs which block the internet gateway, and
therefore the whole VPC, from deleting.

The fix is manual, then re-run destroy:

1. Delete the orphaned load balancer, named `ecs-express-gateway-alb-*`.
2. Delete its target groups, named `ecs-gateway-tg-*`.
3. `terraform destroy` again.

**Do not pipe destroy output through `tee` and `tail`.** The pipeline returns `tail`'s exit code, not
Terraform's, so a failed destroy looks like a successful one and you find out when the bill arrives.

```bash
terraform destroy 2>&1 | tee destroy.log   # exit code is tee's, check the log
echo "${PIPESTATUS[0]}"                    # Terraform's actual exit code
```

## Honest constraints

- **Observability stops at structured logs.** The service emits `log/slog` lines and nothing else: no
  metrics endpoint, no dashboard, no tracing. "What is p95 query latency right now" cannot be answered
  without parsing logs, which is slowest exactly during an incident. Adequate for a service with two
  endpoints and one task; the first thing to add if traffic were real.
- **`make destroy` destroys the data.** `skip_final_snapshot = true` (`infra/rds.tf:97`) and no backup
  retention is configured, so tearing the stack down discards the ingested corpus with no snapshot to
  restore from. That is deliberate for a demo whose corpus is re-ingestible from `eval/` in one
  command, and it would be indefensible for anything holding data you cannot regenerate.
- **`/health` is liveness only.** It touches neither Bedrock nor the database, so a green health check
  coexists with missing model access, an unreachable RDS instance, and an empty corpus. The load
  balancer is the only intended consumer. The real check is the `/ingest` then `/query` pair above.
- **There is no authentication.** Anyone who has the URL can ingest documents and query them, and
  ingest costs money per call. Acceptable only because the stack is destroyed after each session.
- **No scale-to-zero.** RDS, the NAT gateway, and the Express Mode load balancer bill by the hour
  whether or not a request arrives. There is no idle state that costs nothing except `destroy`.
- **Ingest is synchronous and unbatched**, one Bedrock embedding call per chunk in sequence, so
  loading the 45 document eval corpus holds a connection open for minutes. It is the slowest thing the
  service does and the first thing that would change under real load.
- **Rate limiting, retries, and backpressure do not exist here.** A burst of `/ingest` calls will
  happily saturate the Bedrock quota and surface as `500`s. That layer is a separate concern and a
  separate project.
