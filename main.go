package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GongShichen/CodingMan/agent"
)

const (
	colorReset = "\033[0m"
	colorDim   = "\033[2m"
	colorBold  = "\033[1m"
	colorCyan  = "\033[36m"
	colorGreen = "\033[32m"
	colorRed   = "\033[31m"
	colorGray  = "\033[90m"
)

type RuntimeConfig struct {
	Provider  string
	ModelName string
	APIKey    string
	BaseURL   string

	Context          agent.ContextConfig
	MaxLLMTurns      int
	MaxToolCalls     int
	MaxToolErrors    int
	MaxAPIErrors     int
	EnableToolBudget bool
	ToolBudget       agent.ToolBudget
	Retry            agent.RetryConfig
	PromptCache      agent.PromptCacheConfig
}

func main() {
	projectRoot, err := findProjectRoot(".")
	if err != nil {
		fatal("find project root", err)
	}

	cfg, source, err := loadRuntimeConfig(projectRoot)
	if err != nil {
		fatal("load config", err)
	}

	client, err := agent.CreateLLM(agent.LLMConfig{
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
		APIKey:   cfg.APIKey,
	})
	if err != nil {
		fatal("create llm", err)
	}

	a := agent.NewAgent(agent.AgentConfig{
		LLM:                      client,
		Context:                  cfg.Context,
		Model:                    cfg.ModelName,
		MaxLLMTurns:              cfg.MaxLLMTurns,
		MaxToolCalls:             cfg.MaxToolCalls,
		MaxConsecutiveToolErrors: cfg.MaxToolErrors,
		MaxConsecutiveAPIErrors:  cfg.MaxAPIErrors,
		EnableToolBudget:         cfg.EnableToolBudget,
		ToolBudget:               cfg.ToolBudget,
		RetryConfig:              cfg.Retry,
		PromptCache:              cfg.PromptCache,
	})

	RunTUI(a, cfg, source)
}

func RunTUI(a *agent.Agent, cfg RuntimeConfig, source string) {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	if permissions := a.Permission(); permissions != nil {
		permissions.SetAskFunc(tuiPermissionPrompt(scanner))
	}

	printHeader(cfg, source)

	for {
		fmt.Printf("%s>%s ", colorCyan, colorReset)
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				fmt.Fprintf(os.Stderr, "%sread input: %v%s\n", colorRed, err, colorReset)
			}
			return
		}

		prompt := strings.TrimSpace(scanner.Text())
		if prompt == "" {
			continue
		}
		if prompt == "/exit" || prompt == "/quit" {
			fmt.Println(colorDim + "session ended" + colorReset)
			return
		}
		if prompt == "/clear" {
			a.Clear()
			fmt.Println(colorDim + "conversation cleared" + colorReset)
			continue
		}
		if prompt == "/help" {
			printHelp()
			continue
		}
		if strings.HasPrefix(prompt, "/") {
			if handled := handleSlashCommand(a, prompt); handled {
				continue
			}
			fmt.Printf("%sunknown command:%s %s\n", colorRed, colorReset, prompt)
			fmt.Println(colorDim + "Type /help to list slash commands." + colorReset)
			continue
		}

		start := time.Now()
		fmt.Printf("%srunning agent loop...%s\n", colorGray, colorReset)
		resp, err := a.RunToolLoop(context.Background(), prompt)
		if err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			continue
		}

		if resp.Content != "" {
			fmt.Printf("\n%s%s%s\n", colorBold, strings.TrimSpace(resp.Content), colorReset)
		}
		if resp.StopReason != "" && resp.StopReason != "completed" {
			fmt.Printf("%sstop: %s%s\n", colorGray, resp.StopReason, colorReset)
		}
		fmt.Printf("%sinput=%d cached=%d cache_write=%d output=%d retry=%d elapsed=%s%s\n\n",
			colorGray,
			resp.InputTokens,
			resp.CachedInputTokens,
			resp.CacheCreationInputTokens,
			resp.OutputTokens,
			resp.RetryAttempts,
			time.Since(start).Round(time.Millisecond),
			colorReset,
		)
	}
}

