package agent_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
)

type fakeLLM struct {
	mu          sync.Mutex
	streamCalls int
	chatFn      func(context.Context, []agent.Message, agent.ChatOptions) (agent.LLMResponse, error)
	streamFn    func(context.Context, []agent.Message, agent.ChatOptions, int) []agent.StreamEvent
}

func (llm *fakeLLM) Chat(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
	if llm.chatFn != nil {
		return llm.chatFn(ctx, messages, opts)
	}
	return agent.LLMResponse{Content: "ok", StopReason: "stop"}, nil
}

func (llm *fakeLLM) Stream(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) <-chan agent.StreamEvent {
	llm.mu.Lock()
	call := llm.streamCalls
	llm.streamCalls++
	llm.mu.Unlock()

	ch := make(chan agent.StreamEvent, 16)
	go func() {
		defer close(ch)
		events := []agent.StreamEvent{{Type: "text", Text: "ok"}, {Done: true}}
		if llm.streamFn != nil {
			events = llm.streamFn(ctx, messages, opts, call)
		}
		for _, event := range events {
			ch <- event
		}
	}()
	return ch
}

func TestDefaultAndCustomSystemPrompt(t *testing.T) {
	a := agent.NewAgent(agent.AgentConfig{LLM: &fakeLLM{}})
	if a.ID() != "main" {
		t.Fatalf("default agent id = %q, want main", a.ID())
	}
	if !strings.Contains(a.SystemPrompt(), "You are CodingMan") {
		t.Fatalf("default coding system prompt missing:\n%s", a.SystemPrompt())
	}

	if err := a.SetBaseSystemPrompt("custom coding policy"); err != nil {
		t.Fatal(err)
	}
	system := a.SystemPrompt()
	if !strings.Contains(system, "custom coding policy") {
		t.Fatalf("custom system prompt missing:\n%s", system)
	}
	if !strings.Contains(system, "工作目录:") {
		t.Fatalf("context metadata missing after custom system prompt:\n%s", system)
	}
}

func TestPlanDoesNotMutateConversationOrExposeTools(t *testing.T) {
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			if len(opts.Tools) != 0 {
				t.Fatalf("plan mode should not expose tools: %+v", opts.Tools)
			}
			formatted := agent.FormatMessages(messages)
			if !strings.Contains(formatted, "Plan mode:") || !strings.Contains(formatted, "change request") {
				t.Fatalf("plan prompt missing:\n%s", formatted)
			}
			return agent.LLMResponse{Content: "plan only", StopReason: "completed"}, nil
		},
	}
	a := agent.NewAgent(agent.AgentConfig{LLM: llm})
	resp, err := a.Plan(context.Background(), "change request")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "plan only" {
		t.Fatalf("unexpected plan response: %+v", resp)
	}
	if got := len(a.Messages()); got != 0 {
		t.Fatalf("plan mode should not mutate conversation, messages=%d", got)
	}
}

func TestDelegateToolRunsSubAgentAndRecordsA2A(t *testing.T) {
	llm := &fakeLLM{}
	llm.streamFn = func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
		switch call {
		case 0:
			return []agent.StreamEvent{
				{Type: "tool_use", ToolID: "subagent_1", ToolCallID: "subagent_1", ToolName: "subagent"},
				{Type: "tool_use_end", ToolID: "subagent_1", ToolCallID: "subagent_1", ToolName: "subagent", ToolInput: `{"task":"inspect delegated work","agent_name":"worker"}`},
				{Done: true},
			}
		case 1:
			for _, tool := range opts.Tools {
				if tool.Name() == "subagent" {
					t.Fatal("sub-agent must not expose subagent tool")
				}
			}
			return []agent.StreamEvent{
				{Type: "text", Text: "child completed"},
				{Done: true},
			}
		default:
			formatted := agent.FormatMessages(messages)
			if !strings.Contains(formatted, "child completed") {
				t.Fatalf("parent did not receive sub-agent result:\n%s", formatted)
			}
			return []agent.StreamEvent{
				{Type: "text", Text: "parent integrated"},
				{Done: true},
			}
		}
	}

	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Permission: agent.PermissionConfig{
			Mode: agent.PermissionModeFullAuto,
		},
	})
	resp, err := a.RunToolLoop(context.Background(), "start subagent")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "parent integrated" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	messages := a.A2AMessages()
	if len(messages) != 2 {
		t.Fatalf("expected task request and result messages, got %+v", messages)
	}
	if messages[0].Type != agent.A2AMessageTaskRequest || messages[1].Type != agent.A2AMessageTaskResult {
		t.Fatalf("unexpected A2A messages: %+v", messages)
	}
	if messages[0].From != "main" || messages[0].To != "main.1" || messages[1].From != "main.1" || messages[1].To != "main" {
		t.Fatalf("unexpected A2A agent ids: %+v", messages)
	}
}

