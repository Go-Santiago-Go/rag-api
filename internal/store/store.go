// Package store defines what the service needs from a vector database and (in a
// sibling file) provides a pgvector-backed implementation. The interface lives
// here, next to its consumer's needs; concrete backends are plugged in at main.
// This package is the only place that knows SQL.
package store

import "context"

// Chunk is one passage of a document paired with the vector that encodes its
// meaning. It is the unit written during ingestion: the text, its citation
// metadata, and the embedding produced for it.
type Chunk struct {
	DocumentID string    // which source document this chunk came from
	Content    string    // the chunk text itself, returned later as a citation
	Embedding  []float32 // the chunk's embedding; same model/dimension as queries
}

// Match is one search hit: a stored chunk judged similar to a query embedding.
// It carries a Score, which a Chunk does not, because similarity only exists
// after a search has compared a stored vector to the query.
type Match struct {
	Content    string  // the matched chunk text, ready to drop into sources[]
	DocumentID string  // source document, for the citation
	Score      float32 // similarity to the query; higher is closer
}

// VectorStore is the contract the service depends on: persist chunks, and find
// the chunks most similar to a query embedding. pgvector is one implementation;
// a fake satisfies it in tests with no database. Defining the interface here,
// in plain Go types, is the dependency inversion that keeps RAG logic unaware of
// the concrete store.
type VectorStore interface {
	// Save persists a batch of embedded chunks. It takes a slice so a whole
	// document can be written in one call rather than one round trip per chunk.
	Save(ctx context.Context, chunks []Chunk) error
	// Search returns the topK chunks most similar to embedding, nearest first.
	Search(ctx context.Context, embedding []float32, topK int) ([]Match, error)
}
