package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
	tool "github.com/GongShichen/CodingMan/tool"
)

type echoTool struct{}

func (echoTool) Name() string { return "echo" }
func (echoTool) Description() string {
	return "echo input value"
}
func (echoTool) InputSchema() map[string]any {
	return map[string]any{"type": "object"}
}
func (echoTool) ToAPIFormat() map[string]any {
	return map[string]any{"name": "echo", "description": "echo input value", "input_schema": echoTool{}.InputSchema()}
}
func (echoTool) Call(input map[string]any) (string, error) {
	value, _ := input["value"].(string)
	return value, nil
}

func TestPreToolUseHookCanUpdateInput(t *testing.T) {
	manager, err := agent.NewHookManager(agent.HooksConfig{Hooks: []agent.HookConfig{{
		Name:     "rewrite",
		Type:     agent.HookTypeFunction,
		Event:    agent.HookEventPreToolUse,
		ToolName: "echo",
		Function: "rewrite",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	manager.RegisterFunction("rewrite", func(ctx context.Context, payload agent.HookPayload) (agent.HookResult, error) {
		return agent.HookResult{UpdatedInput: map[string]any{"value": "updated"}}, nil
	})

	registry := tool.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:      &fakeLLM{},
		Registry: registry,
		Hooks:    manager,
		Permission: agent.PermissionConfig{
			Mode:         agent.PermissionModeAllowDeny,
			AllowedTools: []string{"echo"},
		},
	})
	result, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "echo",
		"arguments": `{"value":"original"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(result) != "updated" {
		t.Fatalf("hook did not update input, got %q", result)
	}
}

func TestPostToolUseHookRuns(t *testing.T) {
	ran := false
	manager, err := agent.NewHookManager(agent.HooksConfig{Hooks: []agent.HookConfig{{
		Name:     "post",
		Type:     agent.HookTypeFunction,
		Event:    agent.HookEventPostToolUse,
		ToolName: "echo",
		Function: "post",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	manager.RegisterFunction("post", func(ctx context.Context, payload agent.HookPayload) (agent.HookResult, error) {
		ran = payload.Output == "value"
		return agent.HookResult{}, nil
	})

	registry := tool.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	a := agent.NewAgent(agent.AgentConfig{
		LLM:      &fakeLLM{},
		Registry: registry,
		Hooks:    manager,
		Permission: agent.PermissionConfig{
			Mode:         agent.PermissionModeAllowDeny,
			AllowedTools: []string{"echo"},
		},
	})
	if _, err := a.ExecuteTool(context.Background(), map[string]any{
		"name":      "echo",
		"arguments": `{"value":"value"}`,
	}); err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("post hook did not run")
	}
}
