package main

import (
	"bufio"
	"context"
	"errors"
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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SANDBOX_BOOTSTRAP", agent.SandboxBootstrapFalse)
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

	cfg, _, err := loadRuntimeConfig(projectRoot, projectRoot, launchDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Context.Cwd != launchDir {
		t.Fatalf("cwd should default to launch dir: got %q want %q", cfg.Context.Cwd, launchDir)
	}
}

func TestLoadRuntimeConfigSetsSandboxEnvDefaults(t *testing.T) {
	projectRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SANDBOX_VFKIT", "")
	t.Setenv("SANDBOX_ENABLED", "")
	t.Setenv("SANDBOX_BOOTSTRAP", agent.SandboxBootstrapFalse)
	env := strings.Join([]string{
		"PROVIDER=OpenAI",
		"MODEL_NAME=test-model",
		"API_KEY=test-key",
	}, "\n")
	if err := os.WriteFile(filepath.Join(projectRoot, ".env"), []byte(env), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := loadRuntimeConfig(projectRoot, projectRoot, projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("SANDBOX_VFKIT") != "vfkit" {
		t.Fatalf("SANDBOX_VFKIT default = %q", os.Getenv("SANDBOX_VFKIT"))
	}
	if cfg.Sandbox.VFKitPath != "vfkit" || cfg.Sandbox.Enabled != agent.SandboxEnabledAuto {
		t.Fatalf("unexpected sandbox config: %+v", cfg.Sandbox)
	}
	configPath := filepath.Join(home, ".codingman", "sandbox", "config")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "SANDBOX_ROOTFS="+filepath.Join(home, ".codingman", "sandbox", "debian-12-slim-arm64.raw")) {
		t.Fatalf("sandbox config missing rootfs default:\n%s", data)
	}
}

func TestLoadRuntimeConfigFallsBackToUserEnv(t *testing.T) {
	appRoot := t.TempDir()
	workspaceRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SANDBOX_BOOTSTRAP", agent.SandboxBootstrapFalse)
	userConfigDir := filepath.Join(home, ".codingman")
	if err := os.MkdirAll(userConfigDir, 0700); err != nil {
		t.Fatal(err)
	}
	env := strings.Join([]string{
		"PROVIDER=OpenAI",
		"MODEL_NAME=user-model",
		"API_KEY=user-key",
	}, "\n")
	if err := os.WriteFile(filepath.Join(userConfigDir, ".env"), []byte(env), 0600); err != nil {
		t.Fatal(err)
	}

	cfg, source, err := loadRuntimeConfig(appRoot, workspaceRoot, workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(source, filepath.Join(".codingman", ".env")) {
		t.Fatalf("expected user env source, got %q", source)
	}
	if cfg.ModelName != "user-model" || cfg.APIKey != "user-key" {
		t.Fatalf("unexpected config from user env: %+v", cfg)
	}
}

func TestResolveAppRootUsesConfiguredRoot(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{
		"go.mod",
		"main.go",
		filepath.Join("sandbox", "build-rootfs.sh"),
		filepath.Join("cmd", "sandbox-mcp-server", "main.go"),
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CODINGMAN_APP_ROOT", root)
	resolved, err := resolveAppRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if resolved != root {
		t.Fatalf("resolved app root = %q, want %q", resolved, root)
	}
}

func TestSandboxEnvironmentCheckReportsMissingRootFS(t *testing.T) {
	check := checkSandboxEnvironment(t.TempDir(), agent.SandboxConfig{
		Enabled:       agent.SandboxEnabledAuto,
		Bootstrap:     agent.SandboxBootstrapAuto,
		VFKitPath:     "vfkit",
		RootFS:        filepath.Join(t.TempDir(), "missing-rootfs"),
		MCPServerPath: filepath.Join(t.TempDir(), "missing-mcp"),
	})
	if !check.Needed {
		t.Fatal("expected sandbox environment check to need setup")
	}
	summary := check.Summary()
	if strings.Contains(strings.ToLower(summary), "docker") || strings.Contains(strings.ToLower(summary), "colima") {
		t.Fatalf("sandbox check should not mention docker/colima:\n%s", summary)
	}
	if !strings.Contains(summary, "Debian 12 slim VM image") {
		t.Fatalf("missing VM image setup item:\n%s", summary)
	}
	if strings.Contains(strings.ToLower(summary), "debootstrap") {
		t.Fatalf("sandbox check should not mention debootstrap:\n%s", summary)
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

func TestApplyCLIOptionsRequiresFullAutoConfirmationForNonInteractive(t *testing.T) {
	cfg := RuntimeConfig{Context: agent.DefaultContextConfig()}
	t.Setenv("CONFIRM_FULL_AUTO_UNSANDBOXED", "")
	err := applyCLIOptions(&cfg, CLIOptions{NonInteractive: true, Permission: "full-auto"})
	if err == nil {
		t.Fatal("expected full-auto confirmation error")
	}

	t.Setenv("CONFIRM_FULL_AUTO_UNSANDBOXED", "true")
	if err := applyCLIOptions(&cfg, CLIOptions{NonInteractive: true, Permission: "full-auto"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnableFullAutoUnsandboxedDisablesSandbox(t *testing.T) {
	cfg := RuntimeConfig{
		Permission: agent.PermissionConfig{Mode: agent.PermissionModeAsk},
		Sandbox:    agent.SandboxConfig{Enabled: agent.SandboxEnabledAuto},
	}
	enableFullAutoUnsandboxed(&cfg)
	if cfg.Permission.Mode != agent.PermissionModeFullAuto {
		t.Fatalf("permission mode = %q", cfg.Permission.Mode)
	}
	if len(cfg.Permission.AllowedTools) != 1 || cfg.Permission.AllowedTools[0] != "*" {
		t.Fatalf("allowed tools = %+v", cfg.Permission.AllowedTools)
	}
	if cfg.Sandbox.Enabled != agent.SandboxEnabledFalse {
		t.Fatalf("sandbox enabled = %q", cfg.Sandbox.Enabled)
	}
}

func TestConfirmFullAutoAfterSandboxFailure(t *testing.T) {
	tui := newTUIController(bufio.NewScanner(strings.NewReader("1\n")))
	if !tui.confirmFullAutoAfterSandboxFailure(errors.New("download failed")) {
		t.Fatal("expected confirmation")
	}
	tui = newTUIController(bufio.NewScanner(strings.NewReader("no\n")))
	if tui.confirmFullAutoAfterSandboxFailure(errors.New("download failed")) {
		t.Fatal("expected cancellation")
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
