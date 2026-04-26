package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
)

type PermissionMode string

const (
	PermissionModeAsk       PermissionMode = "ask"
	PermissionModeAllowDeny PermissionMode = "allow-deny"
	PermissionModeFullAuto  PermissionMode = "full-auto"
	DefaultPermissionMode   PermissionMode = PermissionModeAsk
)

type PermissionDecision string

const (
	PermissionDecisionAllow     PermissionDecision = "allow"
	PermissionDecisionDeny      PermissionDecision = "deny"
	PermissionDecisionAllowTool PermissionDecision = "allow-tool"
	PermissionDecisionAllowRule PermissionDecision = "allow-rule"
	PermissionDecisionDenyTool  PermissionDecision = "deny-tool"
)

type PermissionConfig struct {
	Mode                    PermissionMode
	AllowedTools            []string
	AllowedCommands         []string
	DeniedTools             []string
	AllowedReadOnlyCommands []string
	Ask                     PermissionAskFunc
}

func DefaultPermissionConfig() PermissionConfig {
	return PermissionConfig{
		Mode:                    PermissionModeAsk,
		AllowedTools:            []string{"read", "grep", "glob"},
		AllowedReadOnlyCommands: []string{"pwd", "ls", "find", "rg", "grep", "cat", "sed", "head", "tail", "wc", "sort", "git status", "git diff", "git log", "git show"},
	}
}

func isZeroPermissionConfig(config PermissionConfig) bool {
	return config.Mode == "" &&
		len(config.AllowedTools) == 0 &&
		len(config.AllowedCommands) == 0 &&
		len(config.DeniedTools) == 0 &&
		len(config.AllowedReadOnlyCommands) == 0 &&
		config.Ask == nil
}

type PermissionAskFunc func(context.Context, PermissionRequest) (PermissionDecision, string, error)

type PermissionRequest struct {
	ToolUseID string
	ToolName  string
	ToolInput map[string]any
}

type PermissionSnapshot struct {
	Mode            PermissionMode
	AllowedTools    []string
	AllowedCommands []string
	DeniedTools     []string
}

type PermissionCheck struct {
	ParallelSafe bool
}

type PermissionManager struct {
	mu               sync.RWMutex
	mode             PermissionMode
	allow            map[string]struct{}
	deny             map[string]struct{}
	allowedCommands  []string
	readonlyCommands []string
	askFunc          PermissionAskFunc
}

func NewPermissionManager(config PermissionConfig) *PermissionManager {
	mode := normalizePermissionMode(config.Mode)
	if mode == "" {
		mode = DefaultPermissionMode
	}
	manager := &PermissionManager{
		mode:             mode,
		allow:            make(map[string]struct{}),
		deny:             make(map[string]struct{}),
		allowedCommands:  normalizeCommandPrefixes(config.AllowedCommands),
		readonlyCommands: normalizeCommandPrefixes(config.AllowedReadOnlyCommands),
		askFunc:          config.Ask,
	}
	for _, name := range config.AllowedTools {
		manager.allow[normalizeToolName(name)] = struct{}{}
	}
	for _, name := range config.DeniedTools {
		manager.deny[normalizeToolName(name)] = struct{}{}
	}
	return manager
}

func (manager *PermissionManager) Check(ctx context.Context, request PermissionRequest) error {
	_, err := manager.CheckWithResult(ctx, request)
	return err
}

