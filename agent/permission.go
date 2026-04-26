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
	PermissionDecisionDenyTool  PermissionDecision = "deny-tool"
)

type PermissionConfig struct {
	Mode         PermissionMode
	AllowedTools []string
	DeniedTools  []string
	Ask          PermissionAskFunc
}

type PermissionAskFunc func(context.Context, PermissionRequest) (PermissionDecision, string, error)

type PermissionRequest struct {
	ToolUseID string
	ToolName  string
	ToolInput map[string]any
}

type PermissionSnapshot struct {
	Mode         PermissionMode
	AllowedTools []string
	DeniedTools  []string
}

type PermissionManager struct {
	mu      sync.RWMutex
	mode    PermissionMode
	allow   map[string]struct{}
	deny    map[string]struct{}
	askFunc PermissionAskFunc
}

func NewPermissionManager(config PermissionConfig) *PermissionManager {
	mode := normalizePermissionMode(config.Mode)
	if mode == "" {
		mode = DefaultPermissionMode
	}
	manager := &PermissionManager{
		mode:    mode,
		allow:   make(map[string]struct{}),
		deny:    make(map[string]struct{}),
		askFunc: config.Ask,
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
	if manager == nil {
		return nil
	}
	if strings.TrimSpace(request.ToolName) == "" {
		return errors.New("permission check requires tool name")
	}
	request.ToolName = normalizeToolName(request.ToolName)

	manager.mu.RLock()
	mode := manager.mode
	_, denied := manager.deny[request.ToolName]
	_, allowed := manager.allow[request.ToolName]
	allowCount := len(manager.allow)
	askFunc := manager.askFunc
	manager.mu.RUnlock()

	switch mode {
	case PermissionModeFullAuto:
		return nil
	case PermissionModeAllowDeny:
		if denied {
			return fmt.Errorf("permission denied: tool %s is denied", request.ToolName)
		}
		if allowed {
			return nil
		}
		if allowCount == 0 {
			return fmt.Errorf("permission denied: tool %s is not allowed", request.ToolName)
		}
		return fmt.Errorf("permission denied: tool %s is not in allow list", request.ToolName)
	case PermissionModeAsk:
		if denied {
			return fmt.Errorf("permission denied: tool %s is denied", request.ToolName)
		}
		if allowed {
			return nil
		}
		if askFunc == nil {
			return fmt.Errorf("permission denied: ask mode has no permission prompt")
		}
		decision, reason, err := askFunc(ctx, request)
		if err != nil {
			return err
		}
		return manager.applyDecision(request.ToolName, decision, reason)
	default:
		return fmt.Errorf("permission denied: invalid permission mode %q", mode)
	}
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
		Mode:         manager.mode,
		AllowedTools: sortedToolNames(manager.allow),
		DeniedTools:  sortedToolNames(manager.deny),
	}
}

func (manager *PermissionManager) applyDecision(toolName string, decision PermissionDecision, reason string) error {
	switch decision {
	case PermissionDecisionAllow:
		return nil
	case PermissionDecisionDeny:
		return permissionDenied(toolName, reason)
	case PermissionDecisionAllowTool:
		if err := manager.AllowTool(toolName); err != nil {
			return err
		}
		return nil
	case PermissionDecisionDenyTool:
		if err := manager.DenyTool(toolName); err != nil {
			return err
		}
		return permissionDenied(toolName, reason)
	default:
		return fmt.Errorf("permission denied: invalid permission decision %q", decision)
	}
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
