package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/go-santiago-go/go-rag-api/internal/handler"
	"github.com/go-santiago-go/go-rag-api/internal/service"
	"github.com/go-santiago-go/go-rag-api/internal/store"
)

// main is the composition root: the one place that builds every concrete
// dependency (Postgres store, Bedrock embedder) and injects them down through
// interfaces. Nothing below main knows which store or embedder it received.
func main() {
	ctx := context.Background()

	// Connection config resolves most-specific-first, so the same binary serves
	// both environments:
	//   1. DATABASE_URL set   -> use it verbatim (local dev / docker-compose).
	//   2. PGHOST set, no URL  -> empty DSN; pgx then reads the standard PG*
	//                             vars (PGHOST/PGUSER/PGPASSWORD/PGDATABASE/
	//                             PGSSLMODE). This is the cloud path: ECS injects
	//                             PGPASSWORD from Secrets Manager, never baking it
	//                             into the image or Terraform state.
	//   3. neither set         -> local default pointing at docker-compose.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" && os.Getenv("PGHOST") == "" {
		dsn = "postgres://postgres:localdev@localhost:5432/go_rag_api?sslmode=disable"
	}
	// NewPostgres pings on startup, so a bad DSN fails here rather than on the
	// first request. A store we cannot reach is fatal: log and exit non-zero.
	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		slog.Error("connect postgres", "err", err)
		os.Exit(1)
	}

	// Apply the schema (idempotent) so a fresh RDS database becomes usable with
	// no separate migration step; a no-op once the extension and table exist.
	// Locally the docker-compose init hook already did this, so it is also a
	// no-op there.
	if err := pg.Migrate(ctx); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}

	// LoadDefaultConfig walks the standard AWS credential chain (env vars, the
	// shared config files, then an IAM role) and resolves the region. That is
	// what lets this binary use local creds on a laptop and the task role on
	// Fargate without any code change.
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		slog.Error("load aws config", "err", err)
		os.Exit(1)
	}
	bedrockClient := bedrockruntime.NewFromConfig(cfg)
	embedder := service.NewBedrockEmbedder(bedrockClient)
	reranker := service.NewBedrockReranker(bedrockClient)
	generator := service.NewBedrockGenerator(bedrockClient)

	// Inject the concrete embedder, store and reranker into the service, which
	// only ever sees the Embedder, VectorStore and Reranker interfaces.
	ingestSvc := service.NewIngestService(embedder, pg)
	querySvc := service.NewQueryService(embedder, pg, reranker, generator)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handler.Health())
	// handler.Ingest closes the service into a route-shaped handler.
	mux.HandleFunc("POST /ingest", handler.Ingest(ingestSvc))
	mux.HandleFunc("POST /query", handler.Query(querySvc))

	slog.Info("listening", "addr", ":8080")
	// ListenAndServe blocks until it fails to serve; a non-nil return means the
	// process can no longer accept requests, so log and exit non-zero.
	if err := http.ListenAndServe(":8080", mux); err != nil {
		slog.Error("server stopped", "err", err)
		os.Exit(1)
	}
}
