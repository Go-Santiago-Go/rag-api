package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-santiago-go/rag-api/internal/store"
)

// topK is how many of the nearest chunks we retrieve to ground an answer. Five is
// a deliberate middle ground: enough context that the answer is well supported,
// few enough that the prompt stays small and cheap. Tunable if answer quality
// warrants; kept as a constant to avoid a magic number at the call site.
const topK = 5

// candidateK is how many chunks are retrieved before reranking. The cross-encoder
// can only reorder what search already found, so this number is the ceiling on
// what reranking can recover: raising it widens the net, and lowering it to topK
// makes the reranker pointless because it would only shuffle the same five
// chunks into a different order, leaving the prompt identical.
//
// Twenty is measured, not guessed. Against the eval corpus the answering passage
// sits in the top 20 far more often than in the top 5, and that gap is exactly
// what the reranker exists to convert.
const candidateK = 20

// Answer is what a query resolves to: the generated prose plus the source chunks
// it was grounded in. Returning the sources, not just the text, is the contract
// that makes this service more than a chatbot: a human (or Project 2's agent)
// can see exactly which passages backed the answer.
type Answer struct {
	Text    string        // the grounded, generated answer
	Sources []store.Match // the chunks the answer was drawn from, for citation
}

// QueryService answers questions about previously ingested documents. Like
// IngestService it depends only on interfaces (embed the question, search the
// store, generate an answer) so tests can drive the whole flow with fakes: no
// Bedrock and no database.
type QueryService struct {
	embedder  Embedder
	store     store.VectorStore
	reranker  Reranker
	generator Generator
}

// NewQueryService wires the query pipeline from its dependencies. The concrete
// Bedrock embedder, pgvector store, and Bedrock generator are injected at main;
// a test injects fakes.
func NewQueryService(embedder Embedder, vs store.VectorStore, reranker Reranker, generator Generator) *QueryService {
	return &QueryService{embedder: embedder, store: vs, reranker: reranker, generator: generator}
}

// Query answers a question about the ingested corpus. It embeds the question with
// the same model used at ingestion (vectors from different models are not
// comparable), then retrieves the topK nearest chunks. Generation is added in the
// next step. Any embed or search failure aborts and is returned wrapped for
// context.
func (s *QueryService) Query(ctx context.Context, question string) (Answer, error) {
	// Embed the question. Must use the same model as ingestion, which is guaranteed
	// by reusing the same injected Embedder; a different model would place the query
	// vector in an incomparable space and make the search meaningless.
	embedding, err := s.embedder.Embed(ctx, question)
	if err != nil {
		return Answer{}, fmt.Errorf("embed question: %w", err)
	}

	// Nearest-neighbour search for the chunks most similar to the question. This
	// retrieves candidateK, not topK: the first stage is fast and approximate, so
	// it is asked for a generous shortlist rather than a final answer.
	matches, err := s.store.Search(ctx, embedding, candidateK)
	if err != nil {
		return Answer{}, fmt.Errorf("search chunks: %w", err)
	}

	// Second stage: score every candidate against the question with a
	// cross-encoder and keep the best topK. Only these reach the prompt and the
	// sources[] response, so the reranker decides what the answer is grounded in.
	matches, err = s.reranker.Rerank(ctx, question, matches, topK)
	if err != nil {
		return Answer{}, fmt.Errorf("rerank chunks: %w", err)
	}

	// Generate: build a grounding prompt from the retrieved chunks and have the
	// model write an answer constrained to them. The same matches are returned as
	// sources so the caller sees exactly which passages backed the answer.
	prompt := buildPrompt(question, matches)
	answer, err := s.generator.Generate(ctx, prompt)
	if err != nil {
		return Answer{}, fmt.Errorf("generate answer: %w", err)
	}
	return Answer{Text: answer, Sources: matches}, nil
}

func buildPrompt(question string, matches []store.Match) string {
	var b strings.Builder
	b.WriteString("Answer using only the context below. If it isn't there, say you don't know.\n\n")
	for _, m := range matches {
		fmt.Fprintf(&b, "[%s] %s\n\n", m.DocumentID, m.Content)
	}
	fmt.Fprintf(&b, "Question: %s\n", question)
	return b.String()
}
