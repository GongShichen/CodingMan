package agent_test

import (
	"context"
	"fmt"
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
	if err := registry.Register(tool.NewWriteTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewEditTool()); err != nil {
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

func TestSkillAllowToolsRestrictsRuntimeTools(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".codingman", "skills", "echo-only", "SKILL.md"), "---\nname: echo-only\ndescription: echo only\nallow_tools: [echo]\ncontext: fork\n---\n")
	readPath := filepath.Join(project, "readme.txt")
	writeFile(t, readPath, "readable\n")
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewReadTool()); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:      &fakeLLM{},
		Registry: registry,
		Context: agent.ContextConfig{
			Cwd:         project,
			ProjectRoot: project,
			BaseSystem:  "base",
			LoadSkills:  true,
		},
		Permission: agent.PermissionConfig{
			Mode:         agent.PermissionModeAllowDeny,
			AllowedTools: []string{"echo", "read"},
		},
	})
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "read",
		"arguments": fmt.Sprintf(`{"filePath":%q}`, readPath),
	}); err != nil {
		t.Fatalf("loaded but inactive skill should not restrict tools: %v", err)
	}
	if err := a.SetActiveSkill("echo-only"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "echo",
		"arguments": `{"value":"ok"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "read",
		"arguments": fmt.Sprintf(`{"filePath":%q}`, readPath),
	}); err == nil || !strings.Contains(err.Error(), "allow_tools") {
		t.Fatalf("expected skill allow_tools denial, got %v", err)
	}
}

func TestSkillEvolutionCreatesUserSkillAfterThreshold(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var reviewCalls atomic.Int64
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			reviewCalls.Add(1)
			prompt := agent.FormatMessages(messages)
			if !strings.Contains(prompt, "经验审查器") || !strings.Contains(prompt, "严格 JSON 数组") {
				t.Fatalf("unexpected skill evolution prompt:\n%s", prompt)
			}
			return agent.LLMResponse{Content: `[{"action":"create","name":"Go Test Fixes","description":"Capture reusable Go test debugging workflow.","content":"---\nname: go-test-fixes\ndescription: Capture reusable Go test debugging workflow.\nallow_tools: [read, grep, bash, edit]\ncontext: fork\n---\n\n# Go Test Fixes\n\nRun focused tests, inspect exact failure output, then patch the smallest failing surface.\n"}]`, StopReason: "completed"}, nil
		},
	}
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewWriteTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewEditTool()); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:                     llm,
		Registry:                registry,
		SessionMemoryThreshold:  100,
		SkillEvolutionThreshold: 2,
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
	waitForCondition(t, func() bool { return reviewCalls.Load() == 1 })
	if reviewCalls.Load() != 1 {
		t.Fatalf("expected one skill review, got %d", reviewCalls.Load())
	}
	path := filepath.Join(home, ".codingman", "skills", "go-test-fixes", "SKILL.md")
	var data []byte
	waitForCondition(t, func() bool {
		var err error
		data, err = os.ReadFile(path)
		return err == nil && strings.Contains(string(data), "Run focused tests")
	})
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Run focused tests") {
		t.Fatalf("skill content missing:\n%s", string(data))
	}

	if err := a.SetActiveSkill("go-test-fixes"); err != nil {
		t.Fatalf("created skill was not reloaded: %v", err)
	}
}

func TestSkillEvolutionEmptyReviewResetsCounter(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var reviewCalls atomic.Int64
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			reviewCalls.Add(1)
			return agent.LLMResponse{Content: `[]`, StopReason: "completed"}, nil
		},
	}
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewWriteTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewEditTool()); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:                     llm,
		Registry:                registry,
		SessionMemoryThreshold:  100,
		SkillEvolutionThreshold: 2,
		Permission: agent.PermissionConfig{
			Mode:         agent.PermissionModeAllowDeny,
			AllowedTools: []string{"echo"},
		},
	})
	for i := 0; i < 3; i++ {
		if _, err := a.ExecuteTool(context.Background(), map[string]any{
			"name":      "echo",
			"arguments": `{"value":"ok"}`,
		}); err != nil {
			t.Fatal(err)
		}
	}
	waitForCondition(t, func() bool { return reviewCalls.Load() == 1 })
	if reviewCalls.Load() != 1 {
		t.Fatalf("expected one review after counter reset, got %d", reviewCalls.Load())
	}
}

func TestSkillEvolutionParsesNoisyJSONAndExecuteToolsTriggersReview(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	var reviewCalls atomic.Int64
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			reviewCalls.Add(1)
			return agent.LLMResponse{Content: "Here is the update:\n```json\n[{\"action\":\"create\",\"name\":\"Noisy JSON Skill\",\"description\":\"Parse fenced JSON output.\",\"content\":\"---\\nname: noisy-json-skill\\ndescription: Parse fenced JSON output.\\nallow_tools: [read]\\ncontext: fork\\n---\\n\\n# Noisy JSON Skill\\n\\nKeep only the JSON array from noisy model output.\\n\"}]\n```", StopReason: "completed"}, nil
		},
	}
	registry := tool.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewWriteTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewEditTool()); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:                     llm,
		Registry:                registry,
		SessionMemoryThreshold:  100,
		SkillEvolutionThreshold: 2,
		Permission: agent.PermissionConfig{
			Mode:         agent.PermissionModeAllowDeny,
			AllowedTools: []string{"echo"},
		},
	})
	results := a.ExecuteTools(context.Background(), []agent.ToolUse{
		{ID: "one", Name: "echo", Input: `{"value":"one"}`},
		{ID: "two", Name: "echo", Input: `{"value":"two"}`},
	}, 2)
	if len(results) != 2 || results[0].IsError || results[1].IsError {
		t.Fatalf("unexpected tool results: %+v", results)
	}
	path := filepath.Join(home, ".codingman", "skills", "noisy-json-skill", "SKILL.md")
	waitForCondition(t, func() bool {
		data, err := os.ReadFile(path)
		return err == nil && strings.Contains(string(data), "Keep only the JSON array")
	})
	if reviewCalls.Load() != 1 {
		t.Fatalf("expected one review, got %d", reviewCalls.Load())
	}
}

