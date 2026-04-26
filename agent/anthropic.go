package agent

import (
	"context"
	"encoding/json"
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
		ToolUses:     extractAnthropicToolUses(resp.Content),
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

		type activeToolUse struct {
			ID      string
			Name    string
			builder strings.Builder
		}
		activeToolUses := make(map[int64]*activeToolUse)

		for stream.Next() {
			event := stream.Current()
			switch event.Type {
			case "content_block_start":
				start := event.AsContentBlockStart()
				block := start.ContentBlock
				if block.Type != "tool_use" {
					continue
				}

				toolUse := block.AsToolUse()
				active := &activeToolUse{
					ID:   toolUse.ID,
					Name: toolUse.Name,
				}
				if len(toolUse.Input) > 0 {
					active.builder.Write(toolUse.Input)
				}
				activeToolUses[start.Index] = active

				streamRes <- StreamEvent{
					Type:      "tool_use",
					ToolID:    toolUse.ID,
					ToolName:  toolUse.Name,
					ToolInput: string(toolUse.Input),
				}
			case "content_block_delta":
				deltaEvent := event.AsContentBlockDelta()
				delta := deltaEvent.Delta

				if delta.Type == "text_delta" && delta.Text != "" {
					streamRes <- StreamEvent{Type: "text", Text: delta.Text}
					continue
				}

				if delta.Type != "input_json_delta" {
					continue
				}

				active, exists := activeToolUses[deltaEvent.Index]
				if !exists {
					continue
				}
				active.builder.WriteString(delta.PartialJSON)
				streamRes <- StreamEvent{
					Type:      "tool_use_delta",
					ToolID:    active.ID,
					ToolName:  active.Name,
					ToolInput: delta.PartialJSON,
				}
			case "content_block_stop":
				stop := event.AsContentBlockStop()
				active, exists := activeToolUses[stop.Index]
				if !exists {
					continue
				}
				streamRes <- StreamEvent{
					Type:      "tool_use_end",
					ToolID:    active.ID,
					ToolName:  active.Name,
					ToolInput: active.builder.String(),
				}
				delete(activeToolUses, stop.Index)
			}
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
	case ContentTypeToolUse:
		if block.ToolID == "" || block.ToolName == "" {
			return anthropic.ContentBlockParamUnion{}, errors.New("tool_use block requires tool id and name")
		}
		var input any = map[string]any{}
		if block.ToolInput != "" {
			input = json.RawMessage(block.ToolInput)
		}
		return anthropic.NewToolUseBlock(block.ToolID, input, block.ToolName), nil
	case ContentTypeToolResult:
		if block.ToolID == "" {
			return anthropic.ContentBlockParamUnion{}, errors.New("tool_result block requires tool id")
		}
		return anthropic.NewToolResultBlock(block.ToolID, block.Text, block.IsError), nil
	default:
		return anthropic.ContentBlockParamUnion{}, fmt.Errorf("unsupported content block type: %s", block.Type)
	}
}

func toAnthropicTool(tool Tool) (anthropic.ToolUnionParam, error) {
	name := tool.Name()
	if name == "" {
		return anthropic.ToolUnionParam{}, errors.New("tool.name is required")
	}

	inputSchemaMap := tool.InputSchema()
	if inputSchemaMap == nil {
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
	if description := tool.Description(); description != "" {
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

func extractAnthropicToolUses(blocks []anthropic.ContentBlockUnion) []ToolUse {
	toolUses := make([]ToolUse, 0)
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		toolUse := block.AsToolUse()
		toolUses = append(toolUses, ToolUse{
			ID:    toolUse.ID,
			Name:  toolUse.Name,
			Input: string(toolUse.Input),
		})
	}
	return toolUses
}
