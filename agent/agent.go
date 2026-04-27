package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tool "github.com/GongShichen/CodingMan/tool"
)

type Agent struct {
	mu                       sync.Mutex
	id                       string
	name                     string
	parentID                 string
	depth                    int
	llm                      LLM
	registry                 *tool.Registry
	system                   string
	model                    string
	messages                 []Message
	fileHistory              []FileHistoryEntry
	attribution              []AttributionEntry
	todos                    []TodoItem
	turns                    int
	maxTurn                  int
	maxToolCalls             int
	maxParallelToolCalls     int
	maxConsecutiveToolErrors int
	maxConsecutiveAPIErrors  int
	maxConcurrentSubAgents   int
	enableToolBudget         bool
	toolBudget               ToolBudget
	enableAutoCompact        bool
	autoCompactThreshold     int
	autoCompactKeepRecent    int
	contextConfig            ContextConfig
	retryConfig              RetryConfig
	permission               *PermissionManager
	promptCache              PromptCacheConfig
	enableStreamRecovery     bool
	streamRecoveryMaxRetries int
	logger                   Logger
	a2a                      *A2ABus
	coordinator              *Coordinator
	coordinationConfig       CoordinationConfig
	hooks                    *HookManager
	nextChildIndex           atomic.Uint64
	maxSubAgentDepth         int
	enableSelfReflection     bool
}

type AgentConfig struct {
	LLM                      LLM
	Registry                 *tool.Registry
	Context                  ContextConfig
	Model                    string
	MaxTurn                  int
	MaxLLMTurns              int
	MaxToolCalls             int
	MaxParallelToolCalls     int
	MaxConsecutiveToolErrors int
	MaxConsecutiveAPIErrors  int
	MaxConcurrentSubAgents   int
	EnableToolBudget         bool
	ToolBudget               ToolBudget
	RetryConfig              RetryConfig
	Permission               PermissionConfig
	PermissionManager        *PermissionManager
	PromptCache              PromptCacheConfig
	EnableStreamRecovery     *bool
	StreamRecoveryMaxRetries int
	Logger                   Logger
	A2ABus                   *A2ABus
	Coordination             CoordinationConfig
	Hooks                    *HookManager
	ID                       string
	Name                     string
	ParentID                 string
	Depth                    int
	MaxSubAgentDepth         int
	EnableSelfReflection     *bool
}

type ToolBudget struct {
	MaxLen  int
	HeadLen int
	TailLen int
}

const (
	defaultMaxTurn                  = 20
	defaultMaxToolCalls             = 50
	defaultMaxParallelToolCalls     = 4
	defaultMaxConsecutiveToolErrors = 3
	defaultMaxConsecutiveAPIErrors  = 3
	defaultMaxSubAgentDepth         = 1
	defaultMaxConcurrentSubAgents   = 4
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
	permissionManager := config.PermissionManager
	if permissionManager == nil {
		permissionConfig := config.Permission
		if isZeroPermissionConfig(permissionConfig) {
			permissionConfig = DefaultPermissionConfig()
		}
		permissionManager = NewPermissionManager(permissionConfig)
	}
	logger := config.Logger
	if logger == nil {
		logger = noopLogger{}
	}
	if loggerAware, ok := config.LLM.(LoggerAware); ok {
		loggerAware.SetLogger(logger)
	}

	id := strings.TrimSpace(config.ID)
	if id == "" {
		id = "main"
	}
	name := strings.TrimSpace(config.Name)
	if name == "" {
		name = id
	}
	a2aBus := config.A2ABus
	if a2aBus == nil {
		a2aBus = NewA2ABus()
	}

	agent := &Agent{
		id:                       id,
		name:                     name,
		parentID:                 strings.TrimSpace(config.ParentID),
		depth:                    config.Depth,
		llm:                      config.LLM,
		registry:                 registry,
		system:                   system,
		model:                    config.Model,
		messages:                 make([]Message, 0),
		fileHistory:              make([]FileHistoryEntry, 0),
		attribution:              make([]AttributionEntry, 0),
		todos:                    make([]TodoItem, 0),
		maxTurn:                  defaultMaxTurnValue(config.MaxLLMTurns, config.MaxTurn),
		maxToolCalls:             defaultPositiveInt(config.MaxToolCalls, defaultMaxToolCalls),
		maxParallelToolCalls:     defaultPositiveInt(config.MaxParallelToolCalls, defaultMaxParallelToolCalls),
		maxConsecutiveToolErrors: defaultPositiveInt(config.MaxConsecutiveToolErrors, defaultMaxConsecutiveToolErrors),
		maxConsecutiveAPIErrors:  defaultPositiveInt(config.MaxConsecutiveAPIErrors, defaultMaxConsecutiveAPIErrors),
		maxConcurrentSubAgents:   defaultPositiveInt(config.MaxConcurrentSubAgents, defaultMaxConcurrentSubAgents),
		enableToolBudget:         config.EnableToolBudget,
		toolBudget:               config.ToolBudget,
		enableAutoCompact:        contextConfig.AutoCompact,
		autoCompactThreshold:     defaultAutoCompactThreshold(contextConfig.CompactThreshold),
		autoCompactKeepRecent:    defaultAutoCompactKeepRecent(contextConfig.KeepRecentRounds),
		contextConfig:            contextConfig,
		retryConfig:              defaultRetryConfig(config.RetryConfig),
		permission:               permissionManager,
		promptCache:              normalizePromptCacheConfig(promptCacheConfig),
		enableStreamRecovery:     defaultBool(config.EnableStreamRecovery, true),
		streamRecoveryMaxRetries: defaultStreamRecoveryMaxRetries(config.StreamRecoveryMaxRetries, config.RetryConfig.MaxRetries),
		logger:                   logger,
		a2a:                      a2aBus,
		coordinationConfig:       config.Coordination,
		hooks:                    config.Hooks,
		maxSubAgentDepth:         defaultPositiveInt(config.MaxSubAgentDepth, defaultMaxSubAgentDepth),
		enableSelfReflection:     defaultBool(config.EnableSelfReflection, true),
	}
	agent.a2a.RegisterAgent(agent.id, agent.parentID)
	agent.coordinator = NewCoordinator(agent, config.Coordination)
	agent.registerInternalTools()

	return agent
}

