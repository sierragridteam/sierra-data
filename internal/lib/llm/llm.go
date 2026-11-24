// Package llm is the one seam between our prompts and whichever provider
// answers them: a single user prompt in, one JSON document matching a schema
// out. The burn line reader is its first user; the alert enhancer is expected
// to follow when the Grid's own model calls move to Bedrock.
//
// It is deliberately this small. Prompts, schemas and what to do with a wrong
// answer stay with the caller; a provider here only carries the request and
// returns the model's text.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	openai "github.com/sashabaranov/go-openai"
)

// JSONRequest asks a model for one JSON document.
type JSONRequest struct {
	// Model is the provider's model id ("gpt-4o", "deepseek.v3.2").
	Model string
	// Prompt is the whole user message.
	Prompt string
	// SchemaName names the schema to the provider (letters, digits, _ and -).
	SchemaName string
	// Schema is the JSON Schema the answer must match. Every object in it needs
	// "additionalProperties": false; both providers enforce the schema strictly.
	Schema json.RawMessage
	// Temperature is passed through as given.
	Temperature float32
	// MaxTokens bounds the answer. Zero means DefaultMaxTokens. Too low and
	// the JSON is cut off, which CompleteJSON reports as an error.
	MaxTokens int
}

// DefaultMaxTokens is generous for our prompts: a cut-off answer is not JSON.
const DefaultMaxTokens = 4000

// JSONCompleter returns the model's answer to req as JSON text. The text is
// guaranteed by the provider to match the schema, not checked here.
type JSONCompleter interface {
	CompleteJSON(ctx context.Context, req JSONRequest) (string, error)
}

// OpenAI answers through the Chat Completions API with a strict json_schema
// response format.
type OpenAI struct {
	Client *openai.Client
}

// CompleteJSON implements JSONCompleter.
func (o OpenAI) CompleteJSON(ctx context.Context, req JSONRequest) (string, error) {
	resp, err := o.Client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model:       req.Model,
		Temperature: req.Temperature,
		Messages: []openai.ChatCompletionMessage{
			{Role: openai.ChatMessageRoleUser, Content: req.Prompt},
		},
		ResponseFormat: &openai.ChatCompletionResponseFormat{
			Type: openai.ChatCompletionResponseFormatTypeJSONSchema,
			JSONSchema: &openai.ChatCompletionResponseFormatJSONSchema{
				Name:   req.SchemaName,
				Strict: true,
				Schema: req.Schema,
			},
		},
	})
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", errors.New("llm: the model returned no choices")
	}
	return resp.Choices[0].Message.Content, nil
}

// ConverseAPI is the one Bedrock call this package makes
// (*bedrockruntime.Client satisfies it).
type ConverseAPI interface {
	Converse(ctx context.Context, in *bedrockruntime.ConverseInput, optFns ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error)
}

// Bedrock answers through the Converse API with a json_schema text format.
// Credentials and Region come from the client: in the hosting account that is
// the task role, so there is no key.
type Bedrock struct {
	Client ConverseAPI
}

// CompleteJSON implements JSONCompleter.
func (b Bedrock) CompleteJSON(ctx context.Context, req JSONRequest) (string, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	out, err := b.Client.Converse(ctx, &bedrockruntime.ConverseInput{
		ModelId: aws.String(req.Model),
		Messages: []types.Message{{
			Role:    types.ConversationRoleUser,
			Content: []types.ContentBlock{&types.ContentBlockMemberText{Value: req.Prompt}},
		}},
		InferenceConfig: &types.InferenceConfiguration{
			MaxTokens:   aws.Int32(int32(maxTokens)),
			Temperature: aws.Float32(req.Temperature),
		},
		OutputConfig: &types.OutputConfig{
			TextFormat: &types.OutputFormat{
				Type: types.OutputFormatTypeJsonSchema,
				Structure: &types.OutputFormatStructureMemberJsonSchema{
					Value: types.JsonSchemaDefinition{
						Name:   aws.String(req.SchemaName),
						Schema: aws.String(string(req.Schema)),
					},
				},
			},
		},
	})
	if err != nil {
		return "", err
	}
	if out.StopReason == types.StopReasonMaxTokens {
		return "", fmt.Errorf("llm: the answer was cut off at %d tokens", maxTokens)
	}
	msg, ok := out.Output.(*types.ConverseOutputMemberMessage)
	if !ok {
		return "", errors.New("llm: the model returned no message")
	}
	// A model that reasons first returns its reasoning as earlier blocks; the
	// answer is the last text block.
	text := ""
	for _, c := range msg.Value.Content {
		if t, ok := c.(*types.ContentBlockMemberText); ok {
			text = t.Value
		}
	}
	if text == "" {
		return "", errors.New("llm: the model returned no text")
	}
	return text, nil
}
