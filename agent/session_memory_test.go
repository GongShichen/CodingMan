package agent_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GongShichen/CodingMan/agent"
	tool "github.com/GongShichen/CodingMan/tool"
)

func TestSessionMemoryUpdatesAfterToolCallThreshold(t *testing.T) {
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:                    &fakeLLM{},
		Registry:               registry,
		SessionMemoryThreshold: 2,
		Permission: agent.PermissionConfig{
			Mode:         agent.PermissionModeAllowDeny,
			AllowedTools: []string{"echo"},
		},
	})

	for i := 0; i < 2; i++ {
		if _, err := a.ExecuteTool(context.Background(), map[string]any{
			"name":      "echo",
			"arguments": `{"value":"ok"}`,
		}); err != nil {
			t.Fatal(err)
		}
	}
	memories := a.SessionMemory()
	if len(memories) != 1 {
		t.Fatalf("expected one session memory, got %+v", memories)
	}
	if memories[0].ToolCallCount != 2 {
		t.Fatalf("unexpected tool count: %+v", memories[0])
	}
}

func TestSessionMemoryRestoresAndEntersSystemPrompt(t *testing.T) {
	called := false
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			called = true
			if opts.System == nil || !strings.Contains(*opts.System, "remembered fact") {
				t.Fatalf("session memory missing from system prompt: %#v", opts.System)
			}
			return agent.LLMResponse{Content: "ok", StopReason: "completed"}, nil
		},
	}
	a := agent.NewAgent(agent.AgentConfig{LLM: llm})
	a.Restore(agent.SessionSnapshot{
		SessionID: "s1",
		Memory: []agent.SessionMemoryEntry{{
			ID:      "memory-1",
			Content: "remembered fact",
		}},
	})
	if _, err := a.Chat(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("llm was not called")
	}
}

func TestCrossSessionMemoryUpdateWritesProjectFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	store, err := agent.NewCrossSessionMemoryStore(project)
	if err != nil {
		t.Fatal(err)
	}
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			return agent.LLMResponse{Content: "```json\n[{\"filename\":\"user_prefs.md\",\"name\":\"selection prompts\",\"description\":\"User prefers option-based prompts.\",\"type\":\"user\",\"content\":\"Use selectable choices for yes/no questions instead of free-form input.\"}]\n```", StopReason: "completed"}, nil
		},
	}
	if err := store.Update(context.Background(), llm, "test-model", agent.PromptCacheConfig{}, "user prefers selectable yes/no", 4000); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join(store.Dir(), "user_prefs.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Use selectable choices") {
		t.Fatalf("cross-session memory was not written:\n%s", string(content))
	}
}

func TestCrossSessionMemoryIndexAndReferenceFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	store, err := agent.NewCrossSessionMemoryStore(project)
	if err != nil {
		t.Fatal(err)
	}
	index, err := os.ReadFile(filepath.Join(store.Dir(), "MEMORY.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "项目记忆索引") || !strings.Contains(string(index), "project_stack.md") {
		t.Fatalf("unexpected memory index:\n%s", string(index))
	}
	if err := store.AppendItems([]agent.CrossMemoryItem{{
		Type:    "reference",
		Name:    "claude style",
		Content: "TUI visual style may reference Claude Code.",
	}}); err != nil {
		t.Fatal(err)
	}
	ref, err := os.ReadFile(filepath.Join(store.Dir(), "references.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ref), "Claude Code") {
		t.Fatalf("reference memory missing:\n%s", string(ref))
	}
}

func TestScheduleCrossSessionMemoryExtractionRunsInBackground(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	var calls atomic.Int32
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			if calls.Add(1) == 1 {
				time.Sleep(50 * time.Millisecond)
				return agent.LLMResponse{Content: "用户偏好：后台提取跨会话记忆。", StopReason: "completed"}, nil
			}
			return agent.LLMResponse{Content: `[{"filename":"user_prefs.md","name":"background memory","description":"Cross-session extraction runs asynchronously.","type":"user","content":"Cross-session memory extraction should not block the TUI turn."}]`, StopReason: "completed"}, nil
		},
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Context: agent.ContextConfig{
			Cwd:         project,
			ProjectRoot: project,
			BaseSystem:  "base",
		},
	})
	start := time.Now()
	if !a.ScheduleCrossSessionMemoryExtraction(context.Background()) {
		t.Fatal("expected cross-session memory job to be scheduled")
	}
	if elapsed := time.Since(start); elapsed > 25*time.Millisecond {
		t.Fatalf("schedule blocked for %s", elapsed)
	}

	memoryPath := filepath.Join(home, ".codingman", "projects", agent.ProjectHash(project), "memory", "user_prefs.md")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(memoryPath)
		if err == nil && strings.Contains(string(data), "should not block") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background memory was not written to %s", memoryPath)
}

func TestSessionMemoryBudgetTruncatesSystemInjection(t *testing.T) {
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			if opts.System == nil {
				t.Fatal("missing system prompt")
			}
			if strings.Contains(*opts.System, "old memory should be pruned") {
				t.Fatalf("old memory exceeded budget:\n%s", *opts.System)
			}
			return agent.LLMResponse{Content: "ok", StopReason: "completed"}, nil
		},
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:                     llm,
		MaxSessionMemoryEntries: 1,
		MaxSessionMemoryChars:   32,
	})
	a.Restore(agent.SessionSnapshot{
		SessionID: "s1",
		Memory: []agent.SessionMemoryEntry{
			{ID: "memory-1", Content: "old memory should be pruned"},
			{ID: "memory-2", Content: "new memory is retained but will be cut"},
		},
	})
	if _, err := a.Chat(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
}
