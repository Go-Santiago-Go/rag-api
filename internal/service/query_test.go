package service

import (
	"context"
	"errors"
	"testing"

	"github.com/go-santiago-go/rag-api/internal/store"
)

// The fakes below are the payoff of dependency inversion: QueryService depends on
// the Embedder, store.VectorStore, Reranker, and Generator interfaces, so a test
// injects these trivial structs that return canned values. No Bedrock, no
// Postgres, no network. Each fake holds its return values as fields, so a test
// case steers it down a success or failure path just by setting them.

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
// set. It records the requested depth: over-retrieval is the mechanism that makes
// reranking worth anything, so it is worth asserting rather than assuming.
type fakeStore struct {
	matches []store.Match
	err     error

	gotTopK int // depth Search was asked for
}

func (f *fakeStore) Save(ctx context.Context, chunks []store.Chunk) error {
	return nil
}

func (f *fakeStore) Search(ctx context.Context, embedding []float32, topK int) ([]store.Match, error) {
	f.gotTopK = topK
	return f.matches, f.err
}

// fakeReranker returns a canned ordering (or error) instead of calling a
// cross-encoder. Tests set matches to something the store did not return, which
// is how they prove the reranker's output is what reaches sources[].
type fakeReranker struct {
	matches []store.Match
	err     error

	gotQuery string // question the reranker was scored against
	gotTopN  int    // number of survivors it was asked for
}

func (f *fakeReranker) Rerank(ctx context.Context, query string, matches []store.Match, topN int) ([]store.Match, error) {
	f.gotQuery, f.gotTopN = query, topN
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
	_ store.VectorStore = (*fakeStore)(nil)
	_ Reranker          = (*fakeReranker)(nil)
	_ Generator         = fakeGenerator{}
)

func TestQueryService_Query(t *testing.T) {
	// retrieved is what the store "returns", reranked is what the cross-encoder
	// promotes out of it. They differ in order so the happy path can prove which
	// one reaches the caller.
	retrieved := []store.Match{
		{Content: "pgvector stores embeddings inside Postgres.", DocumentID: "doc-1", Score: 0.81},
		{Content: "It ships as a Postgres extension.", DocumentID: "doc-1", Score: 0.79},
	}
	reranked := []store.Match{
		{Content: "It ships as a Postgres extension.", DocumentID: "doc-1", Score: 0.94},
		{Content: "pgvector stores embeddings inside Postgres.", DocumentID: "doc-1", Score: 0.12},
	}

	// Sentinels so each error case asserts the failure came from the dependency it
	// expected, not from elsewhere in the pipeline.
	errEmbed := errors.New("embed boom")
	errSearch := errors.New("search boom")
	errRerank := errors.New("rerank boom")
	errGen := errors.New("generate boom")

	tests := []struct {
		name      string
		embedder  fakeEmbedder
		store     *fakeStore
		reranker  *fakeReranker
		generator fakeGenerator
		wantErr   error // sentinel we expect wrapped, or nil for the happy path
	}{
		{
			name:      "happy path returns answer and reranked sources",
			embedder:  fakeEmbedder{vec: []float32{0.1, 0.2}},
			store:     &fakeStore{matches: retrieved},
			reranker:  &fakeReranker{matches: reranked},
			generator: fakeGenerator{answer: "pgvector stores embeddings inside Postgres."},
		},
		{
			name:     "embed failure aborts",
			embedder: fakeEmbedder{err: errEmbed},
			store:    &fakeStore{},
			reranker: &fakeReranker{},
			wantErr:  errEmbed,
		},
		{
			name:     "search failure aborts",
			embedder: fakeEmbedder{vec: []float32{0.1, 0.2}},
			store:    &fakeStore{err: errSearch},
			reranker: &fakeReranker{},
			wantErr:  errSearch,
		},
		{
			name:     "rerank failure aborts",
			embedder: fakeEmbedder{vec: []float32{0.1, 0.2}},
			store:    &fakeStore{matches: retrieved},
			reranker: &fakeReranker{err: errRerank},
			wantErr:  errRerank,
		},
		{
			name:      "generate failure aborts",
			embedder:  fakeEmbedder{vec: []float32{0.1, 0.2}},
			store:     &fakeStore{matches: retrieved},
			reranker:  &fakeReranker{matches: reranked},
			generator: fakeGenerator{err: errGen},
			wantErr:   errGen,
		},
	}

	const question = "where does pgvector store embeddings?"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewQueryService(tt.embedder, tt.store, tt.reranker, tt.generator)
			got, err := svc.Query(context.Background(), question)

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
			// Sources must be the reranker's ordering, not the store's. This is the
			// { answer, sources[] } contract: the citations shown to the caller are
			// the passages that actually grounded the answer, in the order the
			// cross-encoder judged them.
			if len(got.Sources) != len(reranked) {
				t.Fatalf("want %d sources, got %d", len(reranked), len(got.Sources))
			}
			for i, want := range reranked {
				if got.Sources[i] != want {
					t.Errorf("source[%d] = %+v, want %+v", i, got.Sources[i], want)
				}
			}
		})
	}
}

// TestQueryService_OverRetrievesBeforeReranking pins the two-stage shape of the
// pipeline. A reranker that only ever sees the same topK the caller receives
// cannot change the prompt at all, since reordering five chunks still yields
// those five chunks. Retrieving deeper than topK is what gives it something to
// promote, so the relationship candidateK > topK is a correctness property of the
// design and not a tuning knob.
func TestQueryService_OverRetrievesBeforeReranking(t *testing.T) {
	if candidateK <= topK {
		t.Fatalf("candidateK (%d) must exceed topK (%d) for reranking to do anything", candidateK, topK)
	}

	vs := &fakeStore{matches: []store.Match{{Content: "a", DocumentID: "doc-1"}}}
	rr := &fakeReranker{matches: []store.Match{{Content: "a", DocumentID: "doc-1"}}}
	svc := NewQueryService(fakeEmbedder{vec: []float32{0.1}}, vs, rr, fakeGenerator{answer: "ok"})

	const question = "how deep does the first stage search?"
	if _, err := svc.Query(context.Background(), question); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if vs.gotTopK != candidateK {
		t.Errorf("store searched at depth %d, want candidateK %d", vs.gotTopK, candidateK)
	}
	if rr.gotTopN != topK {
		t.Errorf("reranker asked for %d survivors, want topK %d", rr.gotTopN, topK)
	}
	// The cross-encoder scores the question against each passage, so it must
	// receive the question itself rather than the embedding or the prompt.
	if rr.gotQuery != question {
		t.Errorf("reranker scored against %q, want %q", rr.gotQuery, question)
	}
}
