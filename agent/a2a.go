package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tool "github.com/GongShichen/CodingMan/tool"
)

type A2AMessageType string

const (
	A2AMessageTaskRequest A2AMessageType = "task_request"
	A2AMessageTaskResult  A2AMessageType = "task_result"
	A2AMessageReflection  A2AMessageType = "reflection"
	A2AMessageError       A2AMessageType = "error"
)

type A2AMessage struct {
	TraceID  string         `json:"trace_id,omitempty"`
	From     string         `json:"from"`
	To       string         `json:"to"`
	Type     A2AMessageType `json:"type"`
	Content  string         `json:"content,omitempty"`
	Messages []Message      `json:"messages,omitempty"`
	Error    string         `json:"error,omitempty"`
}

type A2ABus struct {
	mu       sync.Mutex
	messages []A2AMessage
	parents  map[string]string
}

func NewA2ABus() *A2ABus {
	return &A2ABus{
		messages: make([]A2AMessage, 0),
		parents:  make(map[string]string),
	}
}

func (bus *A2ABus) RegisterAgent(agentID string, parentID string) {
	if bus == nil {
		return
	}
	agentID = strings.TrimSpace(agentID)
	parentID = strings.TrimSpace(parentID)
	if agentID == "" {
		return
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	bus.parents[agentID] = parentID
}

func (bus *A2ABus) Send(message A2AMessage) error {
	if bus == nil {
		return nil
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	if !bus.directParentChildLocked(message.From, message.To) {
		return fmt.Errorf("a2a communication is limited to direct parent-child agents: %s -> %s", message.From, message.To)
	}
	bus.messages = append(bus.messages, message)
	return nil
}

func (bus *A2ABus) Messages() []A2AMessage {
	if bus == nil {
		return nil
	}
	bus.mu.Lock()
	defer bus.mu.Unlock()
	result := make([]A2AMessage, len(bus.messages))
	copy(result, bus.messages)
	return result
}

func (bus *A2ABus) directParentChildLocked(from string, to string) bool {
	from = strings.TrimSpace(from)
	to = strings.TrimSpace(to)
	if from == "" || to == "" {
		return false
	}
	return bus.parents[from] == to || bus.parents[to] == from
}

type SubAgentResult struct {
	AgentID    string `json:"agent_id"`
	AgentName  string `json:"agent_name"`
	Content    string `json:"content"`
	StopReason string `json:"stop_reason"`
	Error      string `json:"error,omitempty"`
}

type subAgentTool struct {
	agent *Agent
}

func newSubAgentTool(agent *Agent) tool.Tool {
	return &subAgentTool{agent: agent}
}

func (subagent *subAgentTool) Name() string {
	return "subagent"
}

func (subagent *subAgentTool) Description() string {
	return "Start, await, inspect, or stop direct child sub-agents for focused coding subtasks. Supports async worker tasks and XML task notifications."
}

func (subagent *subAgentTool) InputSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"task": map[string]any{
				"type":        "string",
				"description": "A single concrete task for one sub-agent to complete.",
			},
			"action": map[string]any{
				"type":        "string",
				"description": "Action to perform: start, await, taskstop, status, or list. Defaults to start.",
				"enum":        []string{"start", "await", "taskstop", "status", "list"},
			},
			"async": map[string]any{
				"type":        "boolean",
				"description": "When true, start workers asynchronously and return <task-notification> XML immediately.",
			},
			"task_id": map[string]any{
				"type":        "string",
				"description": "Task id for await, taskstop, or status.",
			},
			"mode": map[string]any{
				"type":        "string",
				"description": "Worker mode: worker uses isolated context; fork starts from a parent conversation snapshot.",
				"enum":        []string{"worker", "fork"},
			},
			"tasks": map[string]any{
				"type":        "array",
				"description": "Multiple concrete tasks to run concurrently in sub-agents. Each item may be a string or an object with task, agent_name, and system_prompt.",
				"items": map[string]any{
					"oneOf": []any{
						map[string]any{"type": "string"},
						map[string]any{
							"type": "object",
							"properties": map[string]any{
								"task": map[string]any{
									"type":        "string",
									"description": "The concrete task for the sub-agent to complete.",
								},
								"agent_name": map[string]any{
									"type":        "string",
									"description": "Optional short name for this sub-agent.",
								},
								"system_prompt": map[string]any{
									"type":        "string",
									"description": "Optional additional system instructions for this sub-agent.",
								},
							},
							"required": []string{"task"},
						},
					},
				},
			},
			"agent_name": map[string]any{
				"type":        "string",
				"description": "Optional short name for the single sub-agent.",
			},
			"system_prompt": map[string]any{
				"type":        "string",
				"description": "Optional additional system instructions for the single sub-agent.",
			},
		},
	}
}

