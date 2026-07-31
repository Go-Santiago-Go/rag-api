# Deploying rag-api to AWS

This walks through cloning the repo and standing up the whole service on AWS behind a public HTTPS
URL: a containerized Go RAG service, pgvector in RDS, and Amazon Bedrock for embeddings and
generation, all provisioned with Terraform. At the end you get a `*.ecs.<region>.on.aws` URL that
answers questions with grounded citations.

Two warnings before you start. First, this creates **billable** resources (RDS, a NAT gateway, an
Express Mode load balancer). It is a few cents to about a dollar for a short session, but you must
tear it down when done. Second, the local path (see the README quickstart) is free and proves the
same loop, so deploy to AWS only when you actually want the cloud demo.

## Prerequisites

- An AWS account, with the [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html) configured (`aws configure`).
- [Terraform](https://developer.hashicorp.com/terraform/install) and [Docker](https://docs.docker.com/get-docker/).
- **Bedrock model access.** In the Bedrock console, request access to **Titan Text Embeddings V2**
  and a **Claude** model in your region. Bedrock is opt in per account, and the service fails with
  `AccessDenied` until this is granted. See [Manage model access](https://docs.aws.amazon.com/bedrock/latest/userguide/model-access.html).
- Optional but wise: a [budget alert](https://docs.aws.amazon.com/cost-management/latest/userguide/budgets-create.html)
  at a few dollars so nothing surprises you.

## Step 1: Clone

```bash
git clone https://github.com/Go-Santiago-Go/rag-api.git
cd rag-api
```

## Step 2: Apply the persistent stack (free)

The infrastructure is split into two Terraform stacks by lifetime. The **bootstrap** stack holds the
free, long lived pieces: the ECR container registry and the GitHub OIDC role CI uses to push images.
Apply it once and leave it up.

```bash
cd infra/bootstrap
terraform init
terraform apply    # creates the ECR repo and the CI role; both are free
cd ../..
```

## Step 3: Get an image into ECR

The app stack deploys an image by tag, so ECR needs one before you deploy. You have two ways.

**Option A, build and push locally.**

```bash
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
REGION=us-east-1
REPO="$ACCOUNT.dkr.ecr.$REGION.amazonaws.com/go-rag-api"

docker build -t "$REPO:latest" .
aws ecr get-login-password --region $REGION \
  | docker login --username AWS --password-stdin "$ACCOUNT.dkr.ecr.$REGION.amazonaws.com"
docker push "$REPO:latest"
```

**Option B, let CI push it.** In the GitHub repo, add a repository variable `AWS_ROLE_ARN` (Settings,
then Secrets and variables, then Actions, then Variables) set to the CI role ARN from Step 2. Then
merge to `main`, and the `deploy` workflow builds the image and pushes it to ECR with no stored AWS
keys, using OIDC.

## Step 4: Apply the app stack

This provisions the VPC, RDS Postgres with pgvector, S3, and the ECS Express Mode service, and waits
until the service is healthy. Budget about 10 to 15 minutes: RDS alone takes roughly 5, then Express
Mode waits for health checks.

```bash
cd infra
terraform init
terraform apply
terraform output service_url    # your live public URL
```

## Step 5: Test it

```bash
URL=$(terraform output -raw service_url)

# health
curl "$URL/health"        # {"status":"ok"}

# ingest a document, then ask about it
curl -X POST "$URL/ingest" -H 'Content-Type: application/json' \
  -d '{"document_id":"doc-1","text":"Acme offers a full refund within 30 days of purchase."}'

curl -s -X POST "$URL/query" -H 'Content-Type: application/json' \
  -d '{"question":"What is the refund window?"}'
# { "answer": "...30 days...", "sources": [ ... ] }
```

## Step 6: Tear it down

```bash
cd infra
terraform destroy    # removes the billable app stack (VPC, RDS, S3, ECS)
```

Leave the bootstrap stack up: ECR and the CI role are free, and keeping them means CI can push images
at any time and your pushed image survives for the next deploy. If you want everything gone,
`terraform destroy` in `infra/bootstrap` too.

## Troubleshooting

- **Tasks crash loop, `AccessDenied` from Bedrock.** Model access is not granted, or you are in a
  region without it. Grant Titan v2 and Claude in the region you deployed to.
- **Tasks crash loop, connecting to `localhost`.** The image in ECR is stale, older than the code
  that reads the `PG*` env vars. Rebuild, re-push `:latest`, and force a new deployment
  (`aws ecs update-service --cluster default --service go-rag-api --force-new-deployment`).
- **The URL is not reachable and its `access_type` is `PRIVATE`.** The service was placed in private
  subnets. It must be in the public subnets for an internet facing load balancer (this repo already
  does that in `infra/ecs.tf`).
- **`apply` errors that a resource already exists.** An earlier run left something behind. Reconcile
  with `terraform plan`, or import or delete the stray resource.