func (manager *PermissionManager) CheckWithResult(ctx context.Context, request PermissionRequest) (PermissionCheck, error) {
	if manager == nil {
		return PermissionCheck{ParallelSafe: true}, nil
	}
	if strings.TrimSpace(request.ToolName) == "" {
		return PermissionCheck{}, errors.New("permission check requires tool name")
	}
	request.ToolName = normalizeToolName(request.ToolName)

	manager.mu.RLock()
	mode := manager.mode
	denied := manager.isToolDeniedLocked(request.ToolName)
	allowed := manager.isToolAllowedLocked(request.ToolName)
	allowCount := len(manager.allow)
	allowedCommands := append([]string(nil), manager.allowedCommands...)
	readonlyCommands := append([]string(nil), manager.readonlyCommands...)
	askFunc := manager.askFunc
	manager.mu.RUnlock()

	switch mode {
	case PermissionModeFullAuto:
		return PermissionCheck{ParallelSafe: isParallelSafeRequest(request, allowed, allowedCommands, readonlyCommands)}, nil
	case PermissionModeAllowDeny:
		if denied {
			return PermissionCheck{}, fmt.Errorf("permission denied: tool %s is denied", request.ToolName)
		}
		if allowed {
			return PermissionCheck{ParallelSafe: true}, nil
		}
		if request.ToolName == "bash" && isAllowedBashCommand(request.ToolInput, allowedCommands) {
			return PermissionCheck{ParallelSafe: true}, nil
		}
		if allowCount == 0 {
			return PermissionCheck{}, fmt.Errorf("permission denied: tool %s is not allowed", request.ToolName)
		}
		return PermissionCheck{}, fmt.Errorf("permission denied: tool %s is not in allow list", request.ToolName)
	case PermissionModeAsk:
		if denied {
			return PermissionCheck{}, fmt.Errorf("permission denied: tool %s is denied", request.ToolName)
		}
		if allowed {
			return PermissionCheck{ParallelSafe: true}, nil
		}
		if request.ToolName == "bash" && isAllowedBashCommand(request.ToolInput, allowedCommands) {
			return PermissionCheck{ParallelSafe: true}, nil
		}
		if request.ToolName == "bash" && isReadOnlyBashCommand(request.ToolInput, readonlyCommands) {
			return PermissionCheck{ParallelSafe: true}, nil
		}
		if askFunc == nil {
			return PermissionCheck{}, fmt.Errorf("permission denied: ask mode has no permission prompt")
		}
		decision, reason, err := askFunc(ctx, request)
		if err != nil {
			return PermissionCheck{}, err
		}
		return manager.applyDecision(request.ToolName, decision, reason)
	default:
		return PermissionCheck{}, fmt.Errorf("permission denied: invalid permission mode %q", mode)
	}
}

func isParallelSafeRequest(request PermissionRequest, toolAllowed bool, allowedCommands []string, readonlyCommands []string) bool {
	if toolAllowed {
		return true
	}
	if request.ToolName == "bash" {
		return isAllowedBashCommand(request.ToolInput, allowedCommands) ||
			isReadOnlyBashCommand(request.ToolInput, readonlyCommands)
	}
	return request.ToolName == "read" || request.ToolName == "grep" || request.ToolName == "glob"
}

func (manager *PermissionManager) isToolAllowedLocked(name string) bool {
	if _, allowed := manager.allow["*"]; allowed {
		return true
	}
	_, allowed := manager.allow[name]
	return allowed
}

func (manager *PermissionManager) isToolDeniedLocked(name string) bool {
	if _, denied := manager.deny["*"]; denied {
		return true
	}
	_, denied := manager.deny[name]
	return denied
}

func isAllowedBashCommand(input map[string]any, allowedCommands []string) bool {
	command, _ := input["command"].(string)
	command = normalizeShellCommand(command)
	if command == "" {
		return false
	}
	for _, pattern := range allowedCommands {
		if matchPermissionPattern(pattern, command) {
			return true
		}
	}
	return false
}

func isReadOnlyBashCommand(input map[string]any, allowedPrefixes []string) bool {
	command, _ := input["command"].(string)
	command = normalizeShellCommand(command)
	if command == "" {
		return false
	}
	if hasShellWriteOperation(command) {
		return false
	}
	for _, prefix := range allowedPrefixes {
		if command == prefix || strings.HasPrefix(command, prefix+" ") {
			return true
		}
	}
	return false
}

func normalizeCommandPrefixes(commands []string) []string {
	result := make([]string, 0, len(commands))
	seen := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		command = normalizeShellCommand(command)
		if command == "" {
			continue
		}
		if _, exists := seen[command]; exists {
			continue
		}
		seen[command] = struct{}{}
		result = append(result, command)
	}
	sort.Strings(result)
	return result
}

func normalizeShellCommand(command string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(command)), " ")
}

func hasShellWriteOperation(command string) bool {
	writeMarkers := []string{
		">",
		">>",
		" rm ",
		" rm -",
		"mv ",
		"cp ",
		"mkdir ",
		"rmdir ",
		"touch ",
		"chmod ",
		"chown ",
		"ln ",
		"tee ",
		"truncate ",
		"dd ",
		"install ",
		"sed -i",
		"perl -pi",
		"git add",
		"git commit",
		"git checkout",
		"git reset",
		"git clean",
		"git rm",
		"git mv",
		"npm install",
		"go get",
		"go mod tidy",
	}
	padded := " " + command + " "
	for _, marker := range writeMarkers {
		if strings.Contains(padded, marker) || strings.HasPrefix(command, marker) {
			return true
		}
	}
	return false
}

func (manager *PermissionManager) Mode() PermissionMode {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.mode
}

