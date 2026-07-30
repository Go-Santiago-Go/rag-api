package store

import (
	"context"
	"os"
	"testing"
)

// embeddingDim must match the vector(1024) column in migrations/001_init.sql;
// inserting a differently sized vector would fail.
const embeddingDim = 1024

// unitVector returns a 1024-long vector that is all zeros except a single 1 at
// position hot. Two unit vectors with different hot positions point in
// different directions, which is all we need to give "nearest" an obvious,
// hand-checkable answer without calling a real embedding model.
func unitVector(hot int) []float32 {
	v := make([]float32, embeddingDim)
	v[hot] = 1
	return v
}

// TestPostgres_SaveAndSearch is an integration test: it proves the round trip
// (Save then Search) against a real pgvector database, since the <=> ranking it
// verifies only exists inside Postgres.
func TestPostgres_SaveAndSearch(t *testing.T) {
	// Self-skip guard: with no database configured, skip rather than fail, so
	// `go test ./...` stays green in CI where there is no Postgres.
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run the store integration test")
	}

	ctx := context.Background()
	pg, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Cleanup runs at test end, pass or fail, so reruns don't accumulate rows.
	// Reaching pg.db directly is why this test lives in package store.
	t.Cleanup(func() {
		pg.db.ExecContext(ctx,
			`DELETE FROM chunks WHERE document_id IN ('smoke-A', 'smoke-B')`)
	})

	// Two chunks pointing in different directions in vector space.
	chunks := []Chunk{
		{DocumentID: "smoke-A", Content: "alpha", Embedding: unitVector(0)},
		{DocumentID: "smoke-B", Content: "beta", Embedding: unitVector(1)},
	}
	if err := pg.Save(ctx, chunks); err != nil {
		t.Fatalf("save: %v", err)
	}

	// Query tilted toward dimension 0: mostly chunk A's direction, only a little
	// of B's. A zero query would have no direction and make cosine distance NaN,
	// so these two assignments are what give the test a deterministic answer.
	query := make([]float32, embeddingDim)
	query[0] = 0.9
	query[1] = 0.1

	matches, err := pg.Search(ctx, query, 2)
	if err != nil {
		t.Fatalf("search: %v", err)
	}

	// Three checks: results came back, the nearest is A, and A's similarity
	// score strictly beats B's. Fatalf stops the test (no point asserting on
	// results we did not get); Errorf records a failure but keeps checking.
	if len(matches) < 2 {
		t.Fatalf("want 2 matches, got %d", len(matches))
	}
	if matches[0].DocumentID != "smoke-A" {
		t.Errorf("want smoke-A nearest, got %s", matches[0].DocumentID)
	}
	if matches[0].Score <= matches[1].Score {
		t.Errorf("nearest score %.3f should beat %.3f", matches[0].Score, matches[1].Score)
	}
}
