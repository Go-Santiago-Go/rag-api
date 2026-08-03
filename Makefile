# Task runner. The verbs (help, up, down, run, test, lint, deploy, destroy) are
# the same in every repo in this portfolio, so a reviewer who has seen one knows
# what to type here regardless of the language underneath.

# Load a local .env if present, so `make run` picks up overrides without the
# caller having to export anything. Committed .env.example documents the keys;
# .env itself is gitignored. `-include` keeps this optional.
-include .env
export

.DEFAULT_GOAL := help

.PHONY: help up down run build test lint eval-load eval-recall eval-judge \
        docker-build bootstrap deploy url destroy

help: ## List the available targets
	@grep -hE '^[a-z][a-zA-Z0-9_-]*:.*?## ' $(MAKEFILE_LIST) \
	  | awk -F':.*?## ' '{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# --- Local -------------------------------------------------------------------

up: ## Start local Postgres + pgvector
	docker compose up -d

down: ## Stop local Postgres (add VOLUMES=1 to also drop the data)
	docker compose down $(if $(VOLUMES),-v,)

run: ## Run the service on :8080
	go run ./cmd/server

build: ## Compile everything
	go build ./...

test: ## Run the test suite (no database, no cloud access needed)
	go test ./...

lint: ## Vet and formatting check, matching CI
	go vet ./...
	@unformatted=$$(gofmt -l .); \
	if [ -n "$$unformatted" ]; then \
	  echo "gofmt needed:"; echo "$$unformatted"; exit 1; \
	fi

# --- Evaluation --------------------------------------------------------------
# Repo-specific. These need the service running (make up, then make run) because
# the corpus is loaded through the real /ingest endpoint rather than a shortcut.

eval-load: ## Ingest the pinned eval corpus through POST /ingest
	go run ./eval/cmd/load

eval-recall: ## Measure recall@k on the labelled questions
	go run ./eval/cmd/run -rerank -paraphrase

eval-judge: ## Validate the judge, then grade answer quality
	go run ./eval/cmd/judge -validate
	go run ./eval/cmd/judge -paraphrase

# --- Cloud -------------------------------------------------------------------
# Two stacks split by lifetime: bootstrap is free and stays up, infra bills by
# the hour and is destroyed after each session.

docker-build: ## Build the distroless container image
	docker build -t rag-api:local .

bootstrap: ## Apply the persistent stack (ECR repo + CI role). Run once.
	cd infra/bootstrap && terraform init && terraform apply

deploy: ## Apply the billable app stack (VPC, RDS, ECS Express)
	cd infra && terraform init && terraform apply

url: ## Print the live service URL
	@cd infra && terraform output -raw service_url

destroy: ## Tear the billable stack down. Run this after every session.
	cd infra && terraform destroy