func (subagent *subAgentTool) ToAPIFormat() map[string]any {
	return map[string]any{
		"name":         subagent.Name(),
		"description":  subagent.Description(),
		"input_schema": subagent.InputSchema(),
	}
}

func (subagent *subAgentTool) Call(input map[string]any) (string, error) {
	return subagent.CallContext(context.Background(), input)
}

func (subagent *subAgentTool) CallContext(ctx context.Context, input map[string]any) (string, error) {
	if subagent.agent == nil {
		return "", errors.New("subagent tool has no agent")
	}
	action, _ := input["action"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	if action == "" {
		action = "start"
	}
	coordinator := subagent.agent.Coordinator()
	switch action {
	case "await":
		taskID, _ := input["task_id"].(string)
		notification, err := coordinator.AwaitTask(ctx, taskID)
		return notification.String(), err
	case "taskstop", "stop", "kill":
		taskID, _ := input["task_id"].(string)
		notification, err := coordinator.StopTask(taskID)
		return notification.String(), err
	case "status":
		taskID, _ := input["task_id"].(string)
		notification, err := coordinator.TaskStatus(taskID)
		return notification.String(), err
	case "list":
		notifications := coordinator.ListTasks()
		data, err := taskNotificationsJSON(notifications)
		return data, err
	case "start":
	default:
		return "", fmt.Errorf("unsupported subagent action: %s", action)
	}
	async, _ := input["async"].(bool)
	if rawTasks, ok := input["tasks"]; ok {
		requests, err := subAgentRequestsFromInput(rawTasks)
		if err != nil {
			return "", err
		}
		if async {
			notifications := make([]TaskNotification, 0, len(requests))
			for _, request := range requests {
				notification, err := coordinator.StartWorker(ctx, WorkerTaskRequest{
					Task:         request.Task,
					AgentName:    request.AgentName,
					SystemPrompt: request.SystemPrompt,
					Mode:         request.Mode,
				})
				if err != nil {
					return "", err
				}
				notifications = append(notifications, notification)
			}
			var builder strings.Builder
			for _, notification := range notifications {
				if builder.Len() > 0 {
					builder.WriteString("\n")
				}
				builder.WriteString(notification.String())
			}
			return builder.String(), nil
		}
		results := subagent.agent.RunSubAgents(ctx, requests)
		data, marshalErr := json.MarshalIndent(map[string]any{"results": results}, "", "  ")
		if marshalErr != nil {
			return "", marshalErr
		}
		if err := firstSubAgentResultError(results); err != nil {
			return string(data), err
		}
		return string(data), nil
	}
	task, _ := input["task"].(string)
	task = strings.TrimSpace(task)
	if task == "" {
		return "", errors.New("subagent task is required")
	}
	agentName, _ := input["agent_name"].(string)
	systemPrompt, _ := input["system_prompt"].(string)
	mode := workerModeFromInput(input)
	if async {
		notification, err := coordinator.StartWorker(ctx, WorkerTaskRequest{
			Task:         task,
			AgentName:    strings.TrimSpace(agentName),
			SystemPrompt: strings.TrimSpace(systemPrompt),
			Mode:         mode,
		})
		return notification.String(), err
	}

	result, err := subagent.agent.RunSubAgent(ctx, SubAgentRequest{
		Task:         task,
		AgentName:    strings.TrimSpace(agentName),
		SystemPrompt: strings.TrimSpace(systemPrompt),
		Mode:         mode,
	})
	data, marshalErr := json.MarshalIndent(result, "", "  ")
	if marshalErr != nil {
		return "", marshalErr
	}
	if err != nil {
		return string(data), err
	}
	return string(data), nil
}

func subAgentRequestsFromInput(raw any) ([]SubAgentRequest, error) {
	items, ok := raw.([]any)
	if !ok {
		return nil, errors.New("subagent tasks must be an array")
	}
	if len(items) == 0 {
		return nil, errors.New("subagent tasks must not be empty")
	}
	requests := make([]SubAgentRequest, 0, len(items))
	for i, item := range items {
		switch value := item.(type) {
		case string:
			task := strings.TrimSpace(value)
			if task == "" {
				return nil, fmt.Errorf("subagent tasks[%d] is empty", i)
			}
			requests = append(requests, SubAgentRequest{Task: task})
		case map[string]any:
			task, _ := value["task"].(string)
			task = strings.TrimSpace(task)
			if task == "" {
				return nil, fmt.Errorf("subagent tasks[%d].task is required", i)
			}
			agentName, _ := value["agent_name"].(string)
			systemPrompt, _ := value["system_prompt"].(string)
			requests = append(requests, SubAgentRequest{
				Task:         task,
				AgentName:    strings.TrimSpace(agentName),
				SystemPrompt: strings.TrimSpace(systemPrompt),
				Mode:         workerModeFromInput(value),
			})
		default:
			return nil, fmt.Errorf("subagent tasks[%d] must be a string or object", i)
		}
	}
	return requests, nil
}

func firstSubAgentResultError(results []SubAgentResult) error {
	for _, result := range results {
		if result.Error != "" {
			return errors.New(result.Error)
		}
	}
	return nil
}

type SubAgentRequest struct {
	Task         string
	AgentName    string
	SystemPrompt string
	Mode         WorkerMode
}

func workerModeFromInput(input map[string]any) WorkerMode {
	mode, _ := input["mode"].(string)
	switch WorkerMode(strings.ToLower(strings.TrimSpace(mode))) {
	case WorkerModeFork:
		return WorkerModeFork
	default:
		return WorkerModeWorker
	}
}

func (agent *Agent) RunSubAgents(ctx context.Context, requests []SubAgentRequest) []SubAgentResult {
	if len(requests) == 0 {
		return nil
	}
	agent.mu.Lock()
	maxConcurrent := agent.maxConcurrentSubAgents
	agent.mu.Unlock()
	if maxConcurrent <= 0 {
		maxConcurrent = defaultMaxConcurrentSubAgents
	}
	if maxConcurrent > len(requests) {
		maxConcurrent = len(requests)
	}

	results := make([]SubAgentResult, len(requests))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxConcurrent)
	for i, request := range requests {
		wg.Add(1)
		go func(index int, subRequest SubAgentRequest) {
			defer wg.Done()
			select {
			case <-ctx.Done():
				results[index] = SubAgentResult{Error: ctx.Err().Error(), StopReason: "context_cancelled"}
				return
			case sem <- struct{}{}:
			}
			defer func() { <-sem }()

			result, err := agent.RunSubAgent(ctx, subRequest)
			if err != nil {
				result.Error = err.Error()
				if result.StopReason == "" {
					result.StopReason = "error"
				}
			}
			results[index] = result
		}(i, request)
	}
	wg.Wait()
	return results
}