func TestDelegateToolRunsMultipleSubAgentsConcurrently(t *testing.T) {
	llm := &fakeLLM{}
	llm.streamFn = func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
		switch call {
		case 0:
			return []agent.StreamEvent{
				{Type: "tool_use", ToolID: "subagent_multi", ToolCallID: "subagent_multi", ToolName: "subagent"},
				{Type: "tool_use_end", ToolID: "subagent_multi", ToolCallID: "subagent_multi", ToolName: "subagent", ToolInput: `{"tasks":[{"task":"first","agent_name":"alpha"},{"task":"second","agent_name":"beta"}]}`},
				{Done: true},
			}
		case 1:
			return []agent.StreamEvent{{Type: "text", Text: "child one"}, {Done: true}}
		case 2:
			return []agent.StreamEvent{{Type: "text", Text: "child two"}, {Done: true}}
		default:
			formatted := agent.FormatMessages(messages)
			if !strings.Contains(formatted, `"agent_id": "main.1"`) || !strings.Contains(formatted, `"agent_id": "main.2"`) {
				t.Fatalf("batched sub-agent ids missing:\n%s", formatted)
			}
			if !strings.Contains(formatted, "child one") || !strings.Contains(formatted, "child two") {
				t.Fatalf("batched sub-agent results missing:\n%s", formatted)
			}
			return []agent.StreamEvent{{Type: "text", Text: "parent merged"}, {Done: true}}
		}
	}

	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Permission: agent.PermissionConfig{
			Mode: agent.PermissionModeFullAuto,
		},
		MaxConcurrentSubAgents: 2,
	})
	resp, err := a.RunToolLoop(context.Background(), "start multiple subagents")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "parent merged" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	messages := a.A2AMessages()
	if len(messages) != 4 {
		t.Fatalf("expected 4 A2A messages, got %+v", messages)
	}
	seen := map[string]bool{}
	for _, message := range messages {
		if strings.HasPrefix(message.From, "main.") {
			seen[message.From] = true
		}
		if strings.HasPrefix(message.To, "main.") {
			seen[message.To] = true
		}
	}
	if !seen["main.1"] || !seen["main.2"] {
		t.Fatalf("expected distinct child agent ids, got messages: %+v", messages)
	}
}

