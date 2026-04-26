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