func (agent *Agent) RunSubAgent(ctx context.Context, request SubAgentRequest) (SubAgentResult, error) {
	ctx, _ = ensureTrace(ctx)
	task := strings.TrimSpace(request.Task)
	if task == "" {
		return SubAgentResult{}, errors.New("sub-agent task is required")
	}
	mode := request.Mode
	if mode == "" {
		mode = WorkerModeWorker
	}

	agent.mu.Lock()
	if agent.depth >= agent.maxSubAgentDepth {
		depth := agent.depth
		maxDepth := agent.maxSubAgentDepth
		agent.mu.Unlock()
		return SubAgentResult{}, fmt.Errorf("sub-agent depth limit reached: depth=%d max=%d", depth, maxDepth)
	}
	childID := agent.nextChildAgentID()
	childName := request.AgentName
	if childName == "" {
		childName = strings.Replace(childID, agent.id+".", "subagent-", 1)
	}
	agent.mu.Unlock()

	workerTask := &WorkerTask{
		ID:        childID,
		AgentID:   childID,
		AgentName: childName,
		Mode:      mode,
		Status:    TaskStatusRunning,
		Request: WorkerTaskRequest{
			Task:         task,
			AgentName:    childName,
			SystemPrompt: request.SystemPrompt,
			Mode:         mode,
		},
		StartedAt: time.Now(),
	}
	return agent.runWorkerAgent(ctx, workerTask)
}

func (agent *Agent) nextChildAgentID() string {
	childIndex := agent.nextChildIndex.Add(1)
	return fmt.Sprintf("%s.%d", agent.id, childIndex)
}

