package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	client      anthropic.Client
	apiKey      string
	baseURL     string
	useMessages bool
	logger      Logger
}

func NewAnthropicLLM(config ClientConfig, opts ...anthropicoption.RequestOption) *AnthropicLLM {
	if config.APIKey != "" {
		opts = append(opts, anthropicoption.WithAPIKey(config.APIKey))
	}
	if config.BaseURL != "" {
		opts = append(opts, anthropicoption.WithBaseURL(config.BaseURL))
	}

	return &AnthropicLLM{
		client:      anthropic.NewClient(opts...),
		apiKey:      config.APIKey,
		baseURL:     strings.TrimRight(config.BaseURL, "/"),
		useMessages: strings.TrimSpace(config.BaseURL) != "",
		logger:      noopLogger{},
	}
}

func (l *AnthropicLLM) Client() anthropic.Client {
	return l.client
}

func (l *AnthropicLLM) SetLogger(logger Logger) {
	if logger == nil {
		logger = noopLogger{}
	}
	l.logger = logger
}

func (l *AnthropicLLM) Chat(ctx context.Context, messages []Message, opts ChatOptions) (LLMResponse, error) {
	traceID := TraceIDFromContext(ctx)
	if l.useMessages {
		return l.messages(ctx, messages, opts)
	}

	params, err := buildAnthropicMessageParams(messages, opts)
	if err != nil {
		l.log(traceID, "anthropic sdk chat build_params error=%v", err)
		return LLMResponse{}, err
	}

	l.log(traceID, "anthropic sdk chat request model=%s messages=%d tools=%d", opts.Model, len(messages), len(opts.Tools))
	resp, err := l.client.Messages.New(ctx, params)
	if err != nil {
		l.log(traceID, "anthropic sdk chat network_error=%+v", err)
		return LLMResponse{}, err
	}
	l.log(traceID, "anthropic sdk chat response stop=%s input=%d output=%d", resp.StopReason, resp.Usage.InputTokens, resp.Usage.OutputTokens)

	return LLMResponse{
		Content:                  extractAnthropicText(resp.Content),
		InputTokens:              int(resp.Usage.InputTokens),
		OutputTokens:             int(resp.Usage.OutputTokens),
		CachedInputTokens:        int(resp.Usage.CacheReadInputTokens),
		CacheCreationInputTokens: int(resp.Usage.CacheCreationInputTokens),
		StopReason:               string(resp.StopReason),
		ToolUses:                 extractAnthropicToolUses(resp.Content),
		Raw:                      resp,
	}, nil
}

