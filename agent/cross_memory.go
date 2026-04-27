package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	crossMemoryMainFile            = "MEMORY.md"
	crossMemoryUserPrefsFile       = "user_prefs.md"
	crossMemoryProjectStackFile    = "project_stack.md"
	crossMemoryFeedbackTestingFile = "feedback_testing.md"
	crossMemoryReferencesFile      = "references.md"
)

var crossMemoryFiles = map[string]string{
	"user":      crossMemoryUserPrefsFile,
	"feedback":  crossMemoryFeedbackTestingFile,
	"project":   crossMemoryProjectStackFile,
	"reference": crossMemoryReferencesFile,
}

type CrossSessionMemoryStore struct {
	projectDir string
	dir        string
}

type CrossMemoryItem struct {
	Filename    string `json:"filename"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Type        string `json:"type"`
	Content     string `json:"content"`
}

func NewCrossSessionMemoryStore(projectDir string) (*CrossSessionMemoryStore, error) {
	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(home, ".codingman", "projects", ProjectHash(absProjectDir), "memory")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	store := &CrossSessionMemoryStore{projectDir: absProjectDir, dir: dir}
	return store, store.ensureFiles()
}

func (store *CrossSessionMemoryStore) Dir() string {
	if store == nil {
		return ""
	}
	return store.dir
}

func (store *CrossSessionMemoryStore) Render(maxChars int) string {
	if store == nil {
		return ""
	}
	files := []string{
		crossMemoryMainFile,
		crossMemoryUserPrefsFile,
		crossMemoryProjectStackFile,
		crossMemoryFeedbackTestingFile,
		crossMemoryReferencesFile,
	}
	var builder strings.Builder
	remaining := maxChars
	if remaining <= 0 {
		remaining = defaultMaxCrossMemoryChars
	}
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(store.dir, name))
		if err != nil {
			continue
		}
		body := strings.TrimSpace(string(data))
		if body == "" {
			continue
		}
		part := fmt.Sprintf("### %s\n%s\n", name, body)
		runes := []rune(part)
		if len(runes) > remaining {
			builder.WriteString(string(runes[:remaining]))
			builder.WriteString("\n[cross-session memory truncated]\n")
			break
		}
		builder.WriteString(part)
		remaining -= len(runes)
		if remaining <= 0 {
			break
		}
	}
	return strings.TrimSpace(builder.String())
}

func (store *CrossSessionMemoryStore) Update(ctx context.Context, llm LLM, model string, promptCache PromptCacheConfig, sessionSummary string, maxChars int) error {
	if store == nil {
		return errors.New("cross-session memory store is nil")
	}
	if llm == nil || strings.TrimSpace(sessionSummary) == "" {
		return nil
	}
	existing := store.Render(maxChars)
	prompt := buildCrossMemoryPrompt(existing, sessionSummary)
	resp, err := llm.Chat(ctx, []Message{{
		Role: "user",
		Content: []ContentBlock{
			TextBlock(prompt),
		},
	}}, ChatOptions{
		Model:       model,
		PromptCache: promptCache,
	})
	if err != nil {
		return err
	}
	items, err := parseCrossMemoryItems(resp.Content)
	if err != nil {
		return err
	}
	return store.AppendItems(items)
}

func (store *CrossSessionMemoryStore) AppendItems(items []CrossMemoryItem) error {
	if store == nil {
		return errors.New("cross-session memory store is nil")
	}
	if err := store.ensureFiles(); err != nil {
		return err
	}
	for _, item := range items {
		item = normalizeCrossMemoryItem(item)
		if item.Content == "" {
			continue
		}
		path := filepath.Join(store.dir, item.Filename)
		existingBytes, _ := os.ReadFile(path)
		existing := string(existingBytes)
		if crossMemoryDuplicate(existing, item) {
			continue
		}
		entry := formatCrossMemoryEntry(item)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		if _, err := file.WriteString(entry); err != nil {
			_ = file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

func (store *CrossSessionMemoryStore) ensureFiles() error {
	if store == nil {
		return errors.New("cross-session memory store is nil")
	}
	templates := map[string]string{
		crossMemoryMainFile: `# 项目记忆索引

## 用户偏好

- [User Preferences](user_prefs.md) - User information and coding preferences

## 用户反馈

- [Feedback And Testing](feedback_testing.md) - User feedback, corrections, and testing expectations

## 项目事实

- [Tech Stack](project_stack.md) - Project facts, architecture, stack, and durable decisions

## 外部引用

- [References](references.md) - External references explicitly provided by the user

`,
		crossMemoryUserPrefsFile:       "# User Preferences\n\n",
		crossMemoryProjectStackFile:    "# Project Stack\n\n",
		crossMemoryFeedbackTestingFile: "# Feedback And Testing\n\n",
		crossMemoryReferencesFile:      "# References\n\n",
	}
	for name, content := range templates {
		path := filepath.Join(store.dir, name)
		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			return err
		}
	}
	return nil
}

func buildCrossMemoryPrompt(existing string, sessionSummary string) string {
	return `你是 CodingMan 的跨会话记忆提取器。请从本轮会话记忆中提取适合长期保留、对未来 coding agent 工作有帮助的信息。

四种记忆类型：
1. user：用户信息和偏好。范围包括用户明确表达的交互偏好、代码风格偏好、工作流偏好。示例：“用户希望 yes/no 用选择项展示，不要手动输入”。
2. feedback：用户反馈和纠正。范围包括用户指出的错误、修正方向、对验证方式的要求。示例：“用户要求修改后完整测试整个工程”。
3. project：项目事实。范围包括用户确认过的项目架构、启动方式、长期设计约束。示例：“main 是 TUI 启动入口”，“主 Agent ID 默认为 main”。
4. reference：外部引用。范围包括用户明确给出的外部规范、文档路径、链接或可复用参考材料。示例：“某设计应参考 Claude Code 风格”。

不应保存：
- 从仓库能直接推断的普通信息、临时命令输出、一次性任务状态、短期计划。
- API key、token、账号、私密 URL、完整图片内容、敏感个人信息。
- 已有记忆中语义重复的信息。

去重要求：
- 下面会传入已有记忆清单。请检查已有内容，避免输出重复或只是换句话说的条目。
- 如果没有值得新增的长期记忆，输出 []。

输出格式：
- 只输出 JSON 数组，不要 markdown，不要解释。
- 每个元素必须包含 filename、name、description、type、content。
- filename 只能是 user_prefs.md、project_stack.md、feedback_testing.md、references.md。
- type 只能是 user、feedback、project、reference。

已有记忆清单：
` + existing + `

本轮会话记忆：
` + sessionSummary
}

func parseCrossMemoryItems(text string) ([]CrossMemoryItem, error) {
	text = extractJSONArray(strings.TrimSpace(text))
	var items []CrossMemoryItem
	if err := json.Unmarshal([]byte(text), &items); err != nil {
		return nil, err
	}
	result := make([]CrossMemoryItem, 0, len(items))
	for _, item := range items {
		item = normalizeCrossMemoryItem(item)
		if item.Content != "" {
			result = append(result, item)
		}
	}
	return result, nil
}

func extractJSONArray(text string) string {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "```json")
	text = strings.TrimPrefix(text, "```")
	text = strings.TrimSuffix(text, "```")
	text = strings.TrimSpace(text)
	start := strings.Index(text, "[")
	end := strings.LastIndex(text, "]")
	if start >= 0 && end >= start {
		return strings.TrimSpace(text[start : end+1])
	}
	return text
}

