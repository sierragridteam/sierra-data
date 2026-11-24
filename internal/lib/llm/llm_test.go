package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testReq = JSONRequest{
	Model:       "some-model",
	Prompt:      "Is it a burn day?",
	SchemaName:  "answer",
	Schema:      json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`),
	Temperature: 0.1,
}

type fakeConverse struct {
	in  *bedrockruntime.ConverseInput
	out *bedrockruntime.ConverseOutput
	err error
}

func (f *fakeConverse) Converse(_ context.Context, in *bedrockruntime.ConverseInput, _ ...func(*bedrockruntime.Options)) (*bedrockruntime.ConverseOutput, error) {
	f.in = in
	return f.out, f.err
}

func message(stop types.StopReason, blocks ...types.ContentBlock) *bedrockruntime.ConverseOutput {
	return &bedrockruntime.ConverseOutput{
		StopReason: stop,
		Output:     &types.ConverseOutputMemberMessage{Value: types.Message{Role: types.ConversationRoleAssistant, Content: blocks}},
	}
}

func TestBedrockSendsTheSchemaAndReturnsTheLastTextBlock(t *testing.T) {
	f := &fakeConverse{out: message(types.StopReasonEndTurn,
		&types.ContentBlockMemberText{Value: "thinking aloud"},
		&types.ContentBlockMemberText{Value: `{"ok":true}`},
	)}
	got, err := Bedrock{Client: f}.CompleteJSON(context.Background(), testReq)
	require.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, got)

	assert.Equal(t, "some-model", aws.ToString(f.in.ModelId))
	assert.Equal(t, int32(DefaultMaxTokens), aws.ToInt32(f.in.InferenceConfig.MaxTokens))
	require.NotNil(t, f.in.OutputConfig)
	assert.Equal(t, types.OutputFormatTypeJsonSchema, f.in.OutputConfig.TextFormat.Type)
	schema, ok := f.in.OutputConfig.TextFormat.Structure.(*types.OutputFormatStructureMemberJsonSchema)
	require.True(t, ok)
	assert.Equal(t, "answer", aws.ToString(schema.Value.Name))
	assert.JSONEq(t, string(testReq.Schema), aws.ToString(schema.Value.Schema))
	text, ok := f.in.Messages[0].Content[0].(*types.ContentBlockMemberText)
	require.True(t, ok)
	assert.Equal(t, testReq.Prompt, text.Value)
}

// A cut-off answer is half a JSON document. It must be an error, not
// something the caller tries to parse.
func TestBedrockRefusesACutOffAnswer(t *testing.T) {
	f := &fakeConverse{out: message(types.StopReasonMaxTokens, &types.ContentBlockMemberText{Value: `{"ok":tr`})}
	_, err := Bedrock{Client: f}.CompleteJSON(context.Background(), testReq)
	require.ErrorContains(t, err, "cut off")
}

func TestBedrockRefusesAnAnswerWithNoText(t *testing.T) {
	f := &fakeConverse{out: message(types.StopReasonEndTurn)}
	_, err := Bedrock{Client: f}.CompleteJSON(context.Background(), testReq)
	require.ErrorContains(t, err, "no text")
}

func TestOpenAISendsAStrictSchema(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": `{"ok":true}`},
		}}})
	}))
	defer srv.Close()
	cfg := openai.DefaultConfig("k")
	cfg.BaseURL = srv.URL

	got, err := OpenAI{Client: openai.NewClientWithConfig(cfg)}.CompleteJSON(context.Background(), testReq)
	require.NoError(t, err)
	assert.Equal(t, `{"ok":true}`, got)
	format := body["response_format"].(map[string]any)
	assert.Equal(t, "json_schema", format["type"])
	js := format["json_schema"].(map[string]any)
	assert.Equal(t, "answer", js["name"])
	assert.Equal(t, true, js["strict"])
}
