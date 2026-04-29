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

func TestParseMCPServersSupportsMapAndArray(t *testing.T) {
	mapped, err := parseMCPServers([]byte(`{"mcpServers":{"docs":{"transport":"http","url":"http://example.test/mcp"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(mapped) != 1 || mapped[0].Name != "docs" || mapped[0].URL == "" {
		t.Fatalf("unexpected mapped mcp config: %+v", mapped)
	}
	list, err := parseMCPServers([]byte(`{"mcp_servers":[{"name":"local","transport":"stdio","command":"mcp"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Name != "local" || list[0].Command != "mcp" {
		t.Fatalf("unexpected list mcp config: %+v", list)
	}
}

func TestParseCLIOptionsEnablesNonInteractiveForPromptFile(t *testing.T) {
	options, err := parseCLIOptions([]string{"--prompt-file", "task.md", "--cwd", "/tmp/repo", "--permission", "full-auto"})
	if err != nil {
		t.Fatal(err)
	}
	if !options.NonInteractive {
		t.Fatal("prompt-file should enable non-interactive mode")
	}
	if options.PromptFile != "task.md" || options.Cwd != "/tmp/repo" || options.Permission != "full-auto" {
		t.Fatalf("unexpected options: %+v", options)
	}
}

func TestParseCLIOptionsRequiresPromptForExplicitNonInteractive(t *testing.T) {
	if _, err := parseCLIOptions([]string{"--non-interactive"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestApplyCLIOptionsOverridesRuntimeConfig(t *testing.T) {
	dir := t.TempDir()
	cfg := RuntimeConfig{
		Context:      agent.DefaultContextConfig(),
		MaxLLMTurns:  20,
		MaxToolCalls: 50,
	}
	if err := applyCLIOptions(&cfg, CLIOptions{
		Cwd:          dir,
		Permission:   "full-auto",
		MaxLLMTurns:  3,
		MaxToolCalls: 4,
	}); err != nil {
		t.Fatal(err)
	}
	if cfg.Context.Cwd != dir {
		t.Fatalf("cwd = %q, want %q", cfg.Context.Cwd, dir)
	}
	if cfg.Permission.Mode != agent.PermissionModeFullAuto {
		t.Fatalf("permission mode = %q", cfg.Permission.Mode)
	}
	if cfg.MaxLLMTurns != 3 || cfg.MaxToolCalls != 4 {
		t.Fatalf("limits not overridden: turns=%d tools=%d", cfg.MaxLLMTurns, cfg.MaxToolCalls)
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
