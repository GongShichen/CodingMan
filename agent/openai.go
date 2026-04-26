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

	openai "github.com/openai/openai-go/v3"
	openaioption "github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const (
	defaultOpenAIModel     = shared.ChatModelGPT5Mini
	defaultOpenAIMaxTokens = int64(4096)
)

type OpenAILLM struct {
	client             openai.Client
	apiKey             string
	baseURL            string
	useChatCompletions bool
	logger             Logger
}

func NewOpenAILLM(config ClientConfig, opts ...openaioption.RequestOption) *OpenAILLM {
	if config.APIKey != "" {
		opts = append(opts, openaioption.WithAPIKey(config.APIKey))
	}
	if config.BaseURL != "" {
		opts = append(opts, openaioption.WithBaseURL(config.BaseURL))
	}

	return &OpenAILLM{
		client:             openai.NewClient(opts...),
		apiKey:             config.APIKey,
		baseURL:            strings.TrimRight(config.BaseURL, "/"),
		useChatCompletions: shouldUseChatCompletions(config.BaseURL),
		logger:             noopLogger{},
	}
}

func shouldUseChatCompletions(baseURL string) bool {
	return strings.TrimSpace(baseURL) != ""
}

func (l *OpenAILLM) Client() openai.Client {
	return l.client
}

func (l *OpenAILLM) SetLogger(logger Logger) {
	if logger == nil {
		logger = noopLogger{}
	}
	l.logger = logger
}

func (l *OpenAILLM) Chat(ctx context.Context, messages []Message, opts ChatOptions) (LLMResponse, error) {
	traceID := TraceIDFromContext(ctx)
	if l.useChatCompletions {
		return l.chatCompletions(ctx, messages, opts)
	}
	l.log(traceID, "openai responses chat request model=%s messages=%d tools=%d", opts.Model, len(messages), len(opts.Tools))

	params, err := buildOpenAIResponseParams(messages, opts)
	if err != nil {
		l.log(traceID, "openai responses chat build_params error=%v", err)
		return LLMResponse{}, err
	}

	resp, err := l.client.Responses.New(ctx, params)
	if err != nil {
		l.log(traceID, "openai responses chat network_error=%+v", err)
		return LLMResponse{}, err
	}
	l.log(traceID, "openai responses chat response status=%s input=%d output=%d", resp.Status, resp.Usage.InputTokens, resp.Usage.OutputTokens)

	return LLMResponse{
		Content:           resp.OutputText(),
		InputTokens:       int(resp.Usage.InputTokens),
		OutputTokens:      int(resp.Usage.OutputTokens),
		CachedInputTokens: int(resp.Usage.InputTokensDetails.CachedTokens),
		StopReason:        string(resp.Status),
		ToolUses:          extractOpenAIToolUses(resp.Output),
		Raw:               resp,
	}, nil
}

