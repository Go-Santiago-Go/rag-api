package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/go-santiago-go/go-rag-api/internal/store"
)

// topK is how many of the nearest chunks we retrieve to ground an answer. Five is
// a deliberate middle ground: enough context that the answer is well supported,
// few enough that the prompt stays small and cheap. Tunable if answer quality
// warrants; kept as a constant to avoid a magic number at the call site.
const topK = 5

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
	generator Generator
}

// NewQueryService wires the query pipeline from its dependencies. The concrete
// Bedrock embedder, pgvector store, and Bedrock generator are injected at main;
// a test injects fakes.
func NewQueryService(embedder Embedder, vs store.VectorStore, generator Generator) *QueryService {
	return &QueryService{embedder: embedder, store: vs, generator: generator}
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

	// Nearest-neighbour search for the chunks most similar to the question.
	matches, err := s.store.Search(ctx, embedding, topK)
	if err != nil {
		return Answer{}, fmt.Errorf("search chunks: %w", err)
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
