package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

type HookEvent string

const (
	HookEventPreToolUse   HookEvent = "PreToolUse"
	HookEventPostToolUse  HookEvent = "PostToolUse"
	HookEventNotification HookEvent = "Notification"
	HookEventStop         HookEvent = "Stop"
	HookEventSubagentStop HookEvent = "SubagentStop"
)

type HookType string

const (
	HookTypeHTTP     HookType = "http"
	HookTypeShell    HookType = "shell"
	HookTypeFunction HookType = "function"
	HookTypeLog      HookType = "log"
)

type HooksConfig struct {
	Hooks []HookConfig `json:"hooks"`
}

type HookConfig struct {
	Name     string    `json:"name"`
	Type     HookType  `json:"type"`
	Event    HookEvent `json:"event"`
	ToolName string    `json:"tool_name"`
	Regex    string    `json:"regex"`
	URL      string    `json:"url"`
	Method   string    `json:"method"`
	Command  string    `json:"command"`
	Function string    `json:"function"`
	Path     string    `json:"path"`
	Parallel *bool     `json:"parallel"`
	Timeout  string    `json:"timeout"`

	regex *regexp.Regexp
}

type HookPayload struct {
	Event      HookEvent      `json:"event"`
	AgentID    string         `json:"agent_id,omitempty"`
	TraceID    string         `json:"trace_id,omitempty"`
	ToolUseID  string         `json:"tool_use_id,omitempty"`
	ToolName   string         `json:"tool_name,omitempty"`
	Input      map[string]any `json:"input,omitempty"`
	Output     string         `json:"output,omitempty"`
	IsError    bool           `json:"is_error,omitempty"`
	Message    string         `json:"message,omitempty"`
	StopReason string         `json:"stop_reason,omitempty"`
	TaskID     string         `json:"task_id,omitempty"`
	SubagentID string         `json:"subagent_id,omitempty"`
}

type HookResult struct {
	UpdatedInput map[string]any `json:"updated_input,omitempty"`
	Message      string         `json:"message,omitempty"`
}

type HookFunc func(context.Context, HookPayload) (HookResult, error)

type HookManager struct {
	mu        sync.RWMutex
	hooks     []HookConfig
	functions map[string]HookFunc
}

func NewHookManager(config HooksConfig) (*HookManager, error) {
	manager := &HookManager{
		hooks:     make([]HookConfig, 0, len(config.Hooks)),
		functions: make(map[string]HookFunc),
	}
	for _, hook := range config.Hooks {
		normalized, err := normalizeHookConfig(hook)
		if err != nil {
			return nil, err
		}
		manager.hooks = append(manager.hooks, normalized)
	}
	return manager, nil
}

func normalizeHookConfig(hook HookConfig) (HookConfig, error) {
	hook.Name = strings.TrimSpace(hook.Name)
	hook.ToolName = strings.TrimSpace(hook.ToolName)
	hook.Regex = strings.TrimSpace(hook.Regex)
	if hook.Event == "" {
		return HookConfig{}, errors.New("hook event is required")
	}
	if hook.Type == "" {
		return HookConfig{}, errors.New("hook type is required")
	}
	if hook.Regex != "" {
		compiled, err := regexp.Compile(hook.Regex)
		if err != nil {
			return HookConfig{}, fmt.Errorf("compile hook regex %q: %w", hook.Regex, err)
		}
		hook.regex = compiled
	}
	return hook, nil
}