func (l *OpenAILLM) Stream(ctx context.Context, messages []Message, opts ChatOptions) <-chan StreamEvent {
	ch := make(chan StreamEvent)
	traceID := TraceIDFromContext(ctx)

	go func() {
		defer close(ch)

		if l.useChatCompletions {
			resp, err := l.chatCompletions(ctx, messages, opts)
			if err != nil {
				ch <- StreamEvent{Err: err, Done: true}
				return
			}
			if resp.Content != "" {
				ch <- StreamEvent{Type: "text", Text: resp.Content}
			}
			for _, toolUse := range resp.ToolUses {
				ch <- StreamEvent{
					Type:       "tool_use",
					ToolID:     toolUse.ID,
					ToolCallID: toolUse.ID,
					ToolName:   toolUse.Name,
				}
				ch <- StreamEvent{
					Type:       "tool_use_end",
					ToolID:     toolUse.ID,
					ToolCallID: toolUse.ID,
					ToolName:   toolUse.Name,
					ToolInput:  toolUse.Input,
				}
			}
			ch <- StreamEvent{
				Type:              "usage",
				InputTokens:       resp.InputTokens,
				OutputTokens:      resp.OutputTokens,
				CachedInputTokens: resp.CachedInputTokens,
			}
			ch <- StreamEvent{Done: true}
			return
		}

		params, err := buildOpenAIResponseParams(messages, opts)
		if err != nil {
			l.log(traceID, "openai responses stream build_params error=%v", err)
			ch <- StreamEvent{Err: err, Done: true}
			return
		}

		l.log(traceID, "openai responses stream request model=%s messages=%d tools=%d", opts.Model, len(messages), len(opts.Tools))
		itemCallIDs := make(map[string]string)
		stream := l.client.Responses.NewStreaming(ctx, params)
		defer func() {
			if err := stream.Close(); err != nil {
				ch <- StreamEvent{Err: err, Done: true}
			}
		}()

		for stream.Next() {
			event := stream.Current()

			switch event.Type {
			case "response.output_text.delta":
				if delta := event.Delta; delta != "" {
					ch <- StreamEvent{Type: "text", Text: delta}
				}
			case "response.output_item.added":
				item := event.AsResponseOutputItemAdded().Item
				if item.Type == "function_call" {
					toolCall := item.AsFunctionCall()
					toolID := toolCall.ID
					if toolID == "" {
						toolID = toolCall.CallID
					}
					if toolID != "" && toolCall.CallID != "" {
						itemCallIDs[toolID] = toolCall.CallID
					}
					ch <- StreamEvent{
						Type:       "tool_use",
						ToolID:     toolID,
						ToolCallID: toolCall.CallID,
						ToolName:   toolCall.Name,
						ToolInput:  toolCall.Arguments,
					}
				}
			case "response.function_call_arguments.delta":
				deltaEvent := event.AsResponseFunctionCallArgumentsDelta()
				if deltaEvent.Delta != "" {
					ch <- StreamEvent{
						Type:      "tool_use_delta",
						ToolID:    deltaEvent.ItemID,
						ToolInput: deltaEvent.Delta,
					}
				}
			case "response.function_call_arguments.done":
				doneEvent := event.AsResponseFunctionCallArgumentsDone()
				toolCallID := itemCallIDs[doneEvent.ItemID]
				ch <- StreamEvent{
					Type:       "tool_use_end",
					ToolID:     doneEvent.ItemID,
					ToolCallID: toolCallID,
					ToolName:   doneEvent.Name,
					ToolInput:  doneEvent.Arguments,
				}
			case "response.completed":
				completed := event.AsResponseCompleted()
				usage := completed.Response.Usage
				ch <- StreamEvent{
					Type:              "usage",
					InputTokens:       int(usage.InputTokens),
					OutputTokens:      int(usage.OutputTokens),
					CachedInputTokens: int(usage.InputTokensDetails.CachedTokens),
				}
			case "error":
				message := event.Message
				if message == "" {
					message = "openai stream error"
				}
				l.log(traceID, "openai responses stream event_error=%s raw=%s", message, event.RawJSON())
				ch <- StreamEvent{Err: errors.New(message), Done: true}
				return
			}
		}

		if err := stream.Err(); err != nil {
			l.log(traceID, "openai responses stream network_error=%+v", err)
			ch <- StreamEvent{Err: err, Done: true}
			return
		}

		l.log(traceID, "openai responses stream completed")
		ch <- StreamEvent{Done: true}
	}()

	return ch
}

func (l *OpenAILLM) log(traceID string, format string, args ...any) {
	if l == nil || l.logger == nil {
		return
	}
	l.logger.Log(traceID, format, args...)
}

type chatCompletionRequest struct {
	Model       string                  `json:"model"`
	Messages    []chatCompletionMessage `json:"messages"`
	Tools       []chatCompletionTool    `json:"tools,omitempty"`
	MaxTokens   int64                   `json:"max_tokens,omitempty"`
	Temperature *float64                `json:"temperature,omitempty"`
}