func defaultStreamRecoveryMaxRetries(value int, retryMaxRetries int) int {
	if value > 0 {
		return value
	}
	if retryMaxRetries > 0 {
		return retryMaxRetries
	}
	return defaultMaxRetries
}

func defaultBool(value *bool, defaultValue bool) bool {
	if value == nil {
		return defaultValue
	}
	return *value
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

func (agent *Agent) registerInternalTools() {
	if agent == nil || agent.registry == nil {
		return
	}
	if agent.depth > 0 {
		return
	}
	if err := agent.registry.Register(newSubAgentTool(agent)); err != nil && !errors.Is(err, tool.ErrToolAlreadyRegistered) {
		agent.log("", "register_internal_tool subagent error=%v", err)
	}
}

func (agent *Agent) HandleStreamEvent(event StreamEvent) {
	if event.Type == "text" {
		println(event.Text)
	} else if event.Type == "tool_use_start" {
		fmt.Printf("\033[90m[%s]\033[0m\n", event.ToolName)
	}
}

func (agent *Agent) SetLogger(logger Logger) {
	if logger == nil {
		logger = noopLogger{}
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.logger = logger
	if loggerAware, ok := agent.llm.(LoggerAware); ok {
		loggerAware.SetLogger(logger)
	}
}

func (agent *Agent) log(traceID string, format string, args ...any) {
	agent.mu.Lock()
	logger := agent.logger
	agent.mu.Unlock()
	if logger == nil {
		return
	}
	logger.Log(traceID, format, args...)
}

func (agent *Agent) Chat(ctx context.Context, prompt string, blocks ...ContentBlock) (LLMResponse, error) {
	ctx, traceID := ensureTrace(ctx)
	agent.log(traceID, "chat start prompt_chars=%d blocks=%d", len(prompt), len(blocks))
	agent.log(traceID, "user message:\n%s", formatContentForLog(prompt, blocks))
	defer agent.log(traceID, "chat end")
	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		agent.log(traceID, "chat append_user error=%v", err)
		return LLMResponse{}, err
	}
	agent.autoCompactMessagesIfNeeded(ctx)

	agent.mu.Lock()
	if agent.llm == nil {
		agent.mu.Unlock()
		agent.log(traceID, "chat error=agent llm is nil")
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
	agent.log(traceID, "chat llm_request model=%s messages=%d tools=%d", model, len(messagesSnapshot), len(tools))

	resp, err := retryChat(ctx, llm, messagesSnapshot, ChatOptions{
		System:      system,
		Model:       model,
		Tools:       tools,
		PromptCache: promptCache,
	}, retryConfig)
	if err != nil {
		agent.log(traceID, "chat llm_error retry=%d error=%v", resp.RetryAttempts, err)
		return LLMResponse{StopReason: err.Error()}, err
	}
	agent.log(traceID, "chat llm_response stop=%s input=%d output=%d tools=%d retry=%d", resp.StopReason, resp.InputTokens, resp.OutputTokens, len(resp.ToolUses), resp.RetryAttempts)
	agent.log(traceID, "assistant message:\n%s", formatAssistantResponseForLog(resp))

	agent.appendAssistantMessage(resp.Content)

	return resp, nil
}

func (agent *Agent) Plan(ctx context.Context, prompt string, blocks ...ContentBlock) (LLMResponse, error) {
	ctx, traceID := ensureTrace(ctx)
	agent.log(traceID, "plan start prompt_chars=%d blocks=%d", len(prompt), len(blocks))
	agent.log(traceID, "plan user message:\n%s", formatContentForLog(prompt, blocks))
	defer agent.log(traceID, "plan end")

	content := make([]ContentBlock, 0, len(blocks)+1)
	planPrompt := strings.TrimSpace(prompt)
	if planPrompt != "" {
		content = append(content, TextBlock("Plan mode: propose a concise implementation plan for the following request. Do not call tools and do not modify files. Include verification steps and risks.\n\n"+planPrompt))
	}
	content = append(content, blocks...)
	if len(content) == 0 {
		return LLMResponse{}, errors.New("plan request must contain text or content blocks")
	}

	agent.mu.Lock()
	if agent.llm == nil {
		agent.mu.Unlock()
		agent.log(traceID, "plan error=agent llm is nil")
		return LLMResponse{StopReason: "agent is nil"}, errors.New("agent llm is nil")
	}

	var system *string
	if agent.system != "" {
		planSystem := agent.system + "\n\n## Plan Mode\nYou are in plan mode. Produce a practical plan only. Do not request or execute tool calls."
		system = &planSystem
	}
	messagesSnapshot := agent.snapshotMessagesLocked()
	messagesSnapshot = append(messagesSnapshot, Message{
		Role:    "user",
		Content: content,
	})
	model := agent.model
	llm := agent.llm
	retryConfig := agent.retryConfig
	promptCache := agent.promptCache
	agent.mu.Unlock()
	agent.log(traceID, "plan llm_request model=%s messages=%d", model, len(messagesSnapshot))

	resp, err := retryChat(ctx, llm, messagesSnapshot, ChatOptions{
		System:      system,
		Model:       model,
		PromptCache: promptCache,
	}, retryConfig)
	if err != nil {
		agent.log(traceID, "plan llm_error retry=%d error=%v", resp.RetryAttempts, err)
		return LLMResponse{StopReason: err.Error()}, err
	}
	agent.log(traceID, "plan llm_response stop=%s input=%d output=%d retry=%d", resp.StopReason, resp.InputTokens, resp.OutputTokens, resp.RetryAttempts)
	agent.log(traceID, "plan assistant message:\n%s", formatAssistantResponseForLog(resp))
	return resp, nil
}

func (agent *Agent) RunToolLoop(ctx context.Context, prompt string, blocks ...ContentBlock) (LLMResponse, error) {
	ctx, traceID := ensureTrace(ctx)
	defer func() {
		agent.runHooks(ctx, HookPayload{Event: HookEventStop, AgentID: agent.ID(), TraceID: traceID, Message: "tool loop stopped"})
	}()
	agent.log(traceID, "tool_loop start prompt_chars=%d blocks=%d", len(prompt), len(blocks))
	agent.log(traceID, "user message:\n%s", formatContentForLog(prompt, blocks))
	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		agent.log(traceID, "tool_loop append_user error=%v", err)
		return LLMResponse{}, err
	}

	toolCalls := 0
	consecutiveToolErrors := 0
	consecutiveAPIErrors := 0
	for llmTurn := 0; ; llmTurn++ {
		agent.mu.Lock()
		maxTurn := agent.maxTurn
		maxToolCalls := agent.maxToolCalls
		maxParallelToolCalls := agent.maxParallelToolCalls
		maxConsecutiveToolErrors := agent.maxConsecutiveToolErrors
		maxConsecutiveAPIErrors := agent.maxConsecutiveAPIErrors
		agent.mu.Unlock()
		if llmTurn >= maxTurn {
			agent.log(traceID, "tool_loop stop=maxturns turn=%d max=%d", llmTurn, maxTurn)
			return LLMResponse{StopReason: "maxturns"}, nil
		}
		if toolCalls >= maxToolCalls {
			agent.log(traceID, "tool_loop stop=maxtoolcalls count=%d max=%d", toolCalls, maxToolCalls)
			return LLMResponse{StopReason: "maxtoolcalls"}, nil
		}

		agent.autoCompactMessagesIfNeeded(ctx)

		agent.mu.Lock()
		if agent.llm == nil {
			agent.mu.Unlock()
			agent.log(traceID, "tool_loop error=agent llm is nil")
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
		enableStreamRecovery := agent.enableStreamRecovery
		streamRecoveryMaxRetries := agent.streamRecoveryMaxRetries
		agent.mu.Unlock()
		agent.log(traceID, "tool_loop llm_turn=%d request model=%s messages=%d tools=%d tool_calls=%d", llmTurn, model, len(messagesSnapshot), len(tools), toolCalls)

		resp, results, err := agent.runStreamingToolTurn(ctx, llm, messagesSnapshot, ChatOptions{
			System:      system,
			Model:       model,
			Tools:       tools,
			PromptCache: promptCache,
		}, maxParallelToolCalls, maxToolCalls-toolCalls, StreamRecoveryConfig{
			Enabled:    enableStreamRecovery,
			MaxRetries: streamRecoveryMaxRetries,
			Retry:      retryConfig,
		})
		if err != nil {
			consecutiveAPIErrors++
			agent.log(traceID, "tool_loop llm_turn=%d api_error consecutive=%d max=%d error=%v", llmTurn, consecutiveAPIErrors, maxConsecutiveAPIErrors, err)
			if consecutiveAPIErrors >= maxConsecutiveAPIErrors {
				agent.log(traceID, "tool_loop stop=apierrors")
				return LLMResponse{
					StopReason: "apierrors",
				}, err
			}
			return LLMResponse{StopReason: err.Error()}, err
		}
		consecutiveAPIErrors = 0

		agent.appendAssistantResponse(resp)
		agent.log(traceID, "tool_loop llm_turn=%d response stop=%s input=%d output=%d tools=%d retry=%d", llmTurn, resp.StopReason, resp.InputTokens, resp.OutputTokens, len(resp.ToolUses), resp.RetryAttempts)
		agent.log(traceID, "assistant message:\n%s", formatAssistantResponseForLog(resp))

		toolUses := resp.ToolUses
		if len(toolUses) == 0 {
			agent.log(traceID, "tool_loop completed turns=%d total_tool_calls=%d", llmTurn+1, toolCalls)
			return resp, nil
		}

		toolCalls += len(toolUses)
		agent.appendToolResultMessages(results)
		agent.log(traceID, "tool_loop tool_results count=%d total_tool_calls=%d", len(results), toolCalls)
		for _, result := range results {
			agent.log(traceID, "tool result id=%s name=%s error=%v:\n%s", result.ToolUse.ID, result.ToolUse.Name, result.IsError, result.Content)
		}
		for _, result := range results {
			if result.IsError {
				consecutiveToolErrors++
			} else {
				consecutiveToolErrors = 0
			}
			if consecutiveToolErrors >= maxConsecutiveToolErrors {
				agent.log(traceID, "tool_loop stop=toolerrors consecutive=%d", consecutiveToolErrors)
				return LLMResponse{StopReason: "toolerrors"}, nil
			}
		}
	}
}

func (agent *Agent) Stream(ctx context.Context, prompt string, blocks ...ContentBlock) <-chan StreamEvent {
	streamRes := make(chan StreamEvent)
	ctx, traceID := ensureTrace(ctx)
	agent.log(traceID, "stream start prompt_chars=%d blocks=%d", len(prompt), len(blocks))
	agent.log(traceID, "user message:\n%s", formatContentForLog(prompt, blocks))

	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		go func() {
			defer close(streamRes)
			streamRes <- StreamEvent{
				Err:  err,
				Done: true,
			}
			agent.log(traceID, "stream append_user error=%v", err)
		}()
		return streamRes
	}
	agent.autoCompactMessagesIfNeeded(ctx)

	agent.mu.Lock()
	if agent.llm == nil {
		agent.mu.Unlock()
		go func() {
			defer close(streamRes)
			streamRes <- StreamEvent{
				Err:  errors.New("agent llm is nil"),
				Done: true,
			}
			agent.log(traceID, "stream error=agent llm is nil")
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
	retryConfig := agent.retryConfig
	agent.mu.Unlock()

	go func(messagesSnapshot []Message) {
		defer close(streamRes)

		var builder strings.Builder
		opts := ChatOptions{
			System:      system,
			Model:       model,
			Tools:       tools,
			PromptCache: promptCache,
		}
		retryConfig := defaultRetryConfig(retryConfig)
		delay := retryConfig.InitialDelay
		streamFailed := false

		for attempt := 0; ; attempt++ {
			emitted := false
			agent.log(traceID, "stream attempt=%d request model=%s messages=%d tools=%d", attempt, model, len(messagesSnapshot), len(tools))
			stream := llm.Stream(ctx, messagesSnapshot, opts)
			shouldRetry := false

			for event := range stream {
				if event.Err != nil {
					if !emitted && attempt < retryConfig.MaxRetries && retryConfig.Retryable(event.Err) {
						agent.log(traceID, "stream attempt=%d retryable_error=%v", attempt, event.Err)
						shouldRetry = true
						break
					}
					streamFailed = true
					agent.log(traceID, "stream attempt=%d error=%v", attempt, event.Err)
					streamRes <- event
					break
				}
				if event.Text != "" {
					builder.WriteString(event.Text)
					emitted = true
				}

				streamRes <- event
			}
			if !shouldRetry {
				break
			}
			if err := sleepWithContext(ctx, jitterDelay(delay, retryConfig.Jitter)); err != nil {
				streamRes <- StreamEvent{Err: err, Done: true}
				agent.log(traceID, "stream retry_sleep error=%v", err)
				return
			}
			delay = time.Duration(math.Round(float64(delay) * retryConfig.Multiplier))
			if delay > retryConfig.MaxDelay {
				delay = retryConfig.MaxDelay
			}
		}
		if streamFailed {
			agent.log(traceID, "stream failed")
			return
		}

		responseText := builder.String()
		if responseText != "" {
			agent.appendAssistantMessage(responseText)
			agent.log(traceID, "assistant message:\n%s", responseText)
		}
		agent.log(traceID, "stream completed response_chars=%d", len(responseText))
	}(messagesSnapshot)

	return streamRes
}

func (agent *Agent) Clear() {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	agent.turns = 0
	agent.messages = agent.messages[:0]
}

func (agent *Agent) ID() string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.id
}

func (agent *Agent) A2AMessages() []A2AMessage {
	agent.mu.Lock()
	bus := agent.a2a
	agent.mu.Unlock()
	return bus.Messages()
}

func (agent *Agent) Coordinator() *Coordinator {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.coordinator
}

func (agent *Agent) Hooks() *HookManager {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.hooks
}

func (agent *Agent) SystemPrompt() string {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.system
}

func (agent *Agent) SetBaseSystemPrompt(baseSystem string) error {
	if strings.TrimSpace(baseSystem) == "" {
		return errors.New("system prompt must not be empty")
	}

	agent.mu.Lock()
	contextConfig := agent.contextConfig
	contextConfig.BaseSystem = baseSystem
	agent.mu.Unlock()

	system, err := BuildSystemPromptWithConfig(contextConfig)
	if err != nil {
		return err
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.contextConfig = normalizeContextConfig(contextConfig)
	agent.system = system
	return nil
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
	agent.appendToolResultMessages([]ToolResult{{
		ToolUse: toolUse,
		Content: result,
		IsError: isError,
	}})
}

func (agent *Agent) appendToolResultMessages(results []ToolResult) {
	if len(results) == 0 {
		return
	}
	content := make([]ContentBlock, 0, len(results))
	for _, result := range results {
		content = append(content, ToolResultBlock(result.ToolUse.ID, result.Content, result.IsError))
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = append(agent.messages, Message{
		Role:    "user",
		Content: content,
	})
}

func (agent *Agent) Messages() []Message {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.snapshotMessagesLocked()
}

func (agent *Agent) Snapshot(sessionID string, projectDir string) SessionSnapshot {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return SessionSnapshot{
		SessionID:   sessionID,
		ProjectDir:  projectDir,
		Messages:    agent.snapshotMessagesLocked(),
		FileHistory: append([]FileHistoryEntry(nil), agent.fileHistory...),
		Attribution: append([]AttributionEntry(nil), agent.attribution...),
		Todos:       append([]TodoItem(nil), agent.todos...),
		UpdatedAt:   time.Now().UTC(),
	}
}

func (agent *Agent) Restore(snapshot SessionSnapshot) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = cloneMessages(snapshot.Messages)
	agent.fileHistory = append([]FileHistoryEntry(nil), snapshot.FileHistory...)
	agent.attribution = append([]AttributionEntry(nil), snapshot.Attribution...)
	agent.todos = append([]TodoItem(nil), snapshot.Todos...)
	agent.turns = countAssistantTurns(agent.messages)
}

func countAssistantTurns(messages []Message) int {
	count := 0
	for _, message := range messages {
		if message.Role == "assistant" {
			count++
		}
	}
	return count
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

func (agent *Agent) autoCompactMessagesIfNeeded(ctx context.Context) {
	traceID := TraceIDFromContext(ctx)
	agent.mu.Lock()
	enabled := agent.enableAutoCompact
	threshold := agent.autoCompactThreshold
	keepRecent := agent.autoCompactKeepRecent
	llm := agent.llm
	toolBudget := agent.toolBudget
	model := agent.model
	promptCache := agent.promptCache
	var system *string
	if agent.system != "" {
		systemValue := agent.system
		system = &systemValue
	}
	if !enabled || llm == nil || threshold <= 0 || EstimateMessagesSize(agent.messages) <= threshold {
		agent.mu.Unlock()
		return
	}
	beforeSize := EstimateMessagesSize(agent.messages)
	messagesSnapshot := agent.snapshotMessagesLocked()
	agent.mu.Unlock()
	agent.log(traceID, "compact start messages=%d size=%d threshold=%d keep_recent=%d", len(messagesSnapshot), beforeSize, threshold, keepRecent)

	compacted := CompactMessagesWithOptions(messagesSnapshot, llm, CompactOptions{
		KeepRecentRounds: keepRecent,
		ToolBudget:       toolBudget,
		Context:          ctx,
		ChatOptions: ChatOptions{
			System:      system,
			Model:       model,
			PromptCache: promptCache,
		},
	})
	if len(compacted) == 0 || len(compacted) >= len(messagesSnapshot) {
		agent.log(traceID, "compact skipped before=%d after=%d", len(messagesSnapshot), len(compacted))
		return
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	if len(agent.messages) != len(messagesSnapshot) {
		agent.log(traceID, "compact skipped reason=messages_changed")
		return
	}
	agent.messages = cloneMessages(compacted)
	agent.log(traceID, "compact completed before=%d after=%d", len(messagesSnapshot), len(compacted))
}

func (agent *Agent) ExecuteTool(ctx context.Context, toolUse map[string]any) (string, error) {
	traceID := TraceIDFromContext(ctx)
	agent.log(traceID, "execute_tool start")
	prepared, err := agent.prepareToolExecution(ctx, toolUse)
	if err != nil {
		agent.log(traceID, "execute_tool prepare_error=%v", err)
		return "", err
	}
	result := ToolResult{ToolUse: prepared.toolUse}
	agent.executePreparedToolIntoResult(prepared, &result)
	if result.IsError {
		err := errors.New(result.Content)
		agent.log(traceID, "execute_tool name=%s error=%v", prepared.name, err)
		return result.Content, err
	}
	agent.log(traceID, "execute_tool name=%s result_chars=%d", prepared.name, len(result.Content))
	return result.Content, nil
}

func (agent *Agent) ExecuteTools(ctx context.Context, toolUses []ToolUse, maxParallel int) []ToolResult {
	traceID := TraceIDFromContext(ctx)
	agent.log(traceID, "execute_tools start count=%d max_parallel=%d", len(toolUses), maxParallel)
	results := make([]ToolResult, len(toolUses))
	preparedTools := make([]preparedToolExecution, len(toolUses))

	for i, toolUse := range toolUses {
		results[i].ToolUse = toolUse
		prepared, err := agent.prepareToolExecution(ctx, toolUse.ToMap())
		if err != nil {
			agent.log(traceID, "execute_tools prepare_error index=%d tool=%s error=%v", i, toolUse.Name, err)
			results[i].Content = formatToolError(toolUse, err)
			results[i].IsError = true
			continue
		}
		preparedTools[i] = prepared
	}

	batches := buildToolExecutionBatches(preparedTools, results)
	agent.log(traceID, "execute_tools batches=%d", len(batches))
	for _, batch := range batches {
		if batch.parallel {
			agent.executePreparedToolBatch(ctx, toolUses, preparedTools, results, batch.start, batch.end, maxParallel)
			continue
		}
		if results[batch.start].IsError {
			continue
		}
		agent.executePreparedToolIntoResult(preparedTools[batch.start], &results[batch.start])
	}
	agent.log(traceID, "execute_tools completed count=%d", len(results))
	return results
}

func (agent *Agent) runStreamingToolTurn(ctx context.Context, llm LLM, messages []Message, opts ChatOptions, maxParallel int, maxToolCalls int, recovery StreamRecoveryConfig) (LLMResponse, []ToolResult, error) {
	traceID := TraceIDFromContext(ctx)
	recovery.Retry = defaultRetryConfig(recovery.Retry)
	if recovery.MaxRetries <= 0 {
		recovery.MaxRetries = recovery.Retry.MaxRetries
	}
	if !recovery.Enabled {
		recovery.MaxRetries = 0
	}

	var lastErr error
	delay := recovery.Retry.InitialDelay
	for attempt := 0; attempt <= recovery.MaxRetries; attempt++ {
		agent.log(traceID, "streaming_tool_turn attempt=%d max_retries=%d", attempt, recovery.MaxRetries)
		resp, results, retryable, err := agent.runStreamingToolTurnAttempt(ctx, llm, messages, opts, maxParallel, maxToolCalls)
		resp.RetryAttempts = attempt
		if err == nil {
			if attempt > 0 {
				resp.StopReason = "stream_recovered"
			}
			agent.log(traceID, "streaming_tool_turn attempt=%d success tools=%d results=%d", attempt, len(resp.ToolUses), len(results))
			return resp, results, nil
		}
		lastErr = err
		agent.log(traceID, "streaming_tool_turn attempt=%d error=%v retryable=%v", attempt, err, retryable)
		if !retryable || attempt == recovery.MaxRetries || !recovery.Retry.Retryable(err) {
			if resp.StopReason == "" {
				resp.StopReason = "stream_interrupted"
			}
			resp.RetryAttempts = attempt
			return resp, results, err
		}
		agent.log(traceID, "streaming_tool_turn retry_sleep=%s", delay)
		if err := sleepWithContext(ctx, jitterDelay(delay, recovery.Retry.Jitter)); err != nil {
			return LLMResponse{StopReason: "stream_interrupted", RetryAttempts: attempt}, nil, err
		}
		delay = time.Duration(math.Round(float64(delay) * recovery.Retry.Multiplier))
		if delay > recovery.Retry.MaxDelay {
			delay = recovery.Retry.MaxDelay
		}
	}

	return LLMResponse{StopReason: "stream_interrupted"}, nil, lastErr
}

func (agent *Agent) runStreamingToolTurnAttempt(ctx context.Context, llm LLM, messages []Message, opts ChatOptions, maxParallel int, maxToolCalls int) (LLMResponse, []ToolResult, bool, error) {
	traceID := TraceIDFromContext(ctx)
	stream := llm.Stream(ctx, messages, opts)
	scheduler := newStreamingToolScheduler(agent, ctx, maxParallel)
	defer scheduler.close()

	var content strings.Builder
	var usage streamUsage
	toolUses := make([]ToolUse, 0)
	activeToolUses := make(map[string]*strings.Builder)
	activeToolNames := make(map[string]string)
	activeToolCallIDs := make(map[string]string)

	for event := range stream {
		if event.Err != nil {
			if len(toolUses) > 0 {
				results := scheduler.finish()
				agent.log(traceID, "streaming_tool_turn partial_recovery tools=%d results=%d error=%v", len(toolUses), len(results), event.Err)
				return LLMResponse{
					Content:                  content.String(),
					InputTokens:              usage.inputTokens,
					OutputTokens:             usage.outputTokens,
					CachedInputTokens:        usage.cachedInputTokens,
					CacheCreationInputTokens: usage.cacheCreationInputTokens,
					StopReason:               "stream_recovered",
					ToolUses:                 toolUses,
				}, results, false, nil
			}
			agent.log(traceID, "streaming_tool_turn stream_error before_tools error=%v", event.Err)
			return LLMResponse{StopReason: "stream_interrupted"}, nil, true, event.Err
		}
		if event.Text != "" {
			content.WriteString(event.Text)
		}
		usage.add(event)

		switch event.Type {
		case "tool_use":
			if event.ToolID == "" {
				continue
			}
			agent.log(traceID, "streaming_tool_turn tool_use_start id=%s name=%s", event.ToolID, event.ToolName)
			builder := &strings.Builder{}
			if event.ToolInput != "" {
				builder.WriteString(event.ToolInput)
			}
			activeToolUses[event.ToolID] = builder
			activeToolNames[event.ToolID] = event.ToolName
			if event.ToolCallID != "" {
				activeToolCallIDs[event.ToolID] = event.ToolCallID
			}
		case "tool_use_delta":
			builder := activeToolUses[event.ToolID]
			if builder != nil {
				builder.WriteString(event.ToolInput)
			}
		case "tool_use_end":
			if maxToolCalls <= 0 || len(toolUses) >= maxToolCalls {
				continue
			}
			toolName := event.ToolName
			if toolName == "" {
				toolName = activeToolNames[event.ToolID]
			}
			toolInput := event.ToolInput
			if builder := activeToolUses[event.ToolID]; builder != nil {
				if builder.Len() > 0 {
					toolInput = builder.String()
				}
			}
			toolCallID := event.ToolCallID
			if toolCallID == "" {
				toolCallID = activeToolCallIDs[event.ToolID]
			}
			if toolCallID == "" {
				toolCallID = event.ToolID
			}
			toolUse := ToolUse{
				ID:    toolCallID,
				Name:  toolName,
				Input: toolInput,
			}
			toolUses = append(toolUses, toolUse)
			agent.log(traceID, "streaming_tool_turn tool_use_end id=%s name=%s input_chars=%d index=%d", toolUse.ID, toolUse.Name, len(toolUse.Input), len(toolUses)-1)
			scheduler.add(toolUse)
			delete(activeToolUses, event.ToolID)
			delete(activeToolNames, event.ToolID)
			delete(activeToolCallIDs, event.ToolID)
		}
	}

	results := scheduler.finish()
	agent.log(traceID, "streaming_tool_turn completed content_chars=%d tools=%d results=%d", content.Len(), len(toolUses), len(results))
	return LLMResponse{
		Content:                  content.String(),
		InputTokens:              usage.inputTokens,
		OutputTokens:             usage.outputTokens,
		CachedInputTokens:        usage.cachedInputTokens,
		CacheCreationInputTokens: usage.cacheCreationInputTokens,
		StopReason:               "completed",
		ToolUses:                 toolUses,
	}, results, false, nil
}

type streamUsage struct {
	inputTokens              int
	outputTokens             int
	cachedInputTokens        int
	cacheCreationInputTokens int
}

func (usage *streamUsage) add(event StreamEvent) {
	if event.InputTokens > 0 {
		usage.inputTokens = event.InputTokens
	}
	if event.OutputTokens > 0 {
		usage.outputTokens = event.OutputTokens
	}
	if event.CachedInputTokens > 0 {
		usage.cachedInputTokens = event.CachedInputTokens
	}
	if event.CacheCreationInputTokens > 0 {
		usage.cacheCreationInputTokens = event.CacheCreationInputTokens
	}
}

type streamingToolScheduler struct {
	agent       *Agent
	ctx         context.Context
	maxParallel int
	sem         chan struct{}
	results     []ToolResult
	safeBatch   []streamingPreparedTool
}

type streamingPreparedTool struct {
	index    int
	toolUse  ToolUse
	prepared preparedToolExecution
	done     chan ToolResult
}

func newStreamingToolScheduler(agent *Agent, ctx context.Context, maxParallel int) *streamingToolScheduler {
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallelToolCalls
	}
	return &streamingToolScheduler{
		agent:       agent,
		ctx:         ctx,
		maxParallel: maxParallel,
		sem:         make(chan struct{}, maxParallel),
		results:     make([]ToolResult, 0),
	}
}

func (scheduler *streamingToolScheduler) add(toolUse ToolUse) {
	traceID := TraceIDFromContext(scheduler.ctx)
	index := len(scheduler.results)
	scheduler.results = append(scheduler.results, ToolResult{ToolUse: toolUse})
	scheduler.agent.log(traceID, "streaming_scheduler add index=%d tool=%s", index, toolUse.Name)

	prepared, err := scheduler.agent.prepareToolExecution(scheduler.ctx, toolUse.ToMap())
	if err != nil {
		scheduler.flushSafeBatch()
		scheduler.agent.log(traceID, "streaming_scheduler prepare_error index=%d tool=%s error=%v", index, toolUse.Name, err)
		scheduler.results[index].Content = formatToolError(toolUse, err)
		scheduler.results[index].IsError = true
		return
	}

	if prepared.parallelSafe {
		scheduler.agent.log(traceID, "streaming_scheduler start_parallel index=%d tool=%s", index, toolUse.Name)
		item := streamingPreparedTool{
			index:    index,
			toolUse:  toolUse,
			prepared: prepared,
			done:     make(chan ToolResult, 1),
		}
		scheduler.startSafeTool(item)
		scheduler.safeBatch = append(scheduler.safeBatch, item)
		return
	}

	scheduler.flushSafeBatch()
	scheduler.agent.log(traceID, "streaming_scheduler execute_serial index=%d tool=%s", index, toolUse.Name)
	scheduler.agent.executePreparedToolIntoResult(prepared, &scheduler.results[index])
}

func (scheduler *streamingToolScheduler) finish() []ToolResult {
	scheduler.flushSafeBatch()
	return scheduler.results
}

func (scheduler *streamingToolScheduler) close() {
	scheduler.flushSafeBatch()
}

func (scheduler *streamingToolScheduler) startSafeTool(item streamingPreparedTool) {
	go func() {
		result := ToolResult{ToolUse: item.toolUse}
		select {
		case <-scheduler.ctx.Done():
			result.Content = formatToolError(item.toolUse, scheduler.ctx.Err())
			result.IsError = true
			item.done <- result
			return
		case scheduler.sem <- struct{}{}:
		}
		defer func() { <-scheduler.sem }()
		scheduler.agent.executePreparedToolIntoResult(item.prepared, &result)
		item.done <- result
	}()
}

func (scheduler *streamingToolScheduler) flushSafeBatch() {
	if len(scheduler.safeBatch) == 0 {
		return
	}
	traceID := TraceIDFromContext(scheduler.ctx)
	batch := scheduler.safeBatch
	scheduler.safeBatch = nil
	scheduler.agent.log(traceID, "streaming_scheduler flush_parallel count=%d", len(batch))

	if scheduler.maxParallel <= 1 || len(batch) == 1 {
		for _, item := range batch {
			scheduler.results[item.index] = <-item.done
		}
		return
	}

	for _, item := range batch {
		scheduler.results[item.index] = <-item.done
	}
}

type toolExecutionBatch struct {
	start    int
	end      int
	parallel bool
}

func buildToolExecutionBatches(preparedTools []preparedToolExecution, results []ToolResult) []toolExecutionBatch {
	batches := make([]toolExecutionBatch, 0, len(preparedTools))
	for i := 0; i < len(preparedTools); {
		if results[i].IsError || !preparedTools[i].parallelSafe {
			batches = append(batches, toolExecutionBatch{start: i, end: i + 1, parallel: false})
			i++
			continue
		}
		start := i
		for i < len(preparedTools) && !results[i].IsError && preparedTools[i].parallelSafe {
			i++
		}
		batches = append(batches, toolExecutionBatch{start: start, end: i, parallel: true})
	}
	return batches
}

func (agent *Agent) executePreparedToolBatch(ctx context.Context, toolUses []ToolUse, preparedTools []preparedToolExecution, results []ToolResult, start int, end int, maxParallel int) {
	traceID := TraceIDFromContext(ctx)
	count := end - start
	if count <= 0 {
		return
	}
	if maxParallel <= 0 {
		maxParallel = defaultMaxParallelToolCalls
	}
	if maxParallel > count {
		maxParallel = count
	}
	if maxParallel <= 1 {
		agent.log(traceID, "tool_batch serial start=%d end=%d", start, end)
		for i := start; i < end; i++ {
			agent.executePreparedToolIntoResult(preparedTools[i], &results[i])
		}
		return
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallel)
	agent.log(traceID, "tool_batch parallel start=%d end=%d max_parallel=%d", start, end, maxParallel)
	for i := start; i < end; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				results[index].Content = formatToolError(toolUses[index], ctx.Err())
				results[index].IsError = true
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()
			agent.executePreparedToolIntoResult(preparedTools[index], &results[index])
		}(i)
	}
	wg.Wait()
}

type preparedToolExecution struct {
	toolUse          ToolUse
	ctx              context.Context
	name             string
	input            map[string]any
	parallelSafe     bool
	registry         *tool.Registry
	enableToolBudget bool
	toolBudget       ToolBudget
	traceID          string
}

func (agent *Agent) prepareToolExecution(ctx context.Context, toolUse map[string]any) (preparedToolExecution, error) {
	traceID := TraceIDFromContext(ctx)
	if toolUse == nil {
		return preparedToolExecution{}, errors.New("tool use is nil")
	}

	name, ok := stringFromMap(toolUse, "name", "toolName", "tool_name")
	if !ok || name == "" {
		return preparedToolExecution{}, errors.New("tool name is required")
	}

	input, err := toolInputFromMap(toolUse)
	if err != nil {
		agent.log(traceID, "tool_prepare name=%s input_error=%v", name, err)
		return preparedToolExecution{}, err
	}

	toolUseID, _ := stringFromMap(toolUse, "id", "toolUseID", "tool_use_id")

	agent.mu.Lock()
	registry := agent.registry
	enableToolBudget := agent.enableToolBudget
	toolBudget := agent.toolBudget
	permission := agent.permission
	agent.mu.Unlock()

	if registry == nil {
		agent.log(traceID, "tool_prepare name=%s error=registry_nil", name)
		return preparedToolExecution{}, errors.New("tool registry is nil")
	}

	permissionCheck := PermissionCheck{ParallelSafe: true}
	if permission != nil {
		agent.log(traceID, "tool_permission check name=%s id=%s", name, toolUseID)
		check, err := permission.CheckWithResult(ctx, PermissionRequest{
			ToolUseID: toolUseID,
			ToolName:  name,
			ToolInput: input,
		})
		if err != nil {
			agent.log(traceID, "tool_permission denied name=%s id=%s error=%v", name, toolUseID, err)
			return preparedToolExecution{}, err
		}
		permissionCheck = check
		agent.log(traceID, "tool_permission allowed name=%s id=%s parallel_safe=%v", name, toolUseID, permissionCheck.ParallelSafe)
	}

	return preparedToolExecution{
		toolUse: ToolUse{
			ID:    toolUseID,
			Name:  name,
			Input: mustMarshalToolInput(input),
		},
		ctx:              ctx,
		name:             name,
		input:            input,
		parallelSafe:     permissionCheck.ParallelSafe,
		registry:         registry,
		enableToolBudget: enableToolBudget,
		toolBudget:       toolBudget,
		traceID:          traceID,
	}, nil
}

func (agent *Agent) executePreparedToolIntoResult(prepared preparedToolExecution, result *ToolResult) {
	traceID := prepared.traceID
	if updated := agent.runHooks(prepared.ctx, HookPayload{
		Event:     HookEventPreToolUse,
		AgentID:   agent.ID(),
		TraceID:   traceID,
		ToolUseID: prepared.toolUse.ID,
		ToolName:  prepared.name,
		Input:     prepared.input,
	}).UpdatedInput; updated != nil {
		prepared.input = updated
		prepared.toolUse.Input = mustMarshalToolInput(updated)
		result.ToolUse = prepared.toolUse
	}
	content, err := agent.callPreparedTool(prepared)
	agent.recordToolFileActivity(prepared)
	if err != nil {
		agent.log(traceID, "tool_execute error name=%s id=%s output_chars=%d error=%v", prepared.name, prepared.toolUse.ID, len(content), err)
		result.Content = formatToolErrorWithOutput(result.ToolUse, err, content)
		result.IsError = true
		agent.runHooks(prepared.ctx, HookPayload{
			Event:     HookEventPostToolUse,
			AgentID:   agent.ID(),
			TraceID:   traceID,
			ToolUseID: prepared.toolUse.ID,
			ToolName:  prepared.name,
			Input:     prepared.input,
			Output:    content,
			IsError:   true,
			Message:   err.Error(),
		})
		return
	}
	result.Content = content
	result.IsError = false
	agent.log(traceID, "tool_execute success name=%s id=%s result_chars=%d", prepared.name, prepared.toolUse.ID, len(content))
	agent.runHooks(prepared.ctx, HookPayload{
		Event:     HookEventPostToolUse,
		AgentID:   agent.ID(),
		TraceID:   traceID,
		ToolUseID: prepared.toolUse.ID,
		ToolName:  prepared.name,
		Input:     prepared.input,
		Output:    content,
	})
}

func (agent *Agent) runHooks(ctx context.Context, payload HookPayload) HookResult {
	agent.mu.Lock()
	hooks := agent.hooks
	agent.mu.Unlock()
	if hooks == nil {
		return HookResult{}
	}
	result := hooks.Run(ctx, payload)
	if result.Message != "" {
		agent.log(payload.TraceID, "hook message event=%s tool=%s:\n%s", payload.Event, payload.ToolName, result.Message)
	}
	return result
}

func (agent *Agent) recordToolFileActivity(prepared preparedToolExecution) {
	path, _ := stringFromMap(prepared.input, "path", "file_path", "filePath")
	if path == "" {
		return
	}
	action := ""
	switch prepared.name {
	case "read":
		action = "read"
	case "write":
		action = "write"
	case "edit":
		action = "edit"
	default:
		return
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.fileHistory = append(agent.fileHistory, FileHistoryEntry{
		Path:      path,
		Action:    action,
		AgentID:   agent.id,
		Timestamp: time.Now().UTC(),
	})
	if action == "write" || action == "edit" {
		agent.attribution = append(agent.attribution, AttributionEntry{
			Path:    path,
			AgentID: agent.id,
			Note:    action,
		})
	}
}

func (agent *Agent) callPreparedTool(prepared preparedToolExecution) (string, error) {
	toolInstance, getErr := prepared.registry.Get(prepared.name)
	if getErr != nil {
		return "", getErr
	}
	var result string
	var err error
	if contextual, ok := toolInstance.(contextualTool); ok {
		result, err = contextual.CallContext(prepared.ctx, prepared.input)
	} else {
		result, err = toolInstance.Call(prepared.input)
	}
	if prepared.enableToolBudget {
		truncated, truncateErr := tool.TruncateToolResult(result, prepared.toolBudget.MaxLen, prepared.toolBudget.HeadLen, prepared.toolBudget.TailLen)
		if truncateErr != nil && err == nil {
			return "", truncateErr
		}
		if truncateErr == nil {
			result = truncated
		}
	}
	return result, err
}

type contextualTool interface {
	CallContext(context.Context, map[string]any) (string, error)
}

func mustMarshalToolInput(input map[string]any) string {
	data, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	return string(data)
}

func formatToolError(toolUse ToolUse, err error) string {
	return fmt.Sprintf(`[TOOL_ERROR]
tool: %s
tool_use_id: %s
code: %s
recoverable: true
message: %s`, toolUse.Name, toolUse.ID, classifyToolError(err), err.Error())
}

func formatToolErrorWithOutput(toolUse ToolUse, err error, output string) string {
	message := formatToolError(toolUse, err)
	if strings.TrimSpace(output) == "" {
		return message
	}
	return message + "\noutput:\n" + output
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