func TestSubAgentDoesNotInheritParentConversationContext(t *testing.T) {
	const parentOnly = "parent-only-context-marker"
	const childTask = "child isolated task"

	llm := &fakeLLM{}
	llm.streamFn = func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
		switch call {
		case 0:
			formatted := agent.FormatMessages(messages)
			if !strings.Contains(formatted, parentOnly) {
				t.Fatalf("parent request missing parent context marker:\n%s", formatted)
			}
			return []agent.StreamEvent{
				{Type: "tool_use", ToolID: "subagent_isolated", ToolCallID: "subagent_isolated", ToolName: "subagent"},
				{Type: "tool_use_end", ToolID: "subagent_isolated", ToolCallID: "subagent_isolated", ToolName: "subagent", ToolInput: `{"task":"` + childTask + `"}`},
				{Done: true},
			}
		case 1:
			formatted := agent.FormatMessages(messages)
			if strings.Contains(formatted, parentOnly) {
				t.Fatalf("sub-agent inherited parent conversation context:\n%s", formatted)
			}
			if !strings.Contains(formatted, childTask) {
				t.Fatalf("sub-agent did not receive delegated task:\n%s", formatted)
			}
			return []agent.StreamEvent{{Type: "text", Text: "child isolated"}, {Done: true}}
		default:
			return []agent.StreamEvent{{Type: "text", Text: "parent done"}, {Done: true}}
		}
	}

	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Permission: agent.PermissionConfig{
			Mode: agent.PermissionModeFullAuto,
		},
	})
	resp, err := a.RunToolLoop(context.Background(), "parent asks with "+parentOnly)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "parent done" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestA2ARejectsNonDirectParentChildCommunication(t *testing.T) {
	bus := agent.NewA2ABus()
	bus.RegisterAgent("main", "")
	bus.RegisterAgent("main.1", "main")
	bus.RegisterAgent("main.2", "main")

	if err := bus.Send(agent.A2AMessage{From: "main", To: "main.1", Type: agent.A2AMessageTaskRequest, Content: "ok"}); err != nil {
		t.Fatalf("direct parent-child message rejected: %v", err)
	}
	if err := bus.Send(agent.A2AMessage{From: "main.1", To: "main", Type: agent.A2AMessageTaskResult, Content: "ok"}); err != nil {
		t.Fatalf("direct child-parent message rejected: %v", err)
	}
	if err := bus.Send(agent.A2AMessage{From: "main.1", To: "main.2", Type: agent.A2AMessageTaskResult, Content: "bad"}); err == nil {
		t.Fatal("sibling sub-agent message should be rejected")
	}
}