type chatCompletionMessage struct {
	Role       string                   `json:"role"`
	Content    any                      `json:"content,omitempty"`
	ToolCallID string                   `json:"tool_call_id,omitempty"`
	ToolCalls  []chatCompletionToolCall `json:"tool_calls,omitempty"`
}

type chatCompletionContentPart struct {
	Type     string                         `json:"type"`
	Text     string                         `json:"text,omitempty"`
	ImageURL *chatCompletionImageURLContent `json:"image_url,omitempty"`
}

type chatCompletionImageURLContent struct {
	URL string `json:"url"`
}

type chatCompletionTool struct {
	Type     string                 `json:"type"`
	Function chatCompletionFunction `json:"function"`
}

type chatCompletionFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type chatCompletionToolCall struct {
	ID       string                     `json:"id"`
	Type     string                     `json:"type"`
	Function chatCompletionFunctionCall `json:"function"`
}

type chatCompletionFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatCompletionResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Object  string `json:"object"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Role             string                   `json:"role"`
			Content          string                   `json:"content"`
			ReasoningContent string                   `json:"reasoning_content"`
			ToolCalls        []chatCompletionToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (l *OpenAILLM) chatCompletions(ctx context.Context, messages []Message, opts ChatOptions) (LLMResponse, error) {
	traceID := TraceIDFromContext(ctx)
	if l.baseURL == "" {
		return LLMResponse{}, errors.New("openai-compatible base url is required")
	}
	requestBody, err := buildChatCompletionRequest(messages, opts)
	if err != nil {
		l.log(traceID, "openai chat_completions build_request error=%v", err)
		return LLMResponse{}, err
	}

	data, err := json.Marshal(requestBody)
	if err != nil {
		return LLMResponse{}, err
	}

	endpoint := l.baseURL + "/chat/completions"
	l.log(traceID, "openai chat_completions request endpoint=%s model=%s messages=%d tools=%d", endpoint, requestBody.Model, len(requestBody.Messages), len(requestBody.Tools))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(data))
	if err != nil {
		l.log(traceID, "openai chat_completions new_request error=%v", err)
		return LLMResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+l.apiKey)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		l.log(traceID, "openai chat_completions network_error=%+v", err)
		return LLMResponse{}, err
	}
	defer resp.Body.Close()

	respData, err := io.ReadAll(resp.Body)
	if err != nil {
		l.log(traceID, "openai chat_completions read_body error=%+v", err)
		return LLMResponse{}, err
	}
	l.log(traceID, "openai chat_completions raw_response status=%d body=%s", resp.StatusCode, string(respData))

	var decoded chatCompletionResponse
	if err := json.Unmarshal(respData, &decoded); err != nil {
		l.log(traceID, "openai chat_completions decode_error status=%d error=%+v body=%s", resp.StatusCode, err, string(respData))
		return LLMResponse{}, fmt.Errorf("decode chat completion response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message := strings.TrimSpace(string(respData))
		if decoded.Error != nil && decoded.Error.Message != "" {
			message = decoded.Error.Message
		}
		l.log(traceID, "openai chat_completions response_error status=%d message=%s", resp.StatusCode, message)
		return LLMResponse{StopReason: message, Raw: decoded}, fmt.Errorf("chat completions request failed: status=%d message=%s", resp.StatusCode, message)
	}
	if len(decoded.Choices) == 0 {
		l.log(traceID, "openai chat_completions response_error no_choices")
		return LLMResponse{Raw: decoded}, errors.New("chat completion response has no choices")
	}

	choice := decoded.Choices[0]
	l.log(traceID, "openai chat_completions response status=%d finish=%s input=%d output=%d tools=%d", resp.StatusCode, choice.FinishReason, decoded.Usage.PromptTokens, decoded.Usage.CompletionTokens, len(choice.Message.ToolCalls))
	return LLMResponse{
		Content:           choice.Message.Content,
		InputTokens:       decoded.Usage.PromptTokens,
		OutputTokens:      decoded.Usage.CompletionTokens,
		CachedInputTokens: decoded.Usage.PromptTokensDetails.CachedTokens,
		StopReason:        choice.FinishReason,
		ToolUses:          extractChatCompletionToolUses(choice.Message.ToolCalls),
		Raw:               decoded,
	}, nil
}

func buildChatCompletionRequest(messages []Message, opts ChatOptions) (chatCompletionRequest, error) {
	model := opts.Model
	if model == "" {
		model = string(defaultOpenAIModel)
	}
	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultOpenAIMaxTokens
	}

	convertedMessages := make([]chatCompletionMessage, 0, len(messages)+1)
	if opts.System != nil && strings.TrimSpace(*opts.System) != "" {
		convertedMessages = append(convertedMessages, chatCompletionMessage{
			Role:    "system",
			Content: *opts.System,
		})
	}
	for _, message := range messages {
		converted, err := toChatCompletionMessages(message)
		if err != nil {
			return chatCompletionRequest{}, err
		}
		convertedMessages = append(convertedMessages, converted...)
	}

	tools := make([]chatCompletionTool, 0, len(opts.Tools))
	for _, tool := range opts.Tools {
		converted, err := toChatCompletionTool(tool)
		if err != nil {
			return chatCompletionRequest{}, err
		}
		tools = append(tools, converted)
	}

	return chatCompletionRequest{
		Model:       model,
		Messages:    convertedMessages,
		Tools:       tools,
		MaxTokens:   maxTokens,
		Temperature: opts.Temperature,
	}, nil
}

func toChatCompletionMessages(message Message) ([]chatCompletionMessage, error) {
	role := strings.ToLower(message.Role)
	if role == "" {
		return nil, errors.New("message role is required")
	}

	result := make([]chatCompletionMessage, 0, 2)
	messageBlocks := make([]ContentBlock, 0, len(message.Content))
	flushMessage := func() error {
		if len(messageBlocks) == 0 {
			return nil
		}
		content, err := toChatCompletionContent(messageBlocks)
		if err != nil {
			return err
		}
		result = append(result, chatCompletionMessage{
			Role:    role,
			Content: content,
		})
		messageBlocks = messageBlocks[:0]
		return nil
	}

	for _, block := range message.Content {
		switch block.Type {
		case ContentTypeToolUse:
			if err := flushMessage(); err != nil {
				return nil, err
			}
			if block.ToolID == "" || block.ToolName == "" {
				return nil, errors.New("tool_use block requires tool id and name")
			}
			result = append(result, chatCompletionMessage{
				Role: "assistant",
				ToolCalls: []chatCompletionToolCall{{
					ID:   block.ToolID,
					Type: "function",
					Function: chatCompletionFunctionCall{
						Name:      block.ToolName,
						Arguments: block.ToolInput,
					},
				}},
			})
		case ContentTypeToolResult:
			if err := flushMessage(); err != nil {
				return nil, err
			}
			if block.ToolID == "" {
				return nil, errors.New("tool_result block requires tool id")
			}
			result = append(result, chatCompletionMessage{
				Role:       "tool",
				ToolCallID: block.ToolID,
				Content:    block.Text,
			})
		default:
			messageBlocks = append(messageBlocks, block)
		}
	}
	if err := flushMessage(); err != nil {
		return nil, err
	}
	return result, nil
}

func toChatCompletionContent(blocks []ContentBlock) (any, error) {
	if len(blocks) == 0 {
		return "", nil
	}
	if len(blocks) == 1 && blocks[0].Type == ContentTypeText {
		return blocks[0].Text, nil
	}

	parts := make([]chatCompletionContentPart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeText:
			if block.Text != "" {
				parts = append(parts, chatCompletionContentPart{
					Type: "text",
					Text: block.Text,
				})
			}
		case ContentTypeImage:
			imageURL := block.ImageURL
			if imageURL == "" && block.Data != "" {
				if block.MediaType == "" {
					return nil, errors.New("image block media type is required for base64 data")
				}
				imageURL = "data:" + block.MediaType + ";base64," + block.Data
			}
			if imageURL == "" {
				return nil, errors.New("image block requires image url or base64 data")
			}
			parts = append(parts, chatCompletionContentPart{
				Type: "image_url",
				ImageURL: &chatCompletionImageURLContent{
					URL: imageURL,
				},
			})
		default:
			return nil, fmt.Errorf("unsupported chat completion content block type: %s", block.Type)
		}
	}
	return parts, nil
}

func toChatCompletionTool(tool Tool) (chatCompletionTool, error) {
	name := tool.Name()
	if name == "" {
		return chatCompletionTool{}, errors.New("tool.name is required")
	}
	parameters := tool.InputSchema()
	if parameters == nil {
		return chatCompletionTool{}, errors.New("tool.input_schema must be an object")
	}
	return chatCompletionTool{
		Type: "function",
		Function: chatCompletionFunction{
			Name:        name,
			Description: tool.Description(),
			Parameters:  parameters,
		},
	}, nil
}

func extractChatCompletionToolUses(toolCalls []chatCompletionToolCall) []ToolUse {
	toolUses := make([]ToolUse, 0, len(toolCalls))
	for _, toolCall := range toolCalls {
		if toolCall.Type != "" && toolCall.Type != "function" {
			continue
		}
		toolUses = append(toolUses, ToolUse{
			ID:    toolCall.ID,
			Name:  toolCall.Function.Name,
			Input: toolCall.Function.Arguments,
		})
	}
	return toolUses
}

func buildOpenAIResponseParams(messages []Message, opts ChatOptions) (responses.ResponseNewParams, error) {
	params := responses.ResponseNewParams{
		Model: shared.ResponsesModel(opts.Model),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: make(responses.ResponseInputParam, 0, len(messages)),
		},
	}

	if params.Model == "" {
		params.Model = shared.ResponsesModel(defaultOpenAIModel)
	}

	maxTokens := opts.MaxTokens
	if maxTokens == 0 {
		maxTokens = defaultOpenAIMaxTokens
	}
	params.MaxOutputTokens = param.NewOpt(maxTokens)

	if opts.System != nil && *opts.System != "" {
		params.Instructions = param.NewOpt(*opts.System)
	}
	if opts.Temperature != nil {
		params.Temperature = param.NewOpt(*opts.Temperature)
	}
	cacheConfig := normalizePromptCacheConfig(opts.PromptCache)
	if cacheConfig.Enabled {
		params.PromptCacheKey = param.NewOpt(promptCacheKey(cacheConfig, stringPtrValue(opts.System), opts.Tools))
		switch cacheConfig.Retention {
		case PromptCacheRetention24h:
			params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetention24h
		default:
			params.PromptCacheRetention = responses.ResponseNewParamsPromptCacheRetentionInMemory
		}
	}

	for _, tool := range opts.Tools {
		convertedTool, err := toOpenAITool(tool)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Tools = append(params.Tools, convertedTool)
	}

	for _, message := range messages {
		convertedMessages, err := toOpenAIInputItems(message)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Input.OfInputItemList = append(params.Input.OfInputItemList, convertedMessages...)
	}

	return params, nil
}

func stringPtrValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func toOpenAIInputItems(message Message) ([]responses.ResponseInputItemUnionParam, error) {
	role := message.Role
	if role == "" {
		return nil, errors.New("message role is required")
	}

	items := make([]responses.ResponseInputItemUnionParam, 0, len(message.Content))
	messageBlocks := make([]ContentBlock, 0, len(message.Content))
	flushMessage := func() error {
		if len(messageBlocks) == 0 {
			return nil
		}
		content, err := toOpenAIContentList(messageBlocks)
		if err != nil {
			return err
		}
		switch strings.ToLower(role) {
		case "user", "assistant", "system", "developer":
			items = append(items, responses.ResponseInputItemParamOfMessage(
				content,
				responses.EasyInputMessageRole(strings.ToLower(role)),
			))
		default:
			return fmt.Errorf("unsupported openai message role: %s", role)
		}
		messageBlocks = messageBlocks[:0]
		return nil
	}

	for _, block := range message.Content {
		switch block.Type {
		case ContentTypeToolUse:
			if err := flushMessage(); err != nil {
				return nil, err
			}
			if block.ToolID == "" || block.ToolName == "" {
				return nil, errors.New("tool_use block requires tool id and name")
			}
			items = append(items, responses.ResponseInputItemParamOfFunctionCall(block.ToolInput, block.ToolID, block.ToolName))
		case ContentTypeToolResult:
			if err := flushMessage(); err != nil {
				return nil, err
			}
			if block.ToolID == "" {
				return nil, errors.New("tool_result block requires tool id")
			}
			items = append(items, responses.ResponseInputItemParamOfFunctionCallOutput(block.ToolID, block.Text))
		default:
			messageBlocks = append(messageBlocks, block)
		}
	}
	if err := flushMessage(); err != nil {
		return nil, err
	}
	return items, nil
}

func toOpenAIContentList(content any) (responses.ResponseInputMessageContentListParam, error) {
	blocks, ok := content.([]ContentBlock)
	if !ok {
		return nil, errors.New("message content must be []ContentBlock")
	}
	if len(blocks) == 0 {
		return nil, errors.New("message content must not be empty")
	}

	items := make(responses.ResponseInputMessageContentListParam, 0, len(blocks))
	for _, block := range blocks {
		converted, err := toOpenAIContentBlock(block)
		if err != nil {
			return nil, err
		}
		items = append(items, converted)
	}

	return items, nil
}

func toOpenAIContentBlock(block ContentBlock) (responses.ResponseInputContentUnionParam, error) {
	switch block.Type {
	case ContentTypeText:
		if block.Text == "" {
			return responses.ResponseInputContentUnionParam{}, errors.New("text block requires text")
		}
		return responses.ResponseInputContentParamOfInputText(block.Text), nil
	case ContentTypeImage:
		imageURL := block.ImageURL
		if imageURL == "" && block.Data != "" {
			if block.MediaType == "" {
				return responses.ResponseInputContentUnionParam{}, errors.New("image block media type is required for base64 data")
			}
			imageURL = "data:" + block.MediaType + ";base64," + block.Data
		}
		if imageURL == "" {
			return responses.ResponseInputContentUnionParam{}, errors.New("image block requires image url or base64 data")
		}

		content := responses.ResponseInputContentParamOfInputImage(responses.ResponseInputImageDetailAuto)
		if content.OfInputImage != nil {
			content.OfInputImage.ImageURL = param.NewOpt(imageURL)
		}
		return content, nil
	default:
		return responses.ResponseInputContentUnionParam{}, fmt.Errorf("unsupported content block type: %s", block.Type)
	}
}

func extractOpenAIToolUses(items []responses.ResponseOutputItemUnion) []ToolUse {
	toolUses := make([]ToolUse, 0)
	for _, item := range items {
		if item.Type != "function_call" {
			continue
		}
		toolUses = append(toolUses, ToolUse{
			ID:    item.CallID,
			Name:  item.Name,
			Input: item.Arguments.OfString,
		})
	}
	return toolUses
}

func toOpenAITool(tool Tool) (responses.ToolUnionParam, error) {
	name := tool.Name()
	if name == "" {
		return responses.ToolUnionParam{}, errors.New("tool.name is required")
	}

	parameters := tool.InputSchema()
	if parameters == nil {
		return responses.ToolUnionParam{}, errors.New("tool.input_schema must be an object")
	}

	strict := true
	toolParam := responses.ToolParamOfFunction(name, parameters, strict)
	if description := tool.Description(); description != "" {
		if toolParam.OfFunction != nil {
			toolParam.OfFunction.Description = param.NewOpt(description)
		}
	}

	return toolParam, nil
}