func (agent *Agent) runWorkerAgent(ctx context.Context, task *WorkerTask) (SubAgentResult, error) {
	ctx, traceID := ensureTrace(ctx)
	delegatedTask := strings.TrimSpace(task.Request.Task)
	if delegatedTask == "" {
		return SubAgentResult{}, errors.New("worker task is required")
	}
	childID := task.AgentID
	childName := task.AgentName
	if childName == "" {
		childName = childID
	}

	agent.mu.Lock()
	contextConfig := agent.contextConfig
	if task.WorktreeDir != "" {
		contextConfig.Cwd = task.WorktreeDir
	}
	baseSystem := contextConfig.BaseSystem
	if task.SharedTempDir != "" {
		baseSystem += "\n\n## Shared Worker Temp Directory\n" + task.SharedTempDir
	}
	if task.Request.SystemPrompt != "" {
		baseSystem += "\n\n## Worker Instructions\n" + task.Request.SystemPrompt
	}
	contextConfig.BaseSystem = baseSystem + "\n\n## A2A Worker Role\nYou are a direct child worker. Complete only the delegated task, report concrete findings and edits, and do not start sub-agents."
	enableSelfReflection := agent.enableSelfReflection
	childRegistry := agent.registry.Clone()
	if childRegistry != nil {
		childRegistry.Unregister("subagent")
	}
	parentSnapshot := agent.snapshotMessagesLocked()
	config := AgentConfig{
		LLM:                      agent.llm,
		Registry:                 childRegistry,
		Context:                  contextConfig,
		Model:                    agent.model,
		MaxLLMTurns:              agent.maxTurn,
		MaxToolCalls:             agent.maxToolCalls,
		MaxParallelToolCalls:     agent.maxParallelToolCalls,
		MaxConsecutiveToolErrors: agent.maxConsecutiveToolErrors,
		MaxConsecutiveAPIErrors:  agent.maxConsecutiveAPIErrors,
		MaxConcurrentSubAgents:   agent.maxConcurrentSubAgents,
		SessionMemoryThreshold:   agent.sessionMemoryThreshold,
		SkillEvolutionThreshold:  agent.skillEvolutionThreshold,
		EnableToolBudget:         agent.enableToolBudget,
		ToolBudget:               agent.toolBudget,
		RetryConfig:              agent.retryConfig,
		PermissionManager:        agent.permission,
		PromptCache:              agent.promptCache,
		EnableStreamRecovery:     &agent.enableStreamRecovery,
		StreamRecoveryMaxRetries: agent.streamRecoveryMaxRetries,
		Logger:                   agent.logger,
		A2ABus:                   agent.a2a,
		ID:                       childID,
		Name:                     childName,
		ParentID:                 agent.id,
		Depth:                    agent.depth + 1,
		MaxSubAgentDepth:         1,
		EnableSelfReflection:     &enableSelfReflection,
	}
	agent.mu.Unlock()

	agent.log(traceID, "a2a subagent start child=%s name=%s task:\n%s", childID, childName, delegatedTask)
	agent.a2a.RegisterAgent(childID, agent.id)
	agent.runHooks(ctx, HookPayload{Event: HookEventNotification, AgentID: agent.id, TraceID: traceID, Message: "subagent started", SubagentID: childID, TaskID: task.ID})
	if err := agent.a2a.Send(A2AMessage{
		TraceID: traceID,
		From:    agent.id,
		To:      childID,
		Type:    A2AMessageTaskRequest,
		Content: delegatedTask,
	}); err != nil {
		return SubAgentResult{}, err
	}

	// NewAgent intentionally starts with an empty message history here. A2A only
	// passes the delegated task and final result, not the parent's conversation.
	child := NewAgent(config)
	if task.Mode == WorkerModeFork {
		child.mu.Lock()
		child.messages = cloneMessages(parentSnapshot)
		child.mu.Unlock()
	}
	resp, err := child.RunToolLoop(ctx, delegatedTask)
	result := SubAgentResult{
		AgentID:    childID,
		AgentName:  childName,
		Content:    resp.Content,
		StopReason: resp.StopReason,
	}
	message := A2AMessage{
		TraceID: traceID,
		From:    childID,
		To:      agent.id,
		Type:    A2AMessageTaskResult,
		Content: resp.Content,
	}
	if err != nil {
		message.Type = A2AMessageError
		message.Error = err.Error()
	}
	if sendErr := agent.a2a.Send(message); sendErr != nil && err == nil {
		err = sendErr
	}
	agent.runHooks(ctx, HookPayload{
		Event:      HookEventSubagentStop,
		AgentID:    agent.id,
		TraceID:    traceID,
		Message:    resp.Content,
		StopReason: resp.StopReason,
		TaskID:     task.ID,
		SubagentID: childID,
	})
	agent.log(traceID, "a2a subagent end child=%s stop=%s error=%v", childID, resp.StopReason, err)
	return result, err
}
