package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
)

func TestHandleSystemCommandLoadsPromptFile(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "SYSTEM.md")
	if err := os.WriteFile(promptPath, []byte("custom slash system prompt"), 0600); err != nil {
		t.Fatal(err)
	}

	a := agent.NewAgent(agent.AgentConfig{LLM: testLLM{}})
	if !handleSystemCommand(a, []string{"/system", promptPath}) {
		t.Fatal("system command was not handled")
	}
	if !strings.Contains(a.SystemPrompt(), "custom slash system prompt") {
		t.Fatalf("system prompt was not loaded:\n%s", a.SystemPrompt())
	}
}

func TestLoadRuntimeConfigDefaultsCwdToLaunchDir(t *testing.T) {
	projectRoot := t.TempDir()
	launchDir := filepath.Join(projectRoot, "subdir")
	if err := os.MkdirAll(launchDir, 0755); err != nil {
		t.Fatal(err)
	}
	env := strings.Join([]string{
		"PROVIDER=OpenAI",
		"MODEL_NAME=test-model",
		"API_KEY=test-key",
	}, "\n")
	if err := os.WriteFile(filepath.Join(projectRoot, ".env"), []byte(env), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadRuntimeConfig(projectRoot, launchDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context.Cwd != launchDir {
		t.Fatalf("cwd should default to launch dir: got %q want %q", cfg.Context.Cwd, launchDir)
	}
}

type testLLM struct{}

func (testLLM) Chat(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) (agent.LLMResponse, error) {
	return agent.LLMResponse{Content: "ok", StopReason: "stop"}, nil
}

func (testLLM) Stream(ctx context.Context, messages []agent.Message, opts agent.ChatOptions) <-chan agent.StreamEvent {
	ch := make(chan agent.StreamEvent, 1)
	close(ch)
	return ch
}
