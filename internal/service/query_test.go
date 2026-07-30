package service

import (
	"context"
	"errors"
	"testing"

	"github.com/go-santiago-go/go-rag-api/internal/store"
)

// The fakes below are the payoff of dependency inversion: QueryService depends on
// the Embedder, store.VectorStore, and Generator interfaces, so a test injects
// these trivial structs that return canned values. No Bedrock, no Postgres, no
// network. Each fake holds its return values as fields, so a test case steers it
// down a success or failure path just by setting them.

// fakeEmbedder returns a canned vector (or error) instead of calling Bedrock.
// Query never inspects the vector's contents, only whether embedding succeeded,
// so any non-nil slice stands in for a real embedding.
type fakeEmbedder struct {
	vec []float32
	err error
}

func (f fakeEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	return f.vec, f.err
}

// fakeStore satisfies store.VectorStore. Query only calls Search, but the
// interface also requires Save, so it is stubbed to a no-op to satisfy the method
// set.
type fakeStore struct {
	matches []store.Match
	err     error
}

func (f fakeStore) Save(ctx context.Context, chunks []store.Chunk) error {
	return nil
}

func (f fakeStore) Search(ctx context.Context, embedding []float32, topK int) ([]store.Match, error) {
	return f.matches, f.err
}

// fakeGenerator returns a canned answer (or error) instead of calling an LLM.
type fakeGenerator struct {
	answer string
	err    error
}

func (f fakeGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	return f.answer, f.err
}

// Compile-time proof each fake still satisfies its interface. If a fake's method
// set ever drifts from the interface (a renamed param is fine, a changed type is
// not), the build fails here instead of the fake silently ceasing to be usable.
var (
	_ Embedder          = fakeEmbedder{}
	_ store.VectorStore = fakeStore{}
	_ Generator         = fakeGenerator{}
)

func TestQueryService_Query(t *testing.T) {
	// matches is what the store "returns"; the happy path must hand these back
	// unchanged as the answer's sources.
	matches := []store.Match{
		{Content: "pgvector stores embeddings inside Postgres.", DocumentID: "doc-1"},
		{Content: "It ships as a Postgres extension.", DocumentID: "doc-1"},
	}

	// Sentinels so each error case asserts the failure came from the dependency it
	// expected, not from elsewhere in the pipeline.
	errEmbed := errors.New("embed boom")
	errSearch := errors.New("search boom")
	errGen := errors.New("generate boom")

	tests := []struct {
		name      string
		embedder  fakeEmbedder
		store     fakeStore
		generator fakeGenerator
		wantErr   error // sentinel we expect wrapped, or nil for the happy path
	}{
		{
			name:      "happy path returns answer and sources",
			embedder:  fakeEmbedder{vec: []float32{0.1, 0.2}},
			store:     fakeStore{matches: matches},
			generator: fakeGenerator{answer: "pgvector stores embeddings inside Postgres."},
		},
		{
			name:     "embed failure aborts",
			embedder: fakeEmbedder{err: errEmbed},
			wantErr:  errEmbed,
		},
		{
			name:     "search failure aborts",
			embedder: fakeEmbedder{vec: []float32{0.1, 0.2}},
			store:    fakeStore{err: errSearch},
			wantErr:  errSearch,
		},
		{
			name:      "generate failure aborts",
			embedder:  fakeEmbedder{vec: []float32{0.1, 0.2}},
			store:     fakeStore{matches: matches},
			generator: fakeGenerator{err: errGen},
			wantErr:   errGen,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewQueryService(tt.embedder, tt.store, tt.generator)
			got, err := svc.Query(context.Background(), "where does pgvector store embeddings?")

			// Error paths: Query must surface the originating error. errors.Is walks
			// the %w wrap chain, so it matches even though Query adds context.
			if tt.wantErr != nil {
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("want error wrapping %v, got %v", tt.wantErr, err)
				}
				return
			}

			// Happy path: no error, and the pieces are wired straight through.
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Text != tt.generator.answer {
				t.Errorf("answer = %q, want %q", got.Text, tt.generator.answer)
			}
			// Sources must be exactly what the store returned: Query cites the chunks
			// it retrieved, unmodified. This is the { answer, sources[] } contract.
			if len(got.Sources) != len(matches) {
				t.Fatalf("want %d sources, got %d", len(matches), len(got.Sources))
			}
			for i, want := range matches {
				if got.Sources[i] != want {
					t.Errorf("source[%d] = %+v, want %+v", i, got.Sources[i], want)
				}
			}
		})
	}
}
