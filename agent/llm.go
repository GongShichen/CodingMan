package agent

import (
	"context"
	"errors"

	tool "github.com/GongShichen/CodingMan/tool"
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
	ToolId    string
}

type Message struct {
	Role    string
	Content []ContentBlock
}

type Tool = tool.Tool

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

type ClientConfig struct {
	APIKey  string
	BaseURL string
}

type ChatOptions struct {
	Model       string
	MaxTokens   int64
	Temperature *float64
	System      *string
	Tools       []tool.Tool
}

type StreamEvent struct {
	Text      string
	Err       error
	Done      bool
	Type      string // text | tool_use_start | tool_use_delta | tool_use_end
	ToolID    string
	ToolName  string
	ToolInput string
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
