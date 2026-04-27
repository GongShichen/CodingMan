package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultAgentsMDMaxBytes = 64 * 1024

const DefaultCodingSystemPrompt = `You are CodingMan, an autonomous coding agent running inside the user's local repository.

Your job is to help the user modify, debug, test, and explain code. Work like a careful senior software engineer:
- Understand the existing codebase before changing it.
- Prefer small, focused edits that match local style and architecture.
- Use tools to inspect files, run tests, build the project, and verify behavior.
- Never overwrite user work unless the user explicitly asks for it.
- Preserve command output and error details so failures can be diagnosed.
- When a task requires code changes, implement the change and verify it when practical.
- When a request is ambiguous, make a reasonable low-risk assumption and state it briefly.
- Keep responses concise and concrete. Mention changed files, verification steps, and any remaining risks.

Self-reflection workflow:
- Reflect briefly before acting: identify the user's goal, relevant files, risks, and verification path.
- Use the smallest plan that can complete the task.
- After tool results, reassess whether the plan is still valid before continuing.
- Before the final answer, check that the newest user request is addressed and that verification results are accurate.
- Keep private reasoning out of the final answer; report only decisions, edits, results, and blockers.

Sub-agent and A2A workflow:
- Use the subagent tool only for concrete side tasks that can run independently from your immediate next step.
- Give sub-agents narrow tasks, expected output, and relevant constraints.
- Treat sub-agent results as A2A messages: integrate them, verify important claims when needed, and keep ownership clear.
- Do not delegate work that blocks your very next action; do that work directly.
- In coordinator workflows, async workers report <task-notification> XML. Use await before depending on their result and taskstop to cancel obsolete workers.

Tool and filesystem rules:
- Read before editing.
- Avoid destructive operations unless the user explicitly requests them.
- For write, edit, delete, or shell actions that can change state, follow the active permission policy.
- Treat AGENTS.md and repository-local instructions as project policy.

You are not a general chatbot in this session. Stay focused on the coding task and the repository.`

type ContextConfig struct {
	Cwd              string
	BaseSystem       string
	IncludeDate      bool
	LoadAgentsMD     bool
	AutoCompact      bool
	CompactThreshold int
	KeepRecentRounds int
	MaxAgentsMDBytes int
}

func DefaultContextConfig() ContextConfig {
	return ContextConfig{
		Cwd:              ".",
		BaseSystem:       DefaultCodingSystemPrompt + "\n\n" + CoordinatorSystemPrompt,
		IncludeDate:      true,
		LoadAgentsMD:     true,
		AutoCompact:      true,
		CompactThreshold: defaultAutoCompactSizeThreshold,
		KeepRecentRounds: defaultAutoCompactRecentRounds,
		MaxAgentsMDBytes: defaultAgentsMDMaxBytes,
	}
}

func normalizeContextConfig(config ContextConfig) ContextConfig {
	defaults := DefaultContextConfig()
	if config.Cwd == "" {
		config.Cwd = defaults.Cwd
	}
	if config.BaseSystem == "" {
		config.BaseSystem = defaults.BaseSystem
	}
	if config.CompactThreshold <= 0 {
		config.CompactThreshold = defaults.CompactThreshold
	}
	if config.KeepRecentRounds <= 0 {
		config.KeepRecentRounds = defaults.KeepRecentRounds
	}
	if config.MaxAgentsMDBytes <= 0 {
		config.MaxAgentsMDBytes = defaults.MaxAgentsMDBytes
	}
	return config
}

func FindAgentsMD(startDir string) (string, error) {
	/*
		从指定目录向上递归查找AGENTS.md文件，
		查找顺序： startDir -> parent ->grandparent -> ... -> root
	*/
	if startDir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		startDir = cwd
	}

	current, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(current)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		current = filepath.Dir(current)
	}

	for {
		candidate := filepath.Join(current, "AGENTS.md")
		info, err := os.Stat(candidate)
		if err == nil {
			if info.IsDir() {
				return "", fmt.Errorf("%s is a directory", candidate)
			}
			return candidate, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}

		parent := filepath.Dir(current)
		if parent == current {
			return "", fmt.Errorf("%w: AGENTS.md", os.ErrNotExist)
		}
		current = parent
	}
}

func LoadAgentsMD(startDir string) (string, error) {
	filePath, err := FindAgentsMD(startDir)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	return string(content), nil
}

func BuildSystemPromptWithConfig(config ContextConfig) (string, error) {
	/*
		构建完整的提示系统。
		包含：
			1. 基础系统提示词
			2. AGENTS.md 内容
			3. 当前日期和时间
			4. 工作目录
	*/
	config = normalizeContextConfig(config)
	var parts []string
	if config.BaseSystem != "" {
		parts = append(parts, fmt.Sprintf("%s\n", config.BaseSystem))
	}

	if config.LoadAgentsMD {
		agentsMD, _ := loadAgentsMDWithLimit(config.Cwd, config.MaxAgentsMDBytes)
		if agentsMD != "" {
			parts = append(parts, "## 项目规范 (AGENTS.md)\n")
			parts = append(parts, agentsMD)
		}
	}

	var contextParts []string
	if config.IncludeDate {
		now := time.Now()
		s := now.Format("2006-01-02 15:04:05")
		contextParts = append(contextParts, fmt.Sprintf("当前日期时间： %s", s))
	}
	absCwd, _ := filepath.Abs(config.Cwd)
	if absCwd != "" {
		contextParts = append(contextParts, fmt.Sprintf("工作目录: %s", absCwd))
	}

	if contextParts != nil {
		parts = append(parts, "## 上下文信息\n")
		parts = append(parts, contextParts...)
	}
	if parts == nil {
		return "", errors.New("Cannot build system prompt")
	}

	return strings.Join(parts, "\n"), nil
}

func loadAgentsMDWithLimit(startDir string, maxBytes int) (string, error) {
	filePath, err := FindAgentsMD(startDir)
	if err != nil {
		return "", err
	}

	content, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	if maxBytes > 0 && len(content) > maxBytes {
		content = content[:maxBytes]
		return string(content) + "\n\n[AGENTS.md truncated]\n", nil
	}

	return string(content), nil
}
