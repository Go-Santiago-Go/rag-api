package main

import (
	"cmp"
	"context"
	"log/slog"
	"net/http"
	"os"
	"strconv"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/go-santiago-go/rag-api/internal/handler"
	"github.com/go-santiago-go/rag-api/internal/service"
	"github.com/go-santiago-go/rag-api/internal/store"
)

// Default chunking parameters, all three chosen from measured evidence rather
// than intuition. See eval/README.md for the comparison.
//
// 800 runes over the original 500 is the single largest measured gain in the
// project: +11.4 points of passage recall@5 under the production retrieval path.
// Structure-aware splitting adds little recall on top of that, but it cuts
// severed chunk boundaries from 66% to 4.8% (chunks whose body starts
// mid-sentence, against fixed-size at the same ceiling), and this service returns
// its chunks verbatim as sources[]. A citation is a user-facing artifact, so a
// passage that begins mid-word is a defect the harness cannot see.
const (
	defaultChunkStrategy = "structured"
	defaultChunkSize     = 800
	defaultChunkOverlap  = 80
)

// newChunker builds the chunking strategy from the environment.
//
// This is configuration rather than a knob for its own sake: comparing two
// chunking strategies requires re-ingesting the whole corpus under each one, and
// the evaluation harness drives that through the real /ingest endpoint. Without
// this, every measurement would need a code edit and a rebuild, which is exactly
// how two runs stop being comparable. Defaults reproduce the original behaviour.
func newChunker() service.Chunker {
	size := envInt("CHUNK_SIZE", defaultChunkSize)
	overlap := envInt("CHUNK_OVERLAP", defaultChunkOverlap)
	strategy := cmp.Or(os.Getenv("CHUNK_STRATEGY"), defaultChunkStrategy)

	if strategy != "fixed" && strategy != "structured" {
		slog.Warn("unknown chunk strategy", "value", strategy, "using", defaultChunkStrategy)
		strategy = defaultChunkStrategy
	}

	// Logged after validation so a set of eval numbers can always be traced back
	// to the configuration that actually produced them, not the one requested.
	slog.Info("chunking", "strategy", strategy, "size", size, "overlap", overlap)

	if strategy == "fixed" {
		return service.NewFixedChunker(size, overlap)
	}
	return service.NewStructuredChunker(size, overlap)
}

// envInt reads a positive integer from the environment, falling back to a
// default. An unparseable or non-positive value is loud rather than silent: a
// typo that quietly reverted chunk size to the default would invalidate a
// measurement without anything looking wrong.
func envInt(key string, fallback int) int {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		slog.Warn("ignoring invalid value", "key", key, "value", raw, "using", fallback)
		return fallback
	}
	return n
}

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

	// Inject the concrete embedder, store, chunker and reranker into the service,
	// which only ever sees the Embedder, VectorStore, Chunker and Reranker
	// interfaces.
	ingestSvc := service.NewIngestService(embedder, newChunker(), pg)
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
