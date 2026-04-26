package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	tool "github.com/GongShichen/CodingMan/tool"
)

type Agent struct {
	mu                       sync.Mutex
	llm                      LLM
	registry                 *tool.Registry
	system                   string
	model                    string
	messages                 []Message
	turns                    int
	maxTurn                  int
	maxToolCalls             int
	maxConsecutiveToolErrors int
	maxConsecutiveAPIErrors  int
	enableToolBudget         bool
	toolBudget               ToolBudget
	enableAutoCompact        bool
	autoCompactThreshold     int
	autoCompactKeepRecent    int
	contextConfig            ContextConfig
	retryConfig              RetryConfig
	permission               *PermissionManager
	promptCache              PromptCacheConfig
}

type AgentConfig struct {
	LLM                      LLM
	Registry                 *tool.Registry
	Context                  ContextConfig
	Model                    string
	MaxTurn                  int
	MaxLLMTurns              int
	MaxToolCalls             int
	MaxConsecutiveToolErrors int
	MaxConsecutiveAPIErrors  int
	EnableToolBudget         bool
	ToolBudget               ToolBudget
	RetryConfig              RetryConfig
	Permission               PermissionConfig
	PromptCache              PromptCacheConfig
}

type ToolBudget struct {
	MaxLen  int
	HeadLen int
	TailLen int
}

const (
	defaultMaxTurn                  = 20
	defaultMaxToolCalls             = 50
	defaultMaxConsecutiveToolErrors = 3
	defaultMaxConsecutiveAPIErrors  = 3
)

func NewAgent(config AgentConfig) *Agent {
	registry := config.Registry
	if registry == nil {
		registry = tool.NewDefaultRegistry()
	}

	contextConfig := config.Context
	if contextConfig == (ContextConfig{}) {
		contextConfig = DefaultContextConfig()
	} else {
		contextConfig = normalizeContextConfig(contextConfig)
	}
	system := contextConfig.BaseSystem
	if builtSystem, err := BuildSystemPromptWithConfig(contextConfig); err == nil {
		system = builtSystem
	}
	promptCacheConfig := config.PromptCache
	if promptCacheConfig == (PromptCacheConfig{}) {
		promptCacheConfig.Enabled = true
	}

	agent := &Agent{
		llm:                      config.LLM,
		registry:                 registry,
		system:                   system,
		model:                    config.Model,
		messages:                 make([]Message, 0),
		maxTurn:                  defaultMaxTurnValue(config.MaxLLMTurns, config.MaxTurn),
		maxToolCalls:             defaultPositiveInt(config.MaxToolCalls, defaultMaxToolCalls),
		maxConsecutiveToolErrors: defaultPositiveInt(config.MaxConsecutiveToolErrors, defaultMaxConsecutiveToolErrors),
		maxConsecutiveAPIErrors:  defaultPositiveInt(config.MaxConsecutiveAPIErrors, defaultMaxConsecutiveAPIErrors),
		enableToolBudget:         config.EnableToolBudget,
		toolBudget:               config.ToolBudget,
		enableAutoCompact:        contextConfig.AutoCompact,
		autoCompactThreshold:     defaultAutoCompactThreshold(contextConfig.CompactThreshold),
		autoCompactKeepRecent:    defaultAutoCompactKeepRecent(contextConfig.KeepRecentRounds),
		contextConfig:            contextConfig,
		retryConfig:              defaultRetryConfig(config.RetryConfig),
		permission:               NewPermissionManager(config.Permission),
		promptCache:              normalizePromptCacheConfig(promptCacheConfig),
	}

	return agent
}

func defaultMaxTurnValue(value int, fallback int) int {
	if value > 0 {
		return value
	}
	if fallback > 0 {
		return fallback
	}
	return defaultMaxTurn
}

func defaultPositiveInt(value int, defaultValue int) int {
	if value > 0 {
		return value
	}
	return defaultValue
}

func NewAgentFromLLMConfig(llmConfig LLMConfig, contextConfig ContextConfig, model string) (*Agent, error) {
	llmInstance, err := CreateLLM(llmConfig)
	if err != nil {
		return nil, err
	}

	return NewAgent(AgentConfig{
		LLM:     llmInstance,
		Context: contextConfig,
		Model:   model,
	}), nil
}

