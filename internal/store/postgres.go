package store

import (
	"context"
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" driver with database/sql
	"github.com/pgvector/pgvector-go"
)

// Postgres is the pgvector-backed VectorStore. It is the only type in the
// codebase that knows SQL and the vector column type.
type Postgres struct {
	db *sql.DB
}

// Compile-time assertion that *Postgres satisfies VectorStore. It costs nothing
// at runtime; its only job is to fail the build, naming the missing method, if
// the implementation ever drifts from the interface.
var _ VectorStore = (*Postgres)(nil)

// NewPostgres opens a connection pool to the given DSN and returns a ready
// store. It returns the concrete *Postgres so callers keep full access; those
// that want the abstraction accept the VectorStore interface instead.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}

	// sql.Open is lazy and never actually connects. Ping forces a real
	// connection now, so a bad DSN fails at startup rather than on first query.
	if err := db.PingContext(ctx); err != nil {
		return nil, err
	}
	return &Postgres{db: db}, nil
}

// Save inserts each chunk as a row. One INSERT per chunk is intentionally simple
// for the MVP; batching into a multi-row INSERT or CopyFrom is the later
// optimization, not needed while ingesting one document at a time.
func (s *Postgres) Save(ctx context.Context, chunks []Chunk) error {
	// $1..$3 are placeholders, not string concatenation: the driver sends SQL
	// and values separately, so document text can never be parsed as SQL.
	const q = `INSERT INTO chunks (document_id, content, embedding)
			   VALUES ($1, $2, $3)`
	for _, c := range chunks {
		// ExecContext (not Exec) threads ctx to the driver, so a cancelled
		// request cancels the in-flight statement. NewVector adapts the Go
		// []float32 into the value a vector(1024) column expects.
		if _, err := s.db.ExecContext(ctx, q,
			c.DocumentID,
			c.Content,
			pgvector.NewVector(c.Embedding),
		); err != nil {
			return err
		}
	}
	return nil
}

// Search returns the topk chunks most similar to the query embedding, nearest
// first. This query is the retrieval half of RAG.
func (s *Postgres) Search(ctx context.Context, embedding []float32, topk int) ([]Match, error) {
	// <=> is pgvector's cosine-distance operator. Ordering by it ascending puts
	// the closest vectors first; LIMIT keeps the top k. 1 - distance turns the
	// distance into a 0..1 similarity score for the caller.
	const q = `SELECT content, document_id, 1-(embedding <=> $1) AS score
			   FROM chunks
			   ORDER BY embedding <=> $1
			   LIMIT $2`
	rows, err := s.db.QueryContext(ctx, q, pgvector.NewVector(embedding), topk)
	if err != nil {
		return nil, err
	}
	// rows holds a connection until closed; defer guarantees release even on an
	// early return, which prevents leaking connections from the pool.
	defer rows.Close()

	var matches []Match
	for rows.Next() {
		var m Match
		// Scan argument order must match the SELECT column order above.
		if err := rows.Scan(&m.Content, &m.DocumentID, &m.Score); err != nil {
			return nil, err
		}
		matches = append(matches, m)
	}

	// rows.Err surfaces an error that ended iteration early (e.g. a dropped
	// connection mid-read), which rows.Next returning false would otherwise hide.
	return matches, rows.Err()
}