func (l *AnthropicLLM) Stream(ctx context.Context, messages []Message, opts ChatOptions) <-chan StreamEvent {
	streamRes := make(chan StreamEvent)
	traceID := TraceIDFromContext(ctx)

	go func() {
		defer close(streamRes)

		if l.useMessages {
			resp, err := l.messages(ctx, messages, opts)
			if err != nil {
				streamRes <- StreamEvent{Err: err, Done: true}
				return
			}
			if resp.Content != "" {
				streamRes <- StreamEvent{Type: "text", Text: resp.Content}
			}
			for _, toolUse := range resp.ToolUses {
				streamRes <- StreamEvent{
					Type:       "tool_use",
					ToolID:     toolUse.ID,
					ToolCallID: toolUse.ID,
					ToolName:   toolUse.Name,
					ToolInput:  toolUse.Input,
				}
				streamRes <- StreamEvent{
					Type:       "tool_use_end",
					ToolID:     toolUse.ID,
					ToolCallID: toolUse.ID,
					ToolName:   toolUse.Name,
					ToolInput:  toolUse.Input,
				}
			}
			streamRes <- StreamEvent{
				Type:                     "usage",
				InputTokens:              resp.InputTokens,
				OutputTokens:             resp.OutputTokens,
				CachedInputTokens:        resp.CachedInputTokens,
				CacheCreationInputTokens: resp.CacheCreationInputTokens,
			}
			streamRes <- StreamEvent{Done: true}
			return
		}

		params, err := buildAnthropicMessageParams(messages, opts)
		if err != nil {
			l.log(traceID, "anthropic sdk stream build_params error=%v", err)
			streamRes <- StreamEvent{Err: err, Done: true}
			return
		}

		l.log(traceID, "anthropic sdk stream request model=%s messages=%d tools=%d", opts.Model, len(messages), len(opts.Tools))
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
			case "message_start":
				start := event.AsMessageStart()
				usage := start.Message.Usage
				streamRes <- StreamEvent{
					Type:                     "usage",
					InputTokens:              int(usage.InputTokens),
					OutputTokens:             int(usage.OutputTokens),
					CachedInputTokens:        int(usage.CacheReadInputTokens),
					CacheCreationInputTokens: int(usage.CacheCreationInputTokens),
				}
			case "message_delta":
				delta := event.AsMessageDelta()
				streamRes <- StreamEvent{
					Type:                     "usage",
					InputTokens:              int(delta.Usage.InputTokens),
					OutputTokens:             int(delta.Usage.OutputTokens),
					CachedInputTokens:        int(delta.Usage.CacheReadInputTokens),
					CacheCreationInputTokens: int(delta.Usage.CacheCreationInputTokens),
				}
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
					Type:       "tool_use",
					ToolID:     toolUse.ID,
					ToolCallID: toolUse.ID,
					ToolName:   toolUse.Name,
					ToolInput:  string(toolUse.Input),
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
					Type:       "tool_use_end",
					ToolID:     active.ID,
					ToolCallID: active.ID,
					ToolName:   active.Name,
					ToolInput:  active.builder.String(),
				}
				delete(activeToolUses, stop.Index)
			}
		}

		if err := stream.Err(); err != nil {
			l.log(traceID, "anthropic sdk stream network_error=%+v", err)
			streamRes <- StreamEvent{Err: err, Done: true}
			return
		}

		l.log(traceID, "anthropic sdk stream completed")
		streamRes <- StreamEvent{Done: true}
	}()

	return streamRes
}

func (l *AnthropicLLM) log(traceID string, format string, args ...any) {
	if l == nil || l.logger == nil {
		return
	}
	l.logger.Log(traceID, format, args...)
}

type anthropicMessageRequest struct {
	Model       string                   `json:"model"`
	MaxTokens   int64                    `json:"max_tokens"`
	System      string                   `json:"system,omitempty"`
	Messages    []anthropicCompatMessage `json:"messages"`
	Tools       []anthropicCompatTool    `json:"tools,omitempty"`
	Temperature *float64                 `json:"temperature,omitempty"`
}

type anthropicCompatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicCompatContentBlock struct {
	Type      string                      `json:"type"`
	Text      string                      `json:"text,omitempty"`
	ID        string                      `json:"id,omitempty"`
	Name      string                      `json:"name,omitempty"`
	Input     json.RawMessage             `json:"input,omitempty"`
	ToolUseID string                      `json:"tool_use_id,omitempty"`
	Content   string                      `json:"content,omitempty"`
	IsError   bool                        `json:"is_error,omitempty"`
	Source    *anthropicCompatImageSource `json:"source,omitempty"`
}