func (agent *Agent) HandleStreamEvent(event StreamEvent) {
	if event.Type == "text" {
		println(event.Text)
	} else if event.Type == "tool_use_start" {
		fmt.Printf("\033[90m[%s]\033[0m\n", event.ToolName)
	}
}

func (agent *Agent) Chat(ctx context.Context, prompt string, blocks ...ContentBlock) (LLMResponse, error) {
	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		return LLMResponse{}, err
	}
	agent.autoCompactMessagesIfNeeded()

	agent.mu.Lock()
	if agent.llm == nil {
		agent.mu.Unlock()
		return LLMResponse{StopReason: "agent is nil"}, errors.New("agent llm is nil")
	}

	var system *string
	if agent.system != "" {
		system = &agent.system
	}
	messagesSnapshot := agent.snapshotMessagesLocked()
	model := agent.model
	llm := agent.llm
	tools := agent.currentToolsLocked()
	retryConfig := agent.retryConfig
	promptCache := agent.promptCache
	agent.mu.Unlock()

	resp, err := retryChat(ctx, llm, messagesSnapshot, ChatOptions{
		System:      system,
		Model:       model,
		Tools:       tools,
		PromptCache: promptCache,
	}, retryConfig)
	if err != nil {
		return LLMResponse{StopReason: err.Error()}, err
	}

	agent.appendAssistantMessage(resp.Content)

	return resp, nil
}

func (agent *Agent) RunToolLoop(ctx context.Context, prompt string, blocks ...ContentBlock) (LLMResponse, error) {
	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		return LLMResponse{}, err
	}

	toolCalls := 0
	consecutiveToolErrors := 0
	consecutiveAPIErrors := 0
	for llmTurn := 0; ; llmTurn++ {
		agent.mu.Lock()
		maxTurn := agent.maxTurn
		maxToolCalls := agent.maxToolCalls
		maxConsecutiveToolErrors := agent.maxConsecutiveToolErrors
		maxConsecutiveAPIErrors := agent.maxConsecutiveAPIErrors
		agent.mu.Unlock()
		if llmTurn >= maxTurn {
			return LLMResponse{StopReason: "maxturns"}, nil
		}
		if toolCalls >= maxToolCalls {
			return LLMResponse{StopReason: "maxtoolcalls"}, nil
		}

		agent.autoCompactMessagesIfNeeded()

		agent.mu.Lock()
		if agent.llm == nil {
			agent.mu.Unlock()
			return LLMResponse{StopReason: "agent is nil"}, errors.New("agent llm is nil")
		}

		var system *string
		if agent.system != "" {
			system = &agent.system
		}
		messagesSnapshot := agent.snapshotMessagesLocked()
		model := agent.model
		llm := agent.llm
		tools := agent.currentToolsLocked()
		retryConfig := agent.retryConfig
		promptCache := agent.promptCache
		agent.mu.Unlock()

		resp, err := retryChat(ctx, llm, messagesSnapshot, ChatOptions{
			System:      system,
			Model:       model,
			Tools:       tools,
			PromptCache: promptCache,
		}, retryConfig)
		if err != nil {
			consecutiveAPIErrors++
			if consecutiveAPIErrors >= maxConsecutiveAPIErrors {
				return LLMResponse{
					StopReason:    "apierrors",
					RetryAttempts: resp.RetryAttempts,
				}, err
			}
			return LLMResponse{StopReason: err.Error()}, err
		}
		consecutiveAPIErrors = 0

		agent.appendAssistantResponse(resp)

		toolUses := resp.ToolUses
		if len(toolUses) == 0 {
			return resp, nil
		}

		for _, toolUse := range toolUses {
			if toolCalls >= maxToolCalls {
				return LLMResponse{StopReason: "maxtoolcalls"}, nil
			}
			toolCalls++

			result, err := agent.ExecuteTool(ctx, toolUse.ToMap())
			isError := false
			if err != nil {
				result = formatToolError(toolUse, err)
				isError = true
				consecutiveToolErrors++
			} else {
				consecutiveToolErrors = 0
			}
			agent.appendToolResultMessage(toolUse, result, isError)

			if consecutiveToolErrors >= maxConsecutiveToolErrors {
				return LLMResponse{StopReason: "toolerrors"}, nil
			}
		}
	}
}

