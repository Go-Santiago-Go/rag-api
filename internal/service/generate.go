package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

// generationModelID is the Claude model used to write answers. A small, cheap
// model is the right default for RAG: retrieval does the heavy lifting, and the
// model only has to summarise the passages we hand it. The "us." prefix is a
// cross-region inference profile, which the newer Claude models require on
// Bedrock. Swap the ID to trade cost for quality without touching any logic.
const generationModelID = "us.anthropic.claude-haiku-4-5-20251001-v1:0"

// generationMaxTokens bounds answer length, and with it latency and cost. RAG
// answers are short summaries of retrieved passages, so a modest cap is plenty.
const generationMaxTokens = 1024

// Generator turns a prompt into generated text. Bedrock (a Claude model via the
// Converse API) is one implementation; a fake satisfies it in tests with no
// network call. The RAG service depends on this interface, never on the AWS SDK
// directly, the same dependency inversion used for Embedder and VectorStore.
type Generator interface {
	Generate(ctx context.Context, prompt string) (string, error)
}

// BedrockGenerator implements Generator using a Claude model on Amazon Bedrock via
// the Converse API. It holds a Bedrock Runtime client; all Claude-specific
// request and response knowledge lives in this file so nothing upstream depends
// on it.
type BedrockGenerator struct {
	client  *bedrockruntime.Client
	modelID string
}

// NewBedrockGenerator returns a Generator backed by a configured Bedrock Runtime
// client, using the service's default generation model. The client carries AWS
// credentials and region, loaded once at main and injected here.
func NewBedrockGenerator(client *bedrockruntime.Client) *BedrockGenerator {
	return NewBedrockGeneratorWithModel(client, generationModelID)
}

// NewBedrockGeneratorWithModel returns a Generator pinned to a specific Bedrock
// model.
//
// The evaluation judge needs this: grading an answer with the model that wrote
// it invites self-preference bias, where a model rates its own output more
// favourably than a third party would. Passing the ID explicitly keeps the
// judge's model a visible choice in the harness rather than a constant buried
// in the service.
func NewBedrockGeneratorWithModel(client *bedrockruntime.Client, modelID string) *BedrockGenerator {
	return &BedrockGenerator{client: client, modelID: modelID}
}

// Compile-time proof BedrockGenerator satisfies Generator; fails the build here
// if the method set ever drifts from the interface.
var _ Generator = (*BedrockGenerator)(nil)

// Generate sends prompt to the configured Claude model through the Bedrock
// Converse API and returns the model's text answer. It wraps the prompt in a
// single user message, invokes Converse, then unwraps the response to extract the
// text. Any invocation or unexpected-shape failure is returned wrapped for
// context.
func (g *BedrockGenerator) Generate(ctx context.Context, prompt string) (string, error) {
	// One user message whose single content block is the prompt. ContentBlock and
	// ConverseOutput are SDK "tagged unions": an interface plus concrete member
	// types. A text block is a *types.ContentBlockMemberText because the interface
	// marker method is on the pointer receiver, which is why it is taken by address.
	out, err := g.client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId: aws.String(g.modelID),
		Messages: []types.Message{
			{
				Role: types.ConversationRoleUser,
				Content: []types.ContentBlock{
					&types.ContentBlockMemberText{Value: prompt},
				},
			},
		},
		InferenceConfig: &types.InferenceConfiguration{
			MaxTokens: aws.Int32(generationMaxTokens),
		},
	})
	if err != nil {
		return "", fmt.Errorf("bedrock converse: %w", err)
	}

	// Unwrap the response union. A completed chat turn is always a message; any
	// other concrete type means an unexpected response shape, so fail loudly
	// rather than silently returning empty text.
	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return "", fmt.Errorf("unexpected converse output type %T", out.Output)
	}

	// Concatenate the text blocks. Claude normally returns a single one, but
	// Content is a slice, so join defensively and skip any non-text blocks.
	var b strings.Builder
	for _, block := range msg.Value.Content {
		if text, ok := block.(*types.ContentBlockMemberText); ok {
			b.WriteString(text.Value)
		}
	}

	answer := b.String()
	if answer == "" {
		return "", fmt.Errorf("empty generation response")
	}
	return answer, nil
}