func normalizeCrossMemoryItem(item CrossMemoryItem) CrossMemoryItem {
	item.Type = strings.TrimSpace(item.Type)
	item.Filename = strings.TrimSpace(item.Filename)
	item.Name = strings.TrimSpace(item.Name)
	item.Description = strings.TrimSpace(item.Description)
	item.Content = strings.TrimSpace(item.Content)
	item.Type = normalizeCrossMemoryType(item.Type)
	if filename, ok := crossMemoryFiles[item.Type]; ok {
		item.Filename = filename
	}
	if !validCrossMemoryFilename(item.Filename) {
		item.Filename = crossMemoryUserPrefsFile
	}
	if _, ok := crossMemoryFiles[item.Type]; !ok {
		item.Type = typeForCrossMemoryFile(item.Filename)
	}
	if item.Name == "" {
		item.Name = item.Type
	}
	return item
}

func normalizeCrossMemoryType(memoryType string) string {
	switch memoryType {
	case "user", "feedback", "project", "reference":
		return memoryType
	case "user_prefs":
		return "user"
	case "feedback_testing":
		return "feedback"
	case "project_stack", "memory":
		return "project"
	default:
		return memoryType
	}
}

func validCrossMemoryFilename(filename string) bool {
	for _, value := range crossMemoryFiles {
		if filename == value {
			return true
		}
	}
	return false
}

func typeForCrossMemoryFile(filename string) string {
	for memoryType, name := range crossMemoryFiles {
		if filename == name {
			return memoryType
		}
	}
	return "user"
}

func crossMemoryDuplicate(existing string, item CrossMemoryItem) bool {
	content := strings.TrimSpace(item.Content)
	name := strings.TrimSpace(item.Name)
	if content != "" && strings.Contains(existing, content) {
		return true
	}
	return name != "" && strings.Contains(existing, "## "+name)
}

func formatCrossMemoryEntry(item CrossMemoryItem) string {
	var builder strings.Builder
	builder.WriteString("\n## ")
	builder.WriteString(item.Name)
	builder.WriteString("\n\n")
	if item.Description != "" {
		builder.WriteString("> ")
		builder.WriteString(item.Description)
		builder.WriteString("\n\n")
	}
	builder.WriteString(item.Content)
	builder.WriteString("\n\n")
	builder.WriteString(fmt.Sprintf("_type: %s, updated: %s_\n", item.Type, time.Now().UTC().Format(time.RFC3339)))
	return builder.String()
}