func (agent *Agent) Stream(ctx context.Context, prompt string, blocks ...ContentBlock) <-chan StreamEvent {
	streamRes := make(chan StreamEvent)

	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		go func() {
			defer close(streamRes)
			streamRes <- StreamEvent{
				Err:  err,
				Done: true,
			}
		}()
		return streamRes
	}
	agent.autoCompactMessagesIfNeeded()

	agent.mu.Lock()
	if agent.llm == nil {
		agent.mu.Unlock()
		go func() {
			defer close(streamRes)
			streamRes <- StreamEvent{
				Err:  errors.New("agent llm is nil"),
				Done: true,
			}
		}()
		return streamRes
	}

	var system *string
	if agent.system != "" {
		system = &agent.system
	}
	messagesSnapshot := agent.snapshotMessagesLocked()
	model := agent.model
	llm := agent.llm
	tools := agent.currentToolsLocked()
	promptCache := agent.promptCache
	agent.mu.Unlock()

	go func(messagesSnapshot []Message) {
		defer close(streamRes)

		var builder strings.Builder
		stream := llm.Stream(ctx, messagesSnapshot, ChatOptions{
			System:      system,
			Model:       model,
			Tools:       tools,
			PromptCache: promptCache,
		})

		for event := range stream {
			if event.Text != "" {
				builder.WriteString(event.Text)
			}

			streamRes <- event

			if event.Err != nil {
				return
			}
		}

		responseText := builder.String()
		if responseText != "" {
			agent.appendAssistantMessage(responseText)
		}
	}(messagesSnapshot)

	return streamRes
}

func (agent *Agent) Clear() {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	agent.turns = 0
	agent.messages = agent.messages[:0]
}

func (agent *Agent) appendUserMessage(text string, blocks ...ContentBlock) error {
	content := make([]ContentBlock, 0, len(blocks)+1)
	if text != "" {
		content = append(content, TextBlock(text))
	}
	content = append(content, blocks...)
	if len(content) == 0 {
		return errors.New("user message must contain text or content blocks")
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = append(agent.messages, Message{
		Role:    "user",
		Content: content,
	})
	return nil
}

func (agent *Agent) appendAssistantMessage(text string) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = append(agent.messages, Message{
		Role: "assistant",
		Content: []ContentBlock{
			TextBlock(text),
		},
	})
	agent.turns++
}

func (agent *Agent) appendAssistantResponse(resp LLMResponse) {
	content := make([]ContentBlock, 0, 1+len(resp.ToolUses))
	if resp.Content != "" {
		content = append(content, TextBlock(resp.Content))
	}
	for _, toolUse := range resp.ToolUses {
		content = append(content, ToolUseBlock(toolUse.ID, toolUse.Name, toolUse.Input))
	}
	if len(content) == 0 {
		return
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = append(agent.messages, Message{
		Role:    "assistant",
		Content: content,
	})
	agent.turns++
}

func (agent *Agent) appendToolResultMessage(toolUse ToolUse, result string, isError bool) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = append(agent.messages, Message{
		Role: "user",
		Content: []ContentBlock{
			ToolResultBlock(toolUse.ID, result, isError),
		},
	})
}

func (agent *Agent) Messages() []Message {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.snapshotMessagesLocked()
}

func (agent *Agent) Registry() *tool.Registry {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.registry
}

func (agent *Agent) SetRegistry(registry *tool.Registry) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.registry = registry
}

func (agent *Agent) Permission() *PermissionManager {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.permission
}

func (agent *Agent) PromptCache() PromptCacheConfig {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.promptCache
}

func (agent *Agent) SetPromptCache(config PromptCacheConfig) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.promptCache = normalizePromptCacheConfig(config)
}

func (agent *Agent) SetPromptCacheEnabled(enabled bool) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.promptCache.Enabled = enabled
}

func (agent *Agent) snapshotMessagesLocked() []Message {
	snapshot := make([]Message, 0, len(agent.messages))
	for _, message := range agent.messages {
		content := append([]ContentBlock(nil), message.Content...)
		snapshot = append(snapshot, Message{
			Role:    message.Role,
			Content: content,
		})
	}
	return snapshot
}