func printHeader(cfg RuntimeConfig, source string) {
	fmt.Println(colorCyan + colorBold + "CodingMan" + colorReset)
	fmt.Println(colorDim + "Agent TUI. Type /help for commands, /exit to quit." + colorReset)
	fmt.Printf("%sprovider:%s %s  %smodel:%s %s  %sconfig:%s %s\n\n",
		colorGray, colorReset, cfg.Provider,
		colorGray, colorReset, cfg.ModelName,
		colorGray, colorReset, source,
	)
}

func printHelp() {
	fmt.Println(colorDim + "Slash commands:" + colorReset)
	fmt.Println("  /help                         show this help")
	fmt.Println("  /clear                        clear conversation history")
	fmt.Println("  /cache                        show prompt cache status")
	fmt.Println("  /cache on                     enable prompt cache")
	fmt.Println("  /cache off                    disable prompt cache")
	fmt.Println("  /permission                   show permission mode and policy")
	fmt.Println("  /permission ask               ask before tool calls")
	fmt.Println("  /permission allow-deny        use tool allow/deny policy")
	fmt.Println("  /permission full-auto         allow all tool calls")
	fmt.Println("  /allow <tool>                 allow a tool in this session")
	fmt.Println("  /deny <tool>                  deny a tool in this session")
	fmt.Println("  /permissions                  show permission mode and policy")
	fmt.Println("  /exit                         quit")
	fmt.Println()
}

func handleSlashCommand(a *agent.Agent, prompt string) bool {
	fields := strings.Fields(prompt)
	if len(fields) == 0 {
		return false
	}

	switch fields[0] {
	case "/permission":
		permissions := a.Permission()
		if permissions == nil {
			fmt.Println(colorRed + "permission manager is unavailable" + colorReset)
			return true
		}
		if len(fields) == 1 {
			printPermissionStatus(permissions)
			return true
		}
		mode, err := agent.ParsePermissionMode(fields[1])
		if err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		if err := permissions.SetMode(mode); err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		fmt.Printf("%spermission mode:%s %s\n", colorGray, colorReset, mode)
		return true
	case "/cache":
		return handleCacheCommand(a, fields)
	case "/permissions":
		permissions := a.Permission()
		if permissions == nil {
			fmt.Println(colorRed + "permission manager is unavailable" + colorReset)
			return true
		}
		printPermissionStatus(permissions)
		return true
	case "/allow":
		if len(fields) != 2 {
			fmt.Println(colorRed + "usage: /allow <tool>" + colorReset)
			return true
		}
		if err := a.Permission().AllowTool(fields[1]); err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		fmt.Printf("%sallowed tool:%s %s\n", colorGray, colorReset, fields[1])
		return true
	case "/deny":
		if len(fields) != 2 {
			fmt.Println(colorRed + "usage: /deny <tool>" + colorReset)
			return true
		}
		if err := a.Permission().DenyTool(fields[1]); err != nil {
			fmt.Printf("%serror:%s %v\n", colorRed, colorReset, err)
			return true
		}
		fmt.Printf("%sdenied tool:%s %s\n", colorGray, colorReset, fields[1])
		return true
	default:
		return false
	}
}

func handleCacheCommand(a *agent.Agent, fields []string) bool {
	if len(fields) == 1 {
		printCacheStatus(a.PromptCache())
		return true
	}
	cache := a.PromptCache()
	switch strings.ToLower(fields[1]) {
	case "on", "enable", "enabled":
		cache.Enabled = true
	case "off", "disable", "disabled":
		cache.Enabled = false
	default:
		fmt.Println(colorRed + "usage: /cache [on|off]" + colorReset)
		return true
	}
	a.SetPromptCache(cache)
	printCacheStatus(a.PromptCache())
	return true
}

func printCacheStatus(cache agent.PromptCacheConfig) {
	state := "off"
	if cache.Enabled {
		state = "on"
	}
	key := cache.Key
	if key == "" {
		key = "auto"
	}
	fmt.Printf("%sprompt cache:%s %s  %skey:%s %s  %sretention:%s %s  %sttl:%s %s\n",
		colorGray, colorReset, state,
		colorGray, colorReset, key,
		colorGray, colorReset, cache.Retention,
		colorGray, colorReset, cache.TTL,
	)
}