func (manager *PermissionManager) SetMode(mode PermissionMode) error {
	mode = normalizePermissionMode(mode)
	if mode == "" {
		return errors.New("permission mode must be ask, allow-deny, or full-auto")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.mode = mode
	return nil
}

func (manager *PermissionManager) SetAskFunc(askFunc PermissionAskFunc) {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.askFunc = askFunc
}

func (manager *PermissionManager) AllowTool(name string) error {
	name = normalizeToolName(name)
	if name == "" {
		return errors.New("tool name is required")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.allow[name] = struct{}{}
	delete(manager.deny, name)
	return nil
}

func (manager *PermissionManager) AllowCommand(command string) error {
	command = normalizeShellCommand(command)
	if command == "" {
		return errors.New("command is required")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	for _, existing := range manager.allowedCommands {
		if existing == command {
			return nil
		}
	}
	manager.allowedCommands = append(manager.allowedCommands, command)
	sort.Strings(manager.allowedCommands)
	return nil
}

func (manager *PermissionManager) DenyTool(name string) error {
	name = normalizeToolName(name)
	if name == "" {
		return errors.New("tool name is required")
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.deny[name] = struct{}{}
	delete(manager.allow, name)
	return nil
}

func (manager *PermissionManager) Snapshot() PermissionSnapshot {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return PermissionSnapshot{
		Mode:            manager.mode,
		AllowedTools:    sortedToolNames(manager.allow),
		AllowedCommands: append([]string(nil), manager.allowedCommands...),
		DeniedTools:     sortedToolNames(manager.deny),
	}
}

func (manager *PermissionManager) applyDecision(toolName string, decision PermissionDecision, reason string) (PermissionCheck, error) {
	switch decision {
	case PermissionDecisionAllow:
		return PermissionCheck{ParallelSafe: false}, nil
	case PermissionDecisionDeny:
		return PermissionCheck{}, permissionDenied(toolName, reason)
	case PermissionDecisionAllowTool:
		if err := manager.AllowTool(toolName); err != nil {
			return PermissionCheck{}, err
		}
		return PermissionCheck{ParallelSafe: true}, nil
	case PermissionDecisionAllowRule:
		if err := manager.allowRequestRule(toolName, reason); err != nil {
			return PermissionCheck{}, err
		}
		return PermissionCheck{ParallelSafe: true}, nil
	case PermissionDecisionDenyTool:
		if err := manager.DenyTool(toolName); err != nil {
			return PermissionCheck{}, err
		}
		return PermissionCheck{}, permissionDenied(toolName, reason)
	default:
		return PermissionCheck{}, fmt.Errorf("permission denied: invalid permission decision %q", decision)
	}
}

func (manager *PermissionManager) allowRequestRule(toolName string, command string) error {
	if toolName == "bash" {
		if err := manager.AllowCommand(command); err != nil {
			return err
		}
		return nil
	}
	return manager.AllowTool(toolName)
}

func (request PermissionRequest) AllowRuleValue() string {
	if request.ToolName == "bash" {
		command, _ := request.ToolInput["command"].(string)
		return normalizeShellCommand(command)
	}
	return request.ToolName
}

func (request PermissionRequest) InputJSON() string {
	data, err := json.MarshalIndent(request.ToolInput, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", request.ToolInput)
	}
	return string(data)
}

func ParsePermissionMode(value string) (PermissionMode, error) {
	mode := normalizePermissionMode(PermissionMode(value))
	if mode == "" {
		return "", fmt.Errorf("invalid permission mode %q", value)
	}
	return mode, nil
}

func normalizePermissionMode(mode PermissionMode) PermissionMode {
	switch PermissionMode(strings.ToLower(strings.TrimSpace(string(mode)))) {
	case "", PermissionModeAsk:
		return PermissionModeAsk
	case PermissionModeAllowDeny:
		return PermissionModeAllowDeny
	case PermissionModeFullAuto:
		return PermissionModeFullAuto
	default:
		return ""
	}
}

func normalizeToolName(name string) string {
	return strings.TrimSpace(name)
}

func permissionDenied(toolName string, reason string) error {
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("permission denied: tool %s was denied", toolName)
	}
	return fmt.Errorf("permission denied: tool %s was denied: %s", toolName, strings.TrimSpace(reason))
}

func matchPermissionPattern(pattern string, value string) bool {
	pattern = normalizeShellCommand(pattern)
	value = normalizeShellCommand(value)
	if pattern == "*" {
		return true
	}
	if strings.HasSuffix(pattern, "*") {
		return strings.HasPrefix(value, strings.TrimSpace(strings.TrimSuffix(pattern, "*")))
	}
	return pattern == value
}

func sortedToolNames(values map[string]struct{}) []string {
	names := make([]string, 0, len(values))
	for name := range values {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}