func (agent *Agent) currentToolsLocked() []tool.Tool {
	if agent.registry == nil {
		return nil
	}
	return agent.registry.Tools()
}

func (agent *Agent) autoCompactMessagesIfNeeded() {
	agent.mu.Lock()
	enabled := agent.enableAutoCompact
	threshold := agent.autoCompactThreshold
	keepRecent := agent.autoCompactKeepRecent
	llm := agent.llm
	toolBudget := agent.toolBudget
	if !enabled || llm == nil || threshold <= 0 || EstimateMessagesSize(agent.messages) <= threshold {
		agent.mu.Unlock()
		return
	}
	messagesSnapshot := agent.snapshotMessagesLocked()
	agent.mu.Unlock()

	compacted := CompactMessagesWithOptions(messagesSnapshot, llm, CompactOptions{
		KeepRecentRounds: keepRecent,
		ToolBudget:       toolBudget,
	})
	if len(compacted) == 0 || len(compacted) >= len(messagesSnapshot) {
		return
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.messages) != len(messagesSnapshot) {
		return
	}
	agent.messages = cloneMessages(compacted)
}

func (agent *Agent) ExecuteTool(ctx context.Context, toolUse map[string]any) (string, error) {
	if toolUse == nil {
		return "", errors.New("tool use is nil")
	}

	name, ok := stringFromMap(toolUse, "name", "toolName", "tool_name")
	if !ok || name == "" {
		return "", errors.New("tool name is required")
	}

	input, err := toolInputFromMap(toolUse)
	if err != nil {
		return "", err
	}

	toolUseID, _ := stringFromMap(toolUse, "id", "toolUseID", "tool_use_id")

	agent.mu.Lock()
	registry := agent.registry
	enableToolBudget := agent.enableToolBudget
	toolBudget := agent.toolBudget
	permission := agent.permission
	agent.mu.Unlock()

	if registry == nil {
		return "", errors.New("tool registry is nil")
	}

	if permission != nil {
		if err := permission.Check(ctx, PermissionRequest{
			ToolUseID: toolUseID,
			ToolName:  name,
			ToolInput: input,
		}); err != nil {
			return "", err
		}
	}

	result, err := registry.Call(name, input)
	if err != nil {
		return "", err
	}

	if enableToolBudget {
		return tool.TruncateToolResult(result, toolBudget.MaxLen, toolBudget.HeadLen, toolBudget.TailLen)
	}
	return result, nil
}

func formatToolError(toolUse ToolUse, err error) string {
	return fmt.Sprintf(`[TOOL_ERROR]
tool: %s
tool_use_id: %s
code: %s
recoverable: true
message: %s`, toolUse.Name, toolUse.ID, classifyToolError(err), err.Error())
}

func classifyToolError(err error) string {
	if err == nil {
		return "unknown"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "tool not found"):
		return "tool_not_found"
	case strings.Contains(text, "required") ||
		strings.Contains(text, "invalid") ||
		strings.Contains(text, "illegal") ||
		strings.Contains(text, "decode") ||
		strings.Contains(text, "must be"):
		return "invalid_input"
	case strings.Contains(text, "not found") ||
		strings.Contains(text, "no such file"):
		return "not_found"
	case strings.Contains(text, "permission denied") ||
		strings.Contains(text, "operation not permitted"):
		return "permission_denied"
	case strings.Contains(text, "timed out") ||
		strings.Contains(text, "timeout"):
		return "timeout"
	default:
		return "execution_error"
	}
}

func stringFromMap(values map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		value, ok := values[key].(string)
		if ok {
			return value, true
		}
	}
	return "", false
}

func toolInputFromMap(toolUse map[string]any) (map[string]any, error) {
	for _, key := range []string{"input", "arguments", "args"} {
		value, exists := toolUse[key]
		if !exists {
			continue
		}

		switch v := value.(type) {
		case map[string]any:
			return v, nil
		case string:
			if v == "" {
				return map[string]any{}, nil
			}
			var input map[string]any
			if err := json.Unmarshal([]byte(v), &input); err != nil {
				return nil, fmt.Errorf("decode tool input: %w", err)
			}
			return input, nil
		default:
			return nil, errors.New("tool input must be an object or JSON object string")
		}
	}

	return map[string]any{}, nil
}
