package agent

import (
	"context"
	"errors"
)

const (
	ProviderOpenAI    = "OpenAI"
	ProviderAnthropic = "Anthropic"
	ProviderUnknown   = "Unknown"
)

type LLMConfig struct {
	Provider string
	BaseURL  string
	APIKey   string
}

type LLMResponse struct {
	Content                   string
	InputTokens, OutputTokens int
	StopReason                string
	Raw                       any
}

type ContentType string

const (
	ContentTypeText  ContentType = "text"
	ContentTypeImage ContentType = "image"
)

type ContentBlock struct {
	Type      ContentType
	Text      string
	Data      string
	MediaType string
	ImageURL  string
}

type Message struct {
	Role    string
	Content []ContentBlock
}

func TextBlock(text string) ContentBlock {
	return ContentBlock{
		Type: ContentTypeText,
		Text: text,
	}
}

func ImageBase64Block(mediaType string, data string) ContentBlock {
	return ContentBlock{
		Type:      ContentTypeImage,
		Data:      data,
		MediaType: mediaType,
	}
}

func ImageURLBlock(imageURL string) ContentBlock {
	return ContentBlock{
		Type:     ContentTypeImage,
		ImageURL: imageURL,
	}
}

type Tool map[string]any

type ClientConfig struct {
	APIKey  string
	BaseURL string
}

type ChatOptions struct {
	Model       string
	MaxTokens   int64
	Temperature *float64
	System      *string
	Tools       []Tool
}

type StreamEvent struct {
	Text string
	Err  error
	Done bool
}

type LLM interface {
	Chat(ctx context.Context, messages []Message, opts ChatOptions) (LLMResponse, error)
	Stream(ctx context.Context, messages []Message, opts ChatOptions) <-chan StreamEvent
}

func CreateLLM(config LLMConfig) (LLM, error) {
	if config.Provider == ProviderOpenAI {
		return NewOpenAILLM(ClientConfig{BaseURL: config.BaseURL, APIKey: config.APIKey}), nil
	} else if config.Provider == ProviderAnthropic {
		return NewAnthropicLLM(ClientConfig{BaseURL: config.BaseURL, APIKey: config.APIKey}), nil
	}
	return nil, errors.New("invalid provider")
}
