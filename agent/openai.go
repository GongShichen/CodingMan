package agent

import (
	"context"
	"errors"
	"fmt"
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
	client openai.Client
}

func NewOpenAILLM(config ClientConfig, opts ...openaioption.RequestOption) *OpenAILLM {
	if config.APIKey != "" {
		opts = append(opts, openaioption.WithAPIKey(config.APIKey))
	}
	if config.BaseURL != "" {
		opts = append(opts, openaioption.WithBaseURL(config.BaseURL))
	}

	return &OpenAILLM{
		client: openai.NewClient(opts...),
	}
}

func (l *OpenAILLM) Client() openai.Client {
	return l.client
}

func (l *OpenAILLM) Chat(ctx context.Context, messages []Message, opts ChatOptions) (LLMResponse, error) {
	params, err := buildOpenAIResponseParams(messages, opts)
	if err != nil {
		return LLMResponse{}, err
	}

	resp, err := l.client.Responses.New(ctx, params)
	if err != nil {
		return LLMResponse{}, err
	}

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

	go func() {
		defer close(ch)

		params, err := buildOpenAIResponseParams(messages, opts)
		if err != nil {
			ch <- StreamEvent{Err: err, Done: true}
			return
		}

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
					ch <- StreamEvent{
						Type:      "tool_use",
						ToolID:    toolCall.CallID,
						ToolName:  toolCall.Name,
						ToolInput: toolCall.Arguments,
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
				ch <- StreamEvent{
					Type:      "tool_use_end",
					ToolID:    doneEvent.ItemID,
					ToolName:  doneEvent.Name,
					ToolInput: doneEvent.Arguments,
				}
			case "error":
				message := event.Message
				if message == "" {
					message = "openai stream error"
				}
				ch <- StreamEvent{Err: errors.New(message), Done: true}
				return
			}
		}

		if err := stream.Err(); err != nil {
			ch <- StreamEvent{Err: err, Done: true}
			return
		}

		ch <- StreamEvent{Done: true}
	}()

	return ch
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