func printPermissionStatus(permissions *agent.PermissionManager) {
	snapshot := permissions.Snapshot()
	fmt.Printf("%spermission mode:%s %s\n", colorGray, colorReset, snapshot.Mode)
	fmt.Printf("%sallowed tools:%s %s\n", colorGray, colorReset, strings.Join(snapshot.AllowedTools, ", "))
	fmt.Printf("%sdenied tools:%s %s\n", colorGray, colorReset, strings.Join(snapshot.DeniedTools, ", "))
}

func tuiPermissionPrompt(scanner *bufio.Scanner) agent.PermissionAskFunc {
	return func(ctx context.Context, request agent.PermissionRequest) (agent.PermissionDecision, string, error) {
		fmt.Printf("\n%sTool permission request%s\n", colorBold, colorReset)
		fmt.Printf("%stool:%s %s\n", colorGray, colorReset, request.ToolName)
		if request.ToolUseID != "" {
			fmt.Printf("%sid:%s %s\n", colorGray, colorReset, request.ToolUseID)
		}
		fmt.Printf("%sinput:%s\n%s\n", colorGray, colorReset, request.InputJSON())
		fmt.Println(colorDim + "Choose:" + colorReset)
		fmt.Println("  1. Yes, allow once")
		fmt.Println("  2. No, deny once")
		fmt.Println("  3. Always allow this tool")
		fmt.Println("  4. Always deny this tool")
		fmt.Print(colorDim + "Select option [1-4] > " + colorReset)

		type answer struct {
			text string
			ok   bool
			err  error
		}
		result := make(chan answer, 1)
		go func() {
			if !scanner.Scan() {
				result <- answer{ok: false, err: scanner.Err()}
				return
			}
			result <- answer{text: strings.TrimSpace(scanner.Text()), ok: true}
		}()

		select {
		case <-ctx.Done():
			return agent.PermissionDecisionDeny, "", ctx.Err()
		case res := <-result:
			if res.err != nil {
				return agent.PermissionDecisionDeny, "", res.err
			}
			if !res.ok {
				return agent.PermissionDecisionDeny, "input closed", nil
			}
			switch res.text {
			case "1":
				return agent.PermissionDecisionAllow, "", nil
			case "2":
				return agent.PermissionDecisionDeny, "denied by user", nil
			case "3":
				return agent.PermissionDecisionAllowTool, "", nil
			case "4":
				return agent.PermissionDecisionDenyTool, "denied by user", nil
			default:
				return agent.PermissionDecisionDeny, "invalid permission response", nil
			}
		}
	}
}

