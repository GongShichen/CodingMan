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