func TestCoordinatorAsyncAwaitReturnsTaskNotificationXML(t *testing.T) {
	llm := &fakeLLM{}
	llm.streamFn = func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
		return []agent.StreamEvent{{Type: "text", Text: "worker async done"}, {Done: true}}
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Permission: agent.PermissionConfig{
			Mode: agent.PermissionModeFullAuto,
		},
	})

	notification, err := a.Coordinator().StartWorker(context.Background(), agent.WorkerTaskRequest{
		Task: "async worker task",
		Mode: agent.WorkerModeWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	if notification.Status != agent.TaskStatusRunning || notification.AgentID != "main.1" {
		t.Fatalf("unexpected start notification: %+v", notification)
	}
	if !strings.Contains(notification.String(), "<task-notification") {
		t.Fatalf("notification should render XML: %s", notification.String())
	}

	done, err := a.Coordinator().AwaitTask(context.Background(), notification.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != agent.TaskStatusCompleted || !strings.Contains(done.Content, "worker async done") {
		t.Fatalf("unexpected completion notification: %+v", done)
	}
}

func TestCoordinatorForkModeInheritsParentConversation(t *testing.T) {
	const marker = "fork-parent-context-marker"
	llm := &fakeLLM{}
	llm.streamFn = func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
		if call == 0 {
			return []agent.StreamEvent{{Type: "text", Text: "parent ready"}, {Done: true}}
		}
		formatted := agent.FormatMessages(messages)
		if !strings.Contains(formatted, marker) {
			t.Fatalf("fork worker did not inherit parent conversation:\n%s", formatted)
		}
		return []agent.StreamEvent{{Type: "text", Text: "fork worker done"}, {Done: true}}
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Permission: agent.PermissionConfig{
			Mode: agent.PermissionModeFullAuto,
		},
	})
	if _, err := a.RunToolLoop(context.Background(), "remember "+marker); err != nil {
		t.Fatal(err)
	}
	notification, err := a.Coordinator().StartWorker(context.Background(), agent.WorkerTaskRequest{
		Task: "fork task",
		Mode: agent.WorkerModeFork,
	})
	if err != nil {
		t.Fatal(err)
	}
	done, err := a.Coordinator().AwaitTask(context.Background(), notification.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != agent.TaskStatusCompleted {
		t.Fatalf("unexpected fork status: %+v", done)
	}
}

func TestCoordinatorTaskStopKillsRunningWorker(t *testing.T) {
	llm := &fakeLLM{}
	llm.streamFn = func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
		<-ctx.Done()
		return []agent.StreamEvent{{Err: ctx.Err(), Done: true}}
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Permission: agent.PermissionConfig{
			Mode: agent.PermissionModeFullAuto,
		},
	})
	notification, err := a.Coordinator().StartWorker(context.Background(), agent.WorkerTaskRequest{
		Task: "long worker",
		Mode: agent.WorkerModeWorker,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := a.Coordinator().StopTask(notification.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Status != agent.TaskStatusKilled {
		t.Fatalf("expected killed task, got %+v", stopped)
	}
}

func TestToolErrorPreservesOutput(t *testing.T) {
	llm := &fakeLLM{}
	llm.streamFn = func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
		if call == 0 {
			input := `{"command":"printf stdout-text; printf stderr-text >&2; exit 7"}`
			return []agent.StreamEvent{
				{Type: "tool_use", ToolID: "tool1", ToolCallID: "tool1", ToolName: "bash"},
				{Type: "tool_use_end", ToolID: "tool1", ToolCallID: "tool1", ToolName: "bash", ToolInput: input},
				{Done: true},
			}
		}

		formatted := agent.FormatMessages(messages)
		if !strings.Contains(formatted, "[TOOL_ERROR]") {
			t.Fatalf("tool error marker missing from messages:\n%s", formatted)
		}
		if !strings.Contains(formatted, "stdout-text") || !strings.Contains(formatted, "stderr-text") {
			t.Fatalf("tool stdout/stderr not preserved:\n%s", formatted)
		}
		return []agent.StreamEvent{
			{Type: "text", Text: "saw error output"},
			{Done: true},
		}
	}

	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Permission: agent.PermissionConfig{
			Mode: agent.PermissionModeFullAuto,
		},
	})
	resp, err := a.RunToolLoop(context.Background(), "run failing command")
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "saw error output" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestCompactUsesContextAndChatOptions(t *testing.T) {
	traceCtx := agent.WithTraceID(context.Background(), "trace-compact")
	called := false
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			called = true
			if got := agent.TraceIDFromContext(ctx); got != "trace-compact" {
				t.Fatalf("trace id not propagated: %q", got)
			}
			if opts.Model != "compact-model" {
				t.Fatalf("model not propagated: %q", opts.Model)
			}
			if opts.System == nil || *opts.System != "compact-system" {
				t.Fatalf("system not propagated: %#v", opts.System)
			}
			if !opts.PromptCache.Enabled || opts.PromptCache.Key != "cache-key" {
				t.Fatalf("prompt cache not propagated: %+v", opts.PromptCache)
			}
			return agent.LLMResponse{Content: "summary", StopReason: "stop"}, nil
		},
	}
	system := "compact-system"
	messages := []agent.Message{
		{Role: "user", Content: []agent.ContentBlock{agent.TextBlock("old question")}},
		{Role: "assistant", Content: []agent.ContentBlock{agent.TextBlock("old answer")}},
		{Role: "user", Content: []agent.ContentBlock{agent.TextBlock("new question")}},
	}

	compacted := agent.CompactMessagesWithOptions(messages, llm, agent.CompactOptions{
		KeepRecentRounds: 1,
		Context:          traceCtx,
		ChatOptions: agent.ChatOptions{
			Model:  "compact-model",
			System: &system,
			PromptCache: agent.PromptCacheConfig{
				Enabled: true,
				Key:     "cache-key",
			},
		},
	})
	if !called {
		t.Fatal("compact llm was not called")
	}
	if len(compacted) != 2 || !strings.Contains(agent.FormatMessages(compacted), "[CONVERSATION SUMMARY]") {
		t.Fatalf("unexpected compacted messages: %+v", compacted)
	}
}

func TestOpenAICompatibleStreamParsesSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req["stream"] != true {
			t.Fatalf("stream was not enabled: %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{\\\"path\\\"\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\":\\\"README.md\\\"}\"}}]}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"prompt_tokens_details\":{\"cached_tokens\":1}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	llm := agent.NewOpenAILLM(agent.ClientConfig{BaseURL: server.URL, APIKey: "test"})
	events := collectEvents(llm.Stream(context.Background(), []agent.Message{
		{Role: "user", Content: []agent.ContentBlock{agent.TextBlock("hello")}},
	}, agent.ChatOptions{Model: "m"}))

	if !containsText(events, "hi") {
		t.Fatalf("text delta missing: %+v", events)
	}
	end := findEvent(events, "tool_use_end")
	if end == nil || end.ToolID != "call_1" || end.ToolName != "read" || end.ToolInput != `{"path":"README.md"}` {
		t.Fatalf("tool end not parsed correctly: %+v", end)
	}
	usage := findEvent(events, "usage")
	if usage == nil || usage.InputTokens != 3 || usage.OutputTokens != 4 || usage.CachedInputTokens != 1 {
		t.Fatalf("usage not parsed correctly: %+v", usage)
	}
}

func TestAnthropicCompatibleStreamParsesSSE(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req["stream"] != true {
			t.Fatalf("stream was not enabled: %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeSSE(w, "message_start", `{"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":0,"cache_read_input_tokens":1}}}`)
		writeSSE(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)
		writeSSE(w, "content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_a","name":"read","input":{}}}`)
		writeSSE(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\""}}`)
		writeSSE(w, "content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":":\"README.md\"}"}}`)
		writeSSE(w, "content_block_stop", `{"type":"content_block_stop","index":1}`)
		writeSSE(w, "message_delta", `{"type":"message_delta","usage":{"input_tokens":5,"output_tokens":6,"cache_read_input_tokens":1}}`)
		writeSSE(w, "message_stop", `{"type":"message_stop"}`)
	}))
	defer server.Close()

	llm := agent.NewAnthropicLLM(agent.ClientConfig{BaseURL: server.URL, APIKey: "test"})
	events := collectEvents(llm.Stream(context.Background(), []agent.Message{
		{Role: "user", Content: []agent.ContentBlock{agent.TextBlock("hello")}},
	}, agent.ChatOptions{Model: "m"}))

	if !containsText(events, "hello") {
		t.Fatalf("text delta missing: %+v", events)
	}
	end := findEvent(events, "tool_use_end")
	if end == nil || end.ToolID != "tool_a" || end.ToolName != "read" || end.ToolInput != `{"path":"README.md"}` {
		t.Fatalf("tool end not parsed correctly: %+v", end)
	}
	usage := findLastEvent(events, "usage")
	if usage == nil || usage.InputTokens != 5 || usage.OutputTokens != 6 || usage.CachedInputTokens != 1 {
		t.Fatalf("usage not parsed correctly: %+v", usage)
	}
}

func TestBaseURLIsOptionalForLLMConstruction(t *testing.T) {
	if _, err := agent.CreateLLM(agent.LLMConfig{Provider: agent.ProviderOpenAI, APIKey: "test"}); err != nil {
		t.Fatalf("openai without base url should construct: %v", err)
	}
	if _, err := agent.CreateLLM(agent.LLMConfig{Provider: agent.ProviderAnthropic, APIKey: "test"}); err != nil {
		t.Fatalf("anthropic without base url should construct: %v", err)
	}
}

func collectEvents(ch <-chan agent.StreamEvent) []agent.StreamEvent {
	var events []agent.StreamEvent
	for event := range ch {
		events = append(events, event)
	}
	return events
}

func containsText(events []agent.StreamEvent, text string) bool {
	for _, event := range events {
		if event.Text == text {
			return true
		}
	}
	return false
}

func findEvent(events []agent.StreamEvent, typ string) *agent.StreamEvent {
	for i := range events {
		if events[i].Type == typ {
			return &events[i]
		}
	}
	return nil
}

func findLastEvent(events []agent.StreamEvent, typ string) *agent.StreamEvent {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].Type == typ {
			return &events[i]
		}
	}
	return nil
}

func writeSSE(w http.ResponseWriter, event string, data string) {
	fmt.Fprintf(w, "event: %s\n", event)
	fmt.Fprintf(w, "data: %s\n\n", data)
}