func TestAutoSelectSkillInjectsUserSkillAndRecordsUsage(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	writeFile(t, filepath.Join(home, ".codingman", "skills", "go-test", "SKILL.md"), "---\nname: go-test\ndescription: Go test workflow\nallow_tools: [echo]\ncontext: fork\n---\n\nRun focused go tests before editing.\n")
	var sawActiveSkill bool
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			return agent.LLMResponse{Content: `{"skill":"go-test","reason":"matches go test task"}`, StopReason: "completed"}, nil
		},
		streamFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
			if opts.System == nil || !strings.Contains(*opts.System, "## Active Skill") || !strings.Contains(*opts.System, "Run focused go tests") {
				t.Fatalf("active skill missing from system:\n%v", opts.System)
			}
			sawActiveSkill = true
			return []agent.StreamEvent{{Type: "text", Text: "ok"}, {Done: true}}
		},
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Context: agent.ContextConfig{
			Cwd:         project,
			ProjectRoot: project,
			BaseSystem:  "base",
			LoadSkills:  true,
		},
		Permission: agent.PermissionConfig{Mode: agent.PermissionModeFullAuto},
	})
	var selected string
	a.SetEventSink(func(event agent.AgentEvent) {
		if event.Type == agent.AgentEventSkillSelected && event.SkillSelected != nil {
			selected = event.SkillSelected.Skill.Name
		}
	})
	if _, err := a.RunToolLoop(context.Background(), "fix go test"); err != nil {
		t.Fatal(err)
	}
	if !sawActiveSkill || selected != "go-test" {
		t.Fatalf("skill was not selected/injected, selected=%q saw=%v", selected, sawActiveSkill)
	}
	usagePath := filepath.Join(home, ".codingman", "skills", ".codingman_usage.json")
	data, err := os.ReadFile(usagePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"use_count": 1`) || !strings.Contains(string(data), "go-test") {
		t.Fatalf("usage was not recorded:\n%s", string(data))
	}
}

func TestManualSkillTakesPrecedenceOverAutoSelect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	writeFile(t, filepath.Join(home, ".codingman", "skills", "manual", "SKILL.md"), "---\nname: manual\ndescription: manual skill\nallow_tools: []\ncontext: fork\n---\n\nmanual body\n")
	writeFile(t, filepath.Join(home, ".codingman", "skills", "auto", "SKILL.md"), "---\nname: auto\ndescription: auto skill\nallow_tools: []\ncontext: fork\n---\n\nauto body\n")
	var chatCalls atomic.Int64
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			chatCalls.Add(1)
			return agent.LLMResponse{Content: `{"skill":"auto","reason":"would match"}`, StopReason: "completed"}, nil
		},
		streamFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
			if opts.System == nil || !strings.Contains(*opts.System, "manual body") || strings.Contains(*opts.System, "## Active Skill\nauto body") {
				t.Fatalf("manual active skill did not take precedence:\n%v", opts.System)
			}
			return []agent.StreamEvent{{Type: "text", Text: "ok"}, {Done: true}}
		},
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Context: agent.ContextConfig{
			Cwd:         project,
			ProjectRoot: project,
			BaseSystem:  "base",
			LoadSkills:  true,
		},
		Permission: agent.PermissionConfig{Mode: agent.PermissionModeFullAuto},
	})
	if err := a.SetActiveSkill("manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.RunToolLoop(context.Background(), "use a skill"); err != nil {
		t.Fatal(err)
	}
	if chatCalls.Load() != 0 {
		t.Fatalf("auto selector should not call llm when manual skill is active")
	}
}

func TestInvalidAutoSkillSelectionDoesNotBlockExecution(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	writeFile(t, filepath.Join(home, ".codingman", "skills", "docs", "SKILL.md"), "---\nname: docs\ndescription: docs\ncontext: fork\n---\n\ndocs body\n")
	llm := &fakeLLM{
		chatFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
			return agent.LLMResponse{Content: `not json`, StopReason: "completed"}, nil
		},
		streamFn: func(ctx context.Context, messages []agent.Message, opts agent.ChatOptions, call int) []agent.StreamEvent {
			if opts.System != nil && strings.Contains(*opts.System, "## Active Skill") {
				t.Fatalf("invalid selection should not inject active skill:\n%s", *opts.System)
			}
			return []agent.StreamEvent{{Type: "text", Text: "ok"}, {Done: true}}
		},
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM: llm,
		Context: agent.ContextConfig{
			Cwd:         project,
			ProjectRoot: project,
			BaseSystem:  "base",
			LoadSkills:  true,
		},
		Permission: agent.PermissionConfig{Mode: agent.PermissionModeFullAuto},
	})
	if _, err := a.RunToolLoop(context.Background(), "write docs"); err != nil {
		t.Fatal(err)
	}
}

func TestSkillEvictionOnlyRemovesOldLowUseGeneratedUserSkills(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	old := time.Now().AddDate(0, 0, -120).UTC().Format(time.RFC3339)
	writeFile(t, filepath.Join(home, ".codingman", "skills", "generated-old", "SKILL.md"), "---\nname: generated-old\ndescription: old generated\ncodingman_generated: true\ncreated_at: "+old+"\nupdated_at: "+old+"\ncontext: fork\n---\n\nold\n")
	writeFile(t, filepath.Join(home, ".codingman", "skills", "manual-old", "SKILL.md"), "---\nname: manual-old\ndescription: manual\ncreated_at: "+old+"\nupdated_at: "+old+"\ncontext: fork\n---\n\nmanual\n")
	writeFile(t, filepath.Join(project, ".codingman", "skills", "project-generated", "SKILL.md"), "---\nname: project-generated\ndescription: project\ncodingman_generated: true\ncreated_at: "+old+"\nupdated_at: "+old+"\ncontext: fork\n---\n\nproject\n")
	writeFile(t, filepath.Join(home, ".codingman", "skills", ".codingman_usage.json"), `{"last_eviction_check_at":"`+time.Now().UTC().Format(time.RFC3339)+`","skills":{}}`+"\n")
	a := agent.NewAgent(agent.AgentConfig{
		LLM: &fakeLLM{},
		Context: agent.ContextConfig{
			Cwd:         project,
			ProjectRoot: project,
			BaseSystem:  "base",
			LoadSkills:  true,
		},
		SkillEviction: agent.SkillEvictionConfig{Enabled: true, UnusedDays: 90, MinUses: 3, CheckIntervalHours: 1},
	})
	result, err := a.MaybeEvictGeneratedSkills(time.Now().UTC().Add(2 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Evicted) != 1 || result.Evicted[0] != "generated-old" {
		t.Fatalf("unexpected evictions: %+v", result)
	}
	if _, err := os.Stat(filepath.Join(home, ".codingman", "skills", "generated-old")); !os.IsNotExist(err) {
		t.Fatalf("generated old skill should be deleted, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".codingman", "skills", "manual-old", "SKILL.md")); err != nil {
		t.Fatalf("manual skill must remain: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".codingman", "skills", "project-generated", "SKILL.md")); err != nil {
		t.Fatalf("project skill must remain: %v", err)
	}
}

func TestWriteAndEditEmitDiffEvents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	registry := tool.NewRegistry()
	if err := registry.Register(tool.NewWriteTool()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(tool.NewEditTool()); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:      &fakeLLM{},
		Registry: registry,
		Permission: agent.PermissionConfig{
			Mode:         agent.PermissionModeAllowDeny,
			AllowedTools: []string{"write", "edit"},
		},
	})
	var diffs []string
	a.SetEventSink(func(event agent.AgentEvent) {
		if event.Type == agent.AgentEventFileDiff && event.FileDiff != nil {
			diffs = append(diffs, event.FileDiff.Diff)
		}
	})
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "write",
		"arguments": fmt.Sprintf(`{"filePath":%q,"content":"alpha\nbeta\n"}`, path),
	}); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 1 || !strings.Contains(diffs[0], "+alpha") || !strings.Contains(diffs[0], "+beta") {
		t.Fatalf("write diff missing additions: %+v", diffs)
	}
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "write",
		"arguments": fmt.Sprintf(`{"filePath":%q,"content":"alpha\nbeta2\n","overwrite":true}`, path),
	}); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 2 || !strings.Contains(diffs[1], "-beta") || !strings.Contains(diffs[1], "+beta2") {
		t.Fatalf("overwrite diff missing changes: %+v", diffs)
	}
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "edit",
		"arguments": fmt.Sprintf(`{"filePath":%q,"oldText":"beta2","newText":"gamma"}`, path),
	}); err != nil {
		t.Fatal(err)
	}
	if len(diffs) != 3 || !strings.Contains(diffs[2], "-beta2") || !strings.Contains(diffs[2], "+gamma") {
		t.Fatalf("edit diff missing changes: %+v", diffs)
	}
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "edit",
		"arguments": fmt.Sprintf(`{"filePath":%q,"oldText":"missing","newText":"noop"}`, path),
	}); err == nil {
		t.Fatal("expected failed edit")
	}
	if len(diffs) != 3 {
		t.Fatalf("failed edit should not emit diff: %+v", diffs)
	}
}

func waitForCondition(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition was not met before deadline")
}
