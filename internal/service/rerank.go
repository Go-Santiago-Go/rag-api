package service

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/go-santiago-go/go-rag-api/internal/store"
)

// rerankModelID is the cross-encoder used to reorder retrieved candidates.
//
// Cohere Rerank is reachable two ways on Bedrock: the standalone Rerank API on
// the bedrock-agent-runtime endpoint, or InvokeModel on the runtime client this
// service already holds. InvokeModel is used here because it needs no extra SDK
// module, no second client, and no new IAM action. The cost is that the request
// body below is Cohere-shaped rather than provider-neutral, which is contained
// to this file by the Reranker interface.
const rerankModelID = "cohere.rerank-v3-5:0"

// Reranker reorders retrieved candidates by how well each one answers the query.
//
// This is a cross-encoder: it reads the query and a passage together and scores
// that pair. The embedder is a bi-encoder, which encodes the query and each
// passage independently, so a chunk's vector must summarise it for every
// question that could ever be asked. That is what makes embeddings precomputable
// and searchable at corpus scale, and also what makes them imprecise. A
// cross-encoder is the reverse trade: much better judgement, and nothing can be
// precomputed because there is no pair until the question arrives.
//
// Which is why reranking cannot replace search, only follow it. Scoring all
// 1,126 chunks per query is not affordable; scoring the 20 that search already
// surfaced is.
type Reranker interface {
	// Rerank returns at most topN of matches, ordered most relevant first. It
	// must not invent, alter or duplicate matches: every returned element is one
	// of the inputs, with its Score replaced.
	Rerank(ctx context.Context, query string, matches []store.Match, topN int) ([]store.Match, error)
}

// BedrockReranker implements Reranker using Cohere Rerank on Amazon Bedrock. It
// holds the same Bedrock Runtime client as the embedder and generator; all
// Cohere-specific request and response knowledge lives in this file.
type BedrockReranker struct {
	client *bedrockruntime.Client
}

// NewBedrockReranker returns a Reranker backed by a configured Bedrock Runtime
// client, injected at main alongside the embedder and generator.
func NewBedrockReranker(client *bedrockruntime.Client) *BedrockReranker {
	return &BedrockReranker{client: client}
}

// Compile-time proof BedrockReranker satisfies Reranker; fails the build here if
// the method set ever drifts from the interface.
var _ Reranker = (*BedrockReranker)(nil)

// cohereRerankRequest is the JSON body Cohere Rerank expects. APIVersion is
// required and rejected if absent, and documents are plain strings addressed by
// position in the response.
type cohereRerankRequest struct {
	APIVersion int      `json:"api_version"`
	Query      string   `json:"query"`
	Documents  []string `json:"documents"`
	TopN       int      `json:"top_n"`
}

// cohereRerankResponse is the JSON body Cohere Rerank returns: the surviving
// documents in relevance order, identified by their index in the request rather
// than echoed back as text.
type cohereRerankResponse struct {
	Results []struct {
		Index          int     `json:"index"`
		RelevanceScore float32 `json:"relevance_score"`
	} `json:"results"`
}

// Rerank scores each candidate against the query with the cross-encoder and
// returns the best topN, most relevant first.
//
// The returned Score is the model's relevance judgement, not the cosine
// similarity the store produced. The two are not comparable to each other and
// should never be mixed in one ordering, but both obey Match.Score's contract
// that higher means closer, so callers and the sources[] response need no
// special case.
func (r *BedrockReranker) Rerank(ctx context.Context, query string, matches []store.Match, topN int) ([]store.Match, error) {
	// Nothing retrieved means nothing to reorder. Returning early also avoids
	// sending an empty document list, which the model rejects.
	if len(matches) == 0 {
		return matches, nil
	}
	// Asking for more results than there are candidates is a validation error,
	// and it happens naturally on a small corpus or a narrow search.
	if topN > len(matches) {
		topN = len(matches)
	}

	documents := make([]string, len(matches))
	for i, m := range matches {
		documents[i] = m.Content
	}

	body, err := json.Marshal(cohereRerankRequest{
		APIVersion: 2,
		Query:      query,
		Documents:  documents,
		TopN:       topN,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal rerank request: %w", err)
	}

	out, err := r.client.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(rerankModelID),
		ContentType: aws.String("application/json"),
		Body:        body,
	})
	if err != nil {
		return nil, fmt.Errorf("invoke rerank model: %w", err)
	}

	var resp cohereRerankResponse
	if err := json.Unmarshal(out.Body, &resp); err != nil {
		return nil, fmt.Errorf("unmarshal rerank response: %w", err)
	}
	if len(resp.Results) == 0 {
		return nil, fmt.Errorf("empty rerank response for %d candidates", len(matches))
	}

	ranked := make([]store.Match, 0, len(resp.Results))
	for _, result := range resp.Results {
		// Results address candidates by position, so an out-of-range index would
		// pair one passage's text with another's score and silently corrupt the
		// citations. Fail loudly instead: a wrong source is worse than an error.
		if result.Index < 0 || result.Index >= len(matches) {
			return nil, fmt.Errorf("rerank returned index %d for %d candidates", result.Index, len(matches))
		}
		match := matches[result.Index]
		match.Score = result.RelevanceScore
		ranked = append(ranked, match)
	}
	return ranked, nil
}