func (manager *HookManager) RegisterFunction(name string, fn HookFunc) {
	if manager == nil || fn == nil {
		return
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.functions[strings.TrimSpace(name)] = fn
}

func (manager *HookManager) MatchingCount(payload HookPayload) int {
	if manager == nil {
		return 0
	}
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	count := 0
	for _, hook := range manager.hooks {
		if hookMatches(hook, payload) {
			count++
		}
	}
	return count
}

func (manager *HookManager) Run(ctx context.Context, payload HookPayload) HookResult {
	if manager == nil {
		return HookResult{}
	}
	manager.mu.RLock()
	hooks := make([]HookConfig, 0, len(manager.hooks))
	for _, hook := range manager.hooks {
		if hookMatches(hook, payload) {
			hooks = append(hooks, hook)
		}
	}
	functions := make(map[string]HookFunc, len(manager.functions))
	for name, fn := range manager.functions {
		functions[name] = fn
	}
	manager.mu.RUnlock()

	var result HookResult
	for _, batch := range hookBatches(hooks) {
		if len(batch) == 1 && !hookParallel(batch[0]) {
			result.merge(runHook(ctx, batch[0], payload, functions))
			continue
		}
		var wg sync.WaitGroup
		results := make(chan HookResult, len(batch))
		for _, hook := range batch {
			wg.Add(1)
			go func(h HookConfig) {
				defer wg.Done()
				results <- runHook(ctx, h, payload, functions)
			}(hook)
		}
		wg.Wait()
		close(results)
		for partial := range results {
			result.merge(partial)
		}
	}
	return result
}

func (result *HookResult) merge(other HookResult) {
	if other.UpdatedInput != nil {
		result.UpdatedInput = other.UpdatedInput
	}
	if other.Message != "" {
		if result.Message != "" {
			result.Message += "\n"
		}
		result.Message += other.Message
	}
}

func hookBatches(hooks []HookConfig) [][]HookConfig {
	var batches [][]HookConfig
	var parallel []HookConfig
	flush := func() {
		if len(parallel) > 0 {
			batches = append(batches, parallel)
			parallel = nil
		}
	}
	for _, hook := range hooks {
		if hookParallel(hook) {
			parallel = append(parallel, hook)
			continue
		}
		flush()
		batches = append(batches, []HookConfig{hook})
	}
	flush()
	return batches
}

func hookParallel(hook HookConfig) bool {
	return hook.Parallel == nil || *hook.Parallel
}

func hookMatches(hook HookConfig, payload HookPayload) bool {
	if hook.Event != payload.Event {
		return false
	}
	if hook.ToolName != "" && hook.ToolName != payload.ToolName {
		return false
	}
	if hook.regex != nil {
		data, _ := json.Marshal(payload)
		return hook.regex.Match(data)
	}
	return true
}

func runHook(ctx context.Context, hook HookConfig, payload HookPayload, functions map[string]HookFunc) HookResult {
	timeout := parseHookTimeout(hook.Timeout)
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var result HookResult
	var err error
	switch hook.Type {
	case HookTypeHTTP:
		result, err = runHTTPHook(ctx, hook, payload)
	case HookTypeShell:
		result, err = runShellHook(ctx, hook, payload)
	case HookTypeFunction:
		fn := functions[hook.Function]
		if fn == nil {
			err = fmt.Errorf("hook function not registered: %s", hook.Function)
		} else {
			result, err = fn(ctx, payload)
		}
	case HookTypeLog:
		result, err = runLogHook(hook, payload)
	default:
		err = fmt.Errorf("unsupported hook type: %s", hook.Type)
	}
	if err != nil {
		result.Message = fmt.Sprintf("hook %s error: %v", hook.Name, err)
	}
	return result
}

func runHTTPHook(ctx context.Context, hook HookConfig, payload HookPayload) (HookResult, error) {
	if hook.URL == "" {
		return HookResult{}, errors.New("http hook url is required")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return HookResult{}, err
	}
	method := hook.Method
	if method == "" {
		method = http.MethodPost
	}
	req, err := http.NewRequestWithContext(ctx, method, hook.URL, bytes.NewReader(body))
	if err != nil {
		return HookResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return HookResult{}, err
	}
	defer resp.Body.Close()
	var result HookResult
	_ = json.NewDecoder(resp.Body).Decode(&result)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return result, fmt.Errorf("http hook status=%d", resp.StatusCode)
	}
	return result, nil
}

func runShellHook(ctx context.Context, hook HookConfig, payload HookPayload) (HookResult, error) {
	if hook.Command == "" {
		return HookResult{}, errors.New("shell hook command is required")
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return HookResult{}, err
	}
	cmd := exec.CommandContext(ctx, "sh", "-c", hook.Command)
	cmd.Stdin = bytes.NewReader(data)
	output, err := cmd.CombinedOutput()
	result := parseHookResult(output)
	if err != nil {
		return result, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(output)))
	}
	return result, nil
}

func runLogHook(hook HookConfig, payload HookPayload) (HookResult, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return HookResult{}, err
	}
	line := append(data, '\n')
	if hook.Path == "" {
		_, err = os.Stderr.Write(line)
		return HookResult{}, err
	}
	file, err := os.OpenFile(hook.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return HookResult{}, err
	}
	defer file.Close()
	_, err = file.Write(line)
	return HookResult{}, err
}

func parseHookResult(output []byte) HookResult {
	var result HookResult
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return result
	}
	_ = json.Unmarshal(trimmed, &result)
	return result
}

func parseHookTimeout(value string) time.Duration {
	if strings.TrimSpace(value) == "" {
		return 0
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0
	}
	return duration
}
