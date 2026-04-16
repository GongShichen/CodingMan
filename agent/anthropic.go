package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	anthropic "github.com/anthropics/anthropic-sdk-go"
	anthropicoption "github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
)

const (
	defaultAnthropicModel     = anthropic.ModelClaudeSonnet4_5
	defaultAnthropicMaxTokens = int64(4096)
)

type AnthropicLLM struct {
	client anthropic.Client
}

func NewAnthropicLLM(config ClientConfig, opts ...anthropicoption.RequestOption) *AnthropicLLM {
	if config.APIKey != "" {
		opts = append(opts, anthropicoption.WithAPIKey(config.APIKey))
	}
	if config.BaseURL != "" {
		opts = append(opts, anthropicoption.WithBaseURL(config.BaseURL))
	}

	return &AnthropicLLM{
		client: anthropic.NewClient(opts...),
	}
}

func (l *AnthropicLLM) Client() anthropic.Client {
	return l.client
}

func (l *AnthropicLLM) Chat(ctx context.Context, messages []Message, opts ChatOptions) (LLMResponse, error) {
	params, err := buildAnthropicMessageParams(messages, opts)
	if err != nil {
		return LLMResponse{}, err
	}

	resp, err := l.client.Messages.New(ctx, params)
	if err != nil {
		return LLMResponse{}, err
	}

	return LLMResponse{
		Content:      extractAnthropicText(resp.Content),
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
		StopReason:   string(resp.StopReason),
		Raw:          resp,
	}, nil
}

func (l *AnthropicLLM) Stream(ctx context.Context, messages []Message, opts ChatOptions) <-chan StreamEvent {
	streamRes := make(chan StreamEvent)

	go func() {
		defer close(streamRes)

		params, err := buildAnthropicMessageParams(messages, opts)
		if err != nil {
			streamRes <- StreamEvent{Err: err, Done: true}
			return
		}

		stream := l.client.Messages.NewStreaming(ctx, params)
		defer func() {
			if err := stream.Close(); err != nil {
				streamRes <- StreamEvent{Err: err, Done: true}
			}
		}()

		for stream.Next() {
			event := stream.Current()
			if event.Type != "content_block_delta" {
				continue
			}

			delta := event.AsContentBlockDelta().Delta
			if delta.Type != "text_delta" || delta.Text == "" {
				continue
			}

			streamRes <- StreamEvent{Text: delta.Text}
		}

		if err := stream.Err(); err != nil {
			streamRes <- StreamEvent{Err: err, Done: true}
			return
		}

		streamRes <- StreamEvent{Done: true}
	}()

	return streamRes
}

func buildAnthropicMessageParams(messages []Message, opts ChatOptions) (anthropic.MessageNewParams, error) {
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(opts.Model),
		MaxTokens: opts.MaxTokens,
		Messages:  make([]anthropic.MessageParam, 0, len(messages)),
	}

	if params.Model == "" {
		params.Model = defaultAnthropicModel
	}
	if params.MaxTokens == 0 {
		params.MaxTokens = defaultAnthropicMaxTokens
	}
	if opts.Temperature != nil {
		params.Temperature = param.NewOpt(*opts.Temperature)
	}
	if opts.System != nil && *opts.System != "" {
		params.System = []anthropic.TextBlockParam{{Text: *opts.System}}
	}

	for _, tool := range opts.Tools {
		convertedTool, err := toAnthropicTool(tool)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		params.Tools = append(params.Tools, convertedTool)
	}

	for _, message := range messages {
		convertedMessage, err := toAnthropicMessage(message)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		params.Messages = append(params.Messages, convertedMessage)
	}

	return params, nil
}

func toAnthropicMessage(message Message) (anthropic.MessageParam, error) {
	role := message.Role
	if role == "" {
		return anthropic.MessageParam{}, errors.New("message role is required")
	}

	blocks, err := toAnthropicContentBlocks(message.Content)
	if err != nil {
		return anthropic.MessageParam{}, err
	}

	switch strings.ToLower(role) {
	case "user":
		return anthropic.NewUserMessage(blocks...), nil
	case "assistant":
		return anthropic.NewAssistantMessage(blocks...), nil
	default:
		return anthropic.MessageParam{}, fmt.Errorf("unsupported anthropic message role: %s", role)
	}
}

func toAnthropicContentBlocks(content any) ([]anthropic.ContentBlockParamUnion, error) {
	blocks, ok := content.([]ContentBlock)
	if !ok {
		return nil, errors.New("message content must be []ContentBlock")
	}
	if len(blocks) == 0 {
		return nil, errors.New("message content must not be empty")
	}

	result := make([]anthropic.ContentBlockParamUnion, 0, len(blocks))
	for _, block := range blocks {
		converted, err := toAnthropicContentBlock(block)
		if err != nil {
			return nil, err
		}
		result = append(result, converted)
	}

	return result, nil
}

func toAnthropicContentBlock(block ContentBlock) (anthropic.ContentBlockParamUnion, error) {
	switch block.Type {
	case ContentTypeText:
		if block.Text == "" {
			return anthropic.ContentBlockParamUnion{}, errors.New("text block requires text")
		}
		return anthropic.NewTextBlock(block.Text), nil
	case ContentTypeImage:
		if block.ImageURL != "" {
			return anthropic.NewImageBlock(anthropic.URLImageSourceParam{URL: block.ImageURL}), nil
		}
		if block.Data == "" || block.MediaType == "" {
			return anthropic.ContentBlockParamUnion{}, errors.New("image block requires image url or data/media type")
		}
		return anthropic.NewImageBlockBase64(block.MediaType, block.Data), nil
	default:
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("unsupported content block type: %s", block.Type)
	}
}

func toAnthropicTool(tool Tool) (anthropic.ToolUnionParam, error) {
	name, ok := tool["name"].(string)
	if !ok || name == "" {
		return anthropic.ToolUnionParam{}, errors.New("tool.name is required")
	}

	inputSchemaMap, ok := tool["input_schema"].(map[string]any)
	if !ok {
		return anthropic.ToolUnionParam{}, errors.New("tool.input_schema must be an object")
	}

	schema := anthropic.ToolInputSchemaParam{}
	if properties, exists := inputSchemaMap["properties"]; exists {
		schema.Properties = properties
	}
	if required, exists := inputSchemaMap["required"]; exists {
		switch v := required.(type) {
		case []string:
			schema.Required = v
		case []any:
			schema.Required = make([]string, 0, len(v))
			for _, item := range v {
				text, ok := item.(string)
				if !ok {
					return anthropic.ToolUnionParam{}, errors.New("tool.input_schema.required must contain only strings")
				}
				schema.Required = append(schema.Required, text)
			}
		default:
			return anthropic.ToolUnionParam{}, errors.New("tool.input_schema.required must be a string list")
		}
	}

	toolParam := anthropic.ToolUnionParamOfTool(schema, name)
	if description, ok := tool["description"].(string); ok && description != "" {
		if toolParam.OfTool != nil {
			toolParam.OfTool.Description = param.NewOpt(description)
		}
	}

	return toolParam, nil
}

func extractAnthropicText(blocks []anthropic.ContentBlockUnion) string {
	var builder strings.Builder

	for _, block := range blocks {
		if block.Type != "text" {
			continue
		}
		builder.WriteString(block.Text)
	}

	return builder.String()
}
