package main

import (
	"context"
	"fmt"
	"os"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	"github.com/aws/aws-sdk-go-v2/service/transcribestreaming"
	openai "github.com/sashabaranov/go-openai"

	"github.com/sierragridteam/sierra-data/internal/clients/burnline"
	"github.com/sierragridteam/sierra-data/internal/lib/llm"
)

// Which models read a recording. "openai" is Whisper and a chat model, with a
// key. "aws" is Amazon Transcribe and a Bedrock model, with whatever AWS
// credentials the process has (the task role when hosted): no key.
const (
	providerOpenAI = "openai"
	providerAWS    = "aws"
)

// defaultModel is the extraction model for a provider when -model is not
// given. deepseek.v3.2 was the candidate that read every past transcript
// right, on its day and on a later one, and returned the transcript whole
// (the hosting workspace's notes, 2026-11-24).
func defaultModel(provider string) string {
	if provider == providerAWS {
		return "deepseek.v3.2"
	}
	return openai.GPT4o
}

// models is the pair of providers a run reads with.
type models struct {
	stt burnline.Transcriber
	ai  llm.JSONCompleter
}

// newModels builds the providers. It fails before any call is placed: a
// missing key or an unusable AWS configuration must not cost a phone call.
func newModels(ctx context.Context, provider, vocabulary string) (models, error) {
	switch provider {
	case providerOpenAI:
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return models{}, fmt.Errorf("missing required environment variable OPENAI_API_KEY (-provider=%s)", providerOpenAI)
		}
		if vocabulary != "" {
			return models{}, fmt.Errorf("-vocabulary is Amazon Transcribe's; it means nothing with -provider=%s", providerOpenAI)
		}
		c := openai.NewClient(key)
		return models{stt: burnline.Whisper{Client: c}, ai: llm.OpenAI{Client: c}}, nil
	case providerAWS:
		cfg, err := awsconfig.LoadDefaultConfig(ctx)
		if err != nil {
			return models{}, fmt.Errorf("loading AWS configuration: %w", err)
		}
		if cfg.Region == "" {
			return models{}, fmt.Errorf("no AWS Region (set AWS_REGION)")
		}
		return models{
			stt: burnline.AWSTranscribe{Client: transcribestreaming.NewFromConfig(cfg), Vocabulary: vocabulary},
			ai:  llm.Bedrock{Client: bedrockruntime.NewFromConfig(cfg)},
		}, nil
	default:
		return models{}, fmt.Errorf("-provider must be %q or %q, not %q", providerOpenAI, providerAWS, provider)
	}
}