type anthropicCompatImageSource struct {
	Type      string `json:"type"`
	URL       string `json:"url,omitempty"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
}

type anthropicCompatTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type anthropicMessageResponse struct {
	ID         string                        `json:"id"`
	Model      string                        `json:"model"`
	StopReason string                        `json:"stop_reason"`
	Content    []anthropicCompatContentBlock `json:"content"`
	Usage      struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func (l *AnthropicLLM) messages(ctx context.Context, messages []Message, opts ChatOptions) (LLMResponse, error) {
	traceID := TraceIDFromContext(ctx)
	if l.baseURL == "" {
		return LLMResponse{}, errors.New("anthropic-compatible base url is required")
	}
	requestBody, err := buildAnthropicCompatRequest(messages, opts)
	if err != nil {
		l.log(traceID, "anthropic messages build_request error=%v", err)
		return LLMResponse{}, err
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return LLMResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.baseURL+"/messages", bytes.NewReader(data))
	if err != nil {
		l.log(traceID, "anthropic messages new_request error=%v", err)
		return LLMResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.apiKey != "" {
		req.Header.Set("x-api-key", l.apiKey)
		req.Header.Set("Authorization", "Bearer "+l.apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")

	l.log(traceID, "anthropic messages request endpoint=%s model=%s messages=%d tools=%d", l.baseURL+"/messages", requestBody.Model, len(requestBody.Messages), len(requestBody.Tools))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.log(traceID, "anthropic messages network_error=%+v", err)
		return LLMResponse{}, err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		l.log(traceID, "anthropic messages read_body error=%+v", err)
		return LLMResponse{}, err
	}
	l.log(traceID, "anthropic messages raw_response status=%d body=%s", resp.StatusCode, string(respData))

	var decoded anthropicMessageResponse
	if err := json.Unmarshal(respData, &decoded); err != nil {
		l.log(traceID, "anthropic messages decode_error status=%d error=%+v body=%s", resp.StatusCode, err, string(respData))
		return LLMResponse{}, fmt.Errorf("decode anthropic message response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respData))
		if decoded.Error != nil && decoded.Error.Message != "" {
			message = decoded.Error.Message
		}
		l.log(traceID, "anthropic messages response_error status=%d message=%s", resp.StatusCode, message)
		return LLMResponse{StopReason: message, Raw: decoded}, fmt.Errorf("anthropic messages request failed: status=%d message=%s", resp.StatusCode, message)
	}

	l.log(traceID, "anthropic messages response status=%d stop=%s input=%d output=%d tools=%d", resp.StatusCode, decoded.StopReason, decoded.Usage.InputTokens, decoded.Usage.OutputTokens, len(extractAnthropicCompatToolUses(decoded.Content)))
	return LLMResponse{
		Content:                  extractAnthropicCompatText(decoded.Content),
		InputTokens:              decoded.Usage.InputTokens,
		OutputTokens:             decoded.Usage.OutputTokens,
		CachedInputTokens:        decoded.Usage.CacheReadInputTokens,
		CacheCreationInputTokens: decoded.Usage.CacheCreationInputTokens,
		StopReason:               decoded.StopReason,
		ToolUses:                 extractAnthropicCompatToolUses(decoded.Content),
		Raw:                      decoded,
	}, nil
}

func buildAnthropicCompatRequest(messages []Message, opts ChatOptions) (anthropicMessageRequest, error) {
	model := opts.Model
	if model == "" {
		model = string(defaultAnthropicModel)
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultAnthropicMaxTokens
	}

	convertedMessages := make([]anthropicCompatMessage, 0, len(messages))
	for _, message := range messages {
		converted, err := toAnthropicCompatMessage(message)
		if err != nil {
			return anthropicMessageRequest{}, err
		}
		convertedMessages = append(convertedMessages, converted)
	}

	tools := make([]anthropicCompatTool, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		converted, err := toAnthropicCompatTool(tool)
		if err != nil {
			return anthropicMessageRequest{}, err
		}
		tools = append(tools, converted)
	}

	request := anthropicMessageRequest{
		Model:       model,
		MaxTokens:   maxTokens,
		Messages:    convertedMessages,
		Tools:       tools,
		Temperature: opts.Temperature,
	}
	if opts.System != nil {
		request.System = *opts.System
	}
	return request, nil
}

func toAnthropicCompatMessage(message Message) (anthropicCompatMessage, error) {
	role := strings.ToLower(message.Role)
	if role == "" {
		return anthropicCompatMessage{}, errors.New("message role is required")
	}
	if role != "user" && role != "assistant" {
		return anthropicCompatMessage{}, fmt.Errorf("unsupported anthropic message role: %s", message.Role)
	}

	blocks, err := toAnthropicCompatContent(message.Content)
	if err != nil {
		return anthropicCompatMessage{}, err
	}
	return anthropicCompatMessage{
		Role:    role,
		Content: blocks,
	}, nil
}

func toAnthropicCompatContent(blocks []ContentBlock) ([]anthropicCompatContentBlock, error) {
	if len(blocks) == 0 {
		return nil, errors.New("message content must not be empty")
	}

	result := make([]anthropicCompatContentBlock, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeText:
			if block.Text == "" {
				return nil, errors.New("text block requires text")
			}
			result = append(result, anthropicCompatContentBlock{
				Type: "text",
				Text: block.Text,
			})
		case ContentTypeImage:
			source := &anthropicCompatImageSource{}
			if block.ImageURL != "" {
				source.Type = "url"
				source.URL = block.ImageURL
			} else {
				if block.Data == "" || block.MediaType == "" {
					return nil, errors.New("image block requires image url or data/media type")
				}
				source.Type = "base64"
				source.MediaType = block.MediaType
				source.Data = block.Data
			}
			result = append(result, anthropicCompatContentBlock{
				Type:   "image",
				Source: source,
			})
		case ContentTypeToolUse:
			if block.ToolID == "" || block.ToolName == "" {
				return nil, errors.New("tool_use block requires tool id and name")
			}
			input := json.RawMessage(`{}`)
			if block.ToolInput != "" {
				input = json.RawMessage(block.ToolInput)
			}
			result = append(result, anthropicCompatContentBlock{
				Type:  "tool_use",
				ID:    block.ToolID,
				Name:  block.ToolName,
				Input: input,
			})
		case ContentTypeToolResult:
			if block.ToolID == "" {
				return nil, errors.New("tool_result block requires tool id")
			}
			result = append(result, anthropicCompatContentBlock{
				Type:      "tool_result",
				ToolUseID: block.ToolID,
				Content:   block.Text,
				IsError:   block.IsError,
			})
		default:
			return nil, fmt.Errorf("unsupported content block type: %s", block.Type)
		}
	}
	return result, nil
}

func toAnthropicCompatTool(tool Tool) (anthropicCompatTool, error) {
	name := tool.Name()
	if name == "" {
		return anthropicCompatTool{}, errors.New("tool.name is required")
	}
	inputSchema := tool.InputSchema()
	if inputSchema == nil {
		return anthropicCompatTool{}, errors.New("tool.input_schema must be an object")
	}
	return anthropicCompatTool{
		Name:        name,
		Description: tool.Description(),
		InputSchema: inputSchema,
	}, nil
}

func extractAnthropicCompatText(blocks []anthropicCompatContentBlock) string {
	var builder strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			builder.WriteString(block.Text)
		}
	}
	return builder.String()
}

func extractAnthropicCompatToolUses(blocks []anthropicCompatContentBlock) []ToolUse {
	toolUses := make([]ToolUse, 0)
	for _, block := range blocks {
		if block.Type != "tool_use" {
			continue
		}
		toolUses = append(toolUses, ToolUse{
			ID:    block.ID,
			Name:  block.Name,
			Input: string(block.Input),
		})
	}
	return toolUses
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
	cacheConfig := normalizePromptCacheConfig(opts.PromptCache)
	if cacheConfig.Enabled {
		cacheControl := anthropicCacheControl(cacheConfig)
		if len(params.System) > 0 {
			params.System[len(params.System)-1].CacheControl = cacheControl
		} else {
			params.CacheControl = cacheControl
		}
	}

	for _, tool := range opts.Tools {
		convertedTool, err := toAnthropicTool(tool)
		if err != nil {
			return anthropic.MessageNewParams{}, err
		}
		params.Tools = append(params.Tools, convertedTool)
	}
	if cacheConfig.Enabled && len(params.Tools) > 0 {
		if cacheControl := params.Tools[len(params.Tools)-1].GetCacheControl(); cacheControl != nil {
			*cacheControl = anthropicCacheControl(cacheConfig)
		}
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

func anthropicCacheControl(config PromptCacheConfig) anthropic.CacheControlEphemeralParam {
	cacheControl := anthropic.NewCacheControlEphemeralParam()
	switch config.TTL {
	case PromptCacheTTL1h:
		cacheControl.TTL = anthropic.CacheControlEphemeralTTLTTL1h
	default:
		cacheControl.TTL = anthropic.CacheControlEphemeralTTLTTL5m
	}
	return cacheControl
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
