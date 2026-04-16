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
		Content:      resp.OutputText(),
		InputTokens:  int(resp.Usage.InputTokens),
		OutputTokens: int(resp.Usage.OutputTokens),
		StopReason:   string(resp.Status),
		Raw:          resp,
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
					ch <- StreamEvent{Text: delta}
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

	for _, tool := range opts.Tools {
		convertedTool, err := toOpenAITool(tool)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Tools = append(params.Tools, convertedTool)
	}

	for _, message := range messages {
		convertedMessage, err := toOpenAIMessage(message)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Input.OfInputItemList = append(params.Input.OfInputItemList, convertedMessage)
	}

	return params, nil
}

func toOpenAIMessage(message Message) (responses.ResponseInputItemUnionParam, error) {
	role := message.Role
	if role == "" {
		return responses.ResponseInputItemUnionParam{}, errors.New("message role is required")
	}

	content, err := toOpenAIContentList(message.Content)
	if err != nil {
		return responses.ResponseInputItemUnionParam{}, err
	}

	switch strings.ToLower(role) {
	case "user", "assistant", "system", "developer":
		return responses.ResponseInputItemParamOfMessage(
			content,
			responses.EasyInputMessageRole(strings.ToLower(role)),
		), nil
	default:
		return responses.ResponseInputItemUnionParam{}, fmt.Errorf("unsupported openai message role: %s", role)
	}
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

func toOpenAITool(tool Tool) (responses.ToolUnionParam, error) {
	name, ok := tool["name"].(string)
	if !ok || name == "" {
		return responses.ToolUnionParam{}, errors.New("tool.name is required")
	}

	parameters, ok := tool["input_schema"].(map[string]any)
	if !ok {
		return responses.ToolUnionParam{}, errors.New("tool.input_schema must be an object")
	}

	strict := true
	if v, ok := tool["strict"].(bool); ok {
		strict = v
	}

	toolParam := responses.ToolParamOfFunction(name, parameters, strict)
	if description, ok := tool["description"].(string); ok && description != "" {
		if toolParam.OfFunction != nil {
			toolParam.OfFunction.Description = param.NewOpt(description)
		}
	}

	return toolParam, nil
}
