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
	Content                  string
	InputTokens              int
	OutputTokens             int
	CachedInputTokens        int
	CacheCreationInputTokens int
	StopReason               string
	ToolUses                 []ToolUse
	RetryAttempts            int
	Raw                      any
}

type ContentType string

const (
	ContentTypeText       ContentType = "text"
	ContentTypeImage      ContentType = "image"
	ContentTypeToolUse    ContentType = "tool_use"
	ContentTypeToolResult ContentType = "tool_result"
)

type ContentBlock struct {
	Type      ContentType
	Text      string
	Data      string
	MediaType string
	ImageURL  string
	ToolID    string
	ToolName  string
	ToolInput string
	IsError   bool
}

type Message struct {
	Role    string
	Content []ContentBlock
}

type ToolUse struct {
	ID    string
	Name  string
	Input string
}

func (toolUse ToolUse) ToMap() map[string]any {
	return map[string]any{
		"id":        toolUse.ID,
		"name":      toolUse.Name,
		"arguments": toolUse.Input,
	}
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

func ToolUseBlock(id string, name string, input string) ContentBlock {
	return ContentBlock{
		Type:      ContentTypeToolUse,
		ToolID:    id,
		ToolName:  name,
		ToolInput: input,
	}
}

func ToolResultBlock(toolID string, text string, isError bool) ContentBlock {
	return ContentBlock{
		Type:    ContentTypeToolResult,
		Text:    text,
		ToolID:  toolID,
		IsError: isError,
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
	PromptCache PromptCacheConfig
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