func loadRuntimeConfig(projectRoot string) (RuntimeConfig, string, error) {
	envPath := filepath.Join(projectRoot, ".env")
	values := map[string]string{}
	source := "environment"

	if _, err := os.Stat(envPath); err == nil {
		loaded, err := readDotEnv(envPath)
		if err != nil {
			return RuntimeConfig{}, "", err
		}
		values = loaded
		source = envPath
	} else if !os.IsNotExist(err) {
		return RuntimeConfig{}, "", err
	} else {
		values = readProcessEnv()
	}

	cfg := RuntimeConfig{
		Provider:  strings.TrimSpace(values["PROVIDER"]),
		ModelName: strings.TrimSpace(values["MODEL_NAME"]),
		APIKey:    strings.TrimSpace(values["API_KEY"]),
		BaseURL:   strings.TrimSpace(values["BASE_URL"]),
		Context:   agent.DefaultContextConfig(),
	}
	cfg.Context.Cwd = valueOrDefault(values["CWD"], projectRoot)
	cfg.Context.BaseSystem = values["BASE_SYSTEM"]
	cfg.Context.IncludeDate = boolValue(values, "INCLUDE_DATE", cfg.Context.IncludeDate)
	cfg.Context.LoadAgentsMD = boolValue(values, "LOAD_AGENTS_MD", cfg.Context.LoadAgentsMD)
	cfg.Context.AutoCompact = boolValue(values, "AUTO_COMPACT", cfg.Context.AutoCompact)
	cfg.Context.CompactThreshold = intValue(values, "COMPACT_THRESHOLD", cfg.Context.CompactThreshold)
	cfg.Context.KeepRecentRounds = intValue(values, "KEEP_RECENT_ROUNDS", cfg.Context.KeepRecentRounds)
	cfg.Context.MaxAgentsMDBytes = intValue(values, "MAX_AGENTS_MD_BYTES", cfg.Context.MaxAgentsMDBytes)

	cfg.MaxLLMTurns = intValue(values, "MAX_LLM_TURNS", 20)
	cfg.MaxToolCalls = intValue(values, "MAX_TOOL_CALLS", 50)
	cfg.MaxToolErrors = intValue(values, "MAX_CONSECUTIVE_TOOL_ERRORS", 3)
	cfg.MaxAPIErrors = intValue(values, "MAX_CONSECUTIVE_API_ERRORS", 3)

	cfg.EnableToolBudget = boolValue(values, "ENABLE_TOOL_BUDGET", true)
	cfg.ToolBudget = agent.ToolBudget{
		MaxLen:  intValue(values, "TOOL_BUDGET_MAX_LEN", 10000),
		HeadLen: intValue(values, "TOOL_BUDGET_HEAD_LEN", 3000),
		TailLen: intValue(values, "TOOL_BUDGET_TAIL_LEN", 3000),
	}

	cfg.Retry = agent.RetryConfig{
		MaxRetries:   intValue(values, "RETRY_MAX_RETRIES", 3),
		InitialDelay: durationValue(values, "RETRY_INITIAL_DELAY", time.Second),
		MaxDelay:     durationValue(values, "RETRY_MAX_DELAY", 60*time.Second),
		Multiplier:   floatValue(values, "RETRY_MULTIPLIER", 2.0),
		Jitter:       floatValue(values, "RETRY_JITTER", 0.2),
	}
	cfg.PromptCache = agent.PromptCacheConfig{
		Enabled:   boolValue(values, "PROMPT_CACHE_ENABLED", true),
		Key:       values["PROMPT_CACHE_KEY"],
		Retention: valueOrDefault(values["PROMPT_CACHE_RETENTION"], agent.PromptCacheRetentionInMemory),
		TTL:       valueOrDefault(values["PROMPT_CACHE_TTL"], agent.PromptCacheTTL5m),
	}

	if err := validateRuntimeConfig(cfg); err != nil {
		return RuntimeConfig{}, "", err
	}
	return cfg, source, nil
}

func validateRuntimeConfig(cfg RuntimeConfig) error {
	missing := make([]string, 0)
	if cfg.Provider == "" {
		missing = append(missing, "PROVIDER")
	}
	if cfg.ModelName == "" {
		missing = append(missing, "MODEL_NAME")
	}
	if cfg.APIKey == "" {
		missing = append(missing, "API_KEY")
	}
	if cfg.BaseURL == "" {
		missing = append(missing, "BASE_URL")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing required config: %s", strings.Join(missing, ", "))
	}
	return nil
}

func readDotEnv(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		value = strings.Trim(value, `"'`)
		if key == "" {
			return nil, fmt.Errorf("%s:%d: empty key", path, lineNumber)
		}
		values[key] = value
	}
	return values, scanner.Err()
}

func readProcessEnv() map[string]string {
	values := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}

func findProjectRoot(start string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", errors.New("go.mod not found")
		}
		current = parent
	}
}

func valueOrDefault(value string, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}

func intValue(values map[string]string, key string, defaultValue int) int {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func floatValue(values map[string]string, key string, defaultValue float64) float64 {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func boolValue(values map[string]string, key string, defaultValue bool) bool {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return defaultValue
	}
	return parsed
}

func durationValue(values map[string]string, key string, defaultValue time.Duration) time.Duration {
	value := strings.TrimSpace(values[key])
	if value == "" {
		return defaultValue
	}
	parsed, err := time.ParseDuration(value)
	if err == nil {
		return parsed
	}
	seconds, err := strconv.Atoi(value)
	if err != nil {
		return defaultValue
	}
	return time.Duration(seconds) * time.Second
}

func fatal(scope string, err error) {
	fmt.Fprintf(os.Stderr, "%s%s:%s %v\n", colorRed, scope, colorReset, err)
	os.Exit(1)
}
