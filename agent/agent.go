package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

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
	maxParallelToolCalls     int
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
	enableStreamRecovery     bool
	streamRecoveryMaxRetries int
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
	EnableToolBudget         bool
	ToolBudget               ToolBudget
	RetryConfig              RetryConfig
	Permission               PermissionConfig
	PromptCache              PromptCacheConfig
	EnableStreamRecovery     bool
	StreamRecoveryMaxRetries int
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
	permissionConfig := config.Permission
	if isZeroPermissionConfig(permissionConfig) {
		permissionConfig = DefaultPermissionConfig()
	}

	agent := &Agent{
		llm:                      config.LLM,
		registry:                 registry,
		system:                   system,
		model:                    config.Model,
		messages:                 make([]Message, 0),
		maxTurn:                  defaultMaxTurnValue(config.MaxLLMTurns, config.MaxTurn),
		maxToolCalls:             defaultPositiveInt(config.MaxToolCalls, defaultMaxToolCalls),
		maxParallelToolCalls:     defaultPositiveInt(config.MaxParallelToolCalls, defaultMaxParallelToolCalls),
		maxConsecutiveToolErrors: defaultPositiveInt(config.MaxConsecutiveToolErrors, defaultMaxConsecutiveToolErrors),
		maxConsecutiveAPIErrors:  defaultPositiveInt(config.MaxConsecutiveAPIErrors, defaultMaxConsecutiveAPIErrors),
		enableToolBudget:         config.EnableToolBudget,
		toolBudget:               config.ToolBudget,
		enableAutoCompact:        contextConfig.AutoCompact,
		autoCompactThreshold:     defaultAutoCompactThreshold(contextConfig.CompactThreshold),
		autoCompactKeepRecent:    defaultAutoCompactKeepRecent(contextConfig.KeepRecentRounds),
		contextConfig:            contextConfig,
		retryConfig:              defaultRetryConfig(config.RetryConfig),
		permission:               NewPermissionManager(permissionConfig),
		promptCache:              normalizePromptCacheConfig(promptCacheConfig),
		enableStreamRecovery:     true,
		streamRecoveryMaxRetries: defaultStreamRecoveryMaxRetries(config.StreamRecoveryMaxRetries, config.RetryConfig.MaxRetries),
	}

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
		maxParallelToolCalls := agent.maxParallelToolCalls
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
		enableStreamRecovery := agent.enableStreamRecovery
		streamRecoveryMaxRetries := agent.streamRecoveryMaxRetries
		agent.mu.Unlock()

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
			if consecutiveAPIErrors >= maxConsecutiveAPIErrors {
				return LLMResponse{
					StopReason: "apierrors",
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

		toolCalls += len(toolUses)
		agent.appendToolResultMessages(results)
		for _, result := range results {
			if result.IsError {
				consecutiveToolErrors++
			} else {
				consecutiveToolErrors = 0
			}
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
			stream := llm.Stream(ctx, messagesSnapshot, opts)
			shouldRetry := false

			for event := range stream {
				if event.Err != nil {
					if !emitted && attempt < retryConfig.MaxRetries && retryConfig.Retryable(event.Err) {
						shouldRetry = true
						break
					}
					streamFailed = true
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
				return
			}
			delay = time.Duration(math.Round(float64(delay) * retryConfig.Multiplier))
			if delay > retryConfig.MaxDelay {
				delay = retryConfig.MaxDelay
			}
		}
		if streamFailed {
			return
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
	prepared, err := agent.prepareToolExecution(ctx, toolUse)
	if err != nil {
		return "", err
	}
	return agent.callPreparedTool(prepared)
}

func (agent *Agent) ExecuteTools(ctx context.Context, toolUses []ToolUse, maxParallel int) []ToolResult {
	results := make([]ToolResult, len(toolUses))
	preparedTools := make([]preparedToolExecution, len(toolUses))

	for i, toolUse := range toolUses {
		results[i].ToolUse = toolUse
		prepared, err := agent.prepareToolExecution(ctx, toolUse.ToMap())
		if err != nil {
			results[i].Content = formatToolError(toolUse, err)
			results[i].IsError = true
			continue
		}
		preparedTools[i] = prepared
	}

	batches := buildToolExecutionBatches(preparedTools, results)
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
	return results
}

func (agent *Agent) runStreamingToolTurn(ctx context.Context, llm LLM, messages []Message, opts ChatOptions, maxParallel int, maxToolCalls int, recovery StreamRecoveryConfig) (LLMResponse, []ToolResult, error) {
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
		resp, results, retryable, err := agent.runStreamingToolTurnAttempt(ctx, llm, messages, opts, maxParallel, maxToolCalls)
		resp.RetryAttempts = attempt
		if err == nil {
			if attempt > 0 {
				resp.StopReason = "stream_recovered"
			}
			return resp, results, nil
		}
		lastErr = err
		if !retryable || attempt == recovery.MaxRetries || !recovery.Retry.Retryable(err) {
			if resp.StopReason == "" {
				resp.StopReason = "stream_interrupted"
			}
			resp.RetryAttempts = attempt
			return resp, results, err
		}
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
	stream := llm.Stream(ctx, messages, opts)
	scheduler := newStreamingToolScheduler(agent, ctx, maxParallel)
	defer scheduler.close()

	var content strings.Builder
	toolUses := make([]ToolUse, 0)
	activeToolUses := make(map[string]*strings.Builder)
	activeToolNames := make(map[string]string)
	activeToolCallIDs := make(map[string]string)

	for event := range stream {
		if event.Err != nil {
			if len(toolUses) > 0 {
				results := scheduler.finish()
				return LLMResponse{
					Content:    content.String(),
					StopReason: "stream_recovered",
					ToolUses:   toolUses,
				}, results, false, nil
			}
			return LLMResponse{StopReason: "stream_interrupted"}, nil, true, event.Err
		}
		if event.Text != "" {
			content.WriteString(event.Text)
		}

		switch event.Type {
		case "tool_use":
			if event.ToolID == "" {
				continue
			}
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
				toolInput = builder.String()
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
			scheduler.add(toolUse)
			delete(activeToolUses, event.ToolID)
			delete(activeToolNames, event.ToolID)
			delete(activeToolCallIDs, event.ToolID)
		}
	}

	results := scheduler.finish()
	return LLMResponse{
		Content:    content.String(),
		StopReason: "completed",
		ToolUses:   toolUses,
	}, results, false, nil
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
	index := len(scheduler.results)
	scheduler.results = append(scheduler.results, ToolResult{ToolUse: toolUse})

	prepared, err := scheduler.agent.prepareToolExecution(scheduler.ctx, toolUse.ToMap())
	if err != nil {
		scheduler.flushSafeBatch()
		scheduler.results[index].Content = formatToolError(toolUse, err)
		scheduler.results[index].IsError = true
		return
	}

	if prepared.parallelSafe {
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
	batch := scheduler.safeBatch
	scheduler.safeBatch = nil

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
		for i := start; i < end; i++ {
			agent.executePreparedToolIntoResult(preparedTools[i], &results[i])
		}
		return
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallel)
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
	name             string
	input            map[string]any
	parallelSafe     bool
	registry         *tool.Registry
	enableToolBudget bool
	toolBudget       ToolBudget
}

func (agent *Agent) prepareToolExecution(ctx context.Context, toolUse map[string]any) (preparedToolExecution, error) {
	if toolUse == nil {
		return preparedToolExecution{}, errors.New("tool use is nil")
	}

	name, ok := stringFromMap(toolUse, "name", "toolName", "tool_name")
	if !ok || name == "" {
		return preparedToolExecution{}, errors.New("tool name is required")
	}

	input, err := toolInputFromMap(toolUse)
	if err != nil {
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
		return preparedToolExecution{}, errors.New("tool registry is nil")
	}

	permissionCheck := PermissionCheck{ParallelSafe: true}
	if permission != nil {
		check, err := permission.CheckWithResult(ctx, PermissionRequest{
			ToolUseID: toolUseID,
			ToolName:  name,
			ToolInput: input,
		})
		if err != nil {
			return preparedToolExecution{}, err
		}
		permissionCheck = check
	}

	return preparedToolExecution{
		toolUse: ToolUse{
			ID:    toolUseID,
			Name:  name,
			Input: mustMarshalToolInput(input),
		},
		name:             name,
		input:            input,
		parallelSafe:     permissionCheck.ParallelSafe,
		registry:         registry,
		enableToolBudget: enableToolBudget,
		toolBudget:       toolBudget,
	}, nil
}

func (agent *Agent) executePreparedToolIntoResult(prepared preparedToolExecution, result *ToolResult) {
	content, err := agent.callPreparedTool(prepared)
	if err != nil {
		result.Content = formatToolError(result.ToolUse, err)
		result.IsError = true
		return
	}
	result.Content = content
	result.IsError = false
}

func (agent *Agent) callPreparedTool(prepared preparedToolExecution) (string, error) {
	result, err := prepared.registry.Call(prepared.name, prepared.input)
	if err != nil {
		return "", err
	}

	if prepared.enableToolBudget {
		return tool.TruncateToolResult(result, prepared.toolBudget.MaxLen, prepared.toolBudget.HeadLen, prepared.toolBudget.TailLen)
	}
	return result, nil
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
