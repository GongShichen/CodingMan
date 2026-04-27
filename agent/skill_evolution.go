package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

type SkillEvolutionItem struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Content     string `json:"content"`
}

func (agent *Agent) MaybeEvolveSkills(ctxs ...context.Context) bool {
	agent.mu.Lock()
	if agent.skillEvolutionThreshold <= 0 || agent.toolCallsSinceSkillReview < agent.skillEvolutionThreshold || agent.skillEvolutionRunning {
		agent.mu.Unlock()
		return false
	}
	jobs := agent.skillEvolutionJobs
	agent.skillEvolutionRunning = true
	agent.mu.Unlock()

	ctx := context.Background()
	if len(ctxs) > 0 && ctxs[0] != nil {
		ctx = ctxs[0]
	}
	_, traceID := ensureTrace(ctx)
	if jobs == nil {
		agent.finishSkillEvolution(false, traceID, "skill_evolution schedule_skipped reason=jobs_nil")
		return false
	}
	select {
	case jobs <- traceID:
		agent.log(traceID, "skill_evolution scheduled")
		return true
	default:
		agent.finishSkillEvolution(false, traceID, "skill_evolution schedule_skipped reason=busy")
		return false
	}
}

func (agent *Agent) startSkillEvolutionWorker() {
	agent.mu.Lock()
	jobs := agent.skillEvolutionJobs
	agent.mu.Unlock()
	if jobs == nil {
		return
	}
	go func() {
		for traceID := range jobs {
			ctx := WithTraceID(context.Background(), traceID)
			if err := agent.runSkillEvolution(ctx); err != nil {
				agent.finishSkillEvolution(false, traceID, "skill_evolution worker_error=%v", err)
				continue
			}
			agent.finishSkillEvolution(true, traceID, "skill_evolution completed")
		}
	}()
}

func (agent *Agent) finishSkillEvolution(success bool, traceID string, format string, args ...any) {
	if format != "" {
		agent.log(traceID, format, args...)
	}
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if success {
		agent.toolCallsSinceSkillReview = 0
		agent.skills = loadedSkillsForConfig(agent.contextConfig)
		agent.refreshActiveSkillLocked()
	}
	agent.skillEvolutionRunning = false
}

func (agent *Agent) runSkillEvolution(ctx context.Context) error {
	traceID := TraceIDFromContext(ctx)
	agent.mu.Lock()
	toolCallCount := agent.toolCallsSinceSkillReview
	messages := agent.snapshotMessagesLocked()
	fileHistory := append([]FileHistoryEntry(nil), agent.fileHistory...)
	attribution := append([]AttributionEntry(nil), agent.attribution...)
	todos := append([]TodoItem(nil), agent.todos...)
	llm := agent.llm
	model := agent.model
	promptCache := agent.promptCache
	contextConfig := agent.contextConfig
	registry := agent.registry
	agent.mu.Unlock()

	if llm == nil {
		return errors.New("llm is nil")
	}
	if registry == nil {
		return errors.New("tool registry is nil")
	}

	agent.log(traceID, "skill_evolution review start tool_calls=%d messages=%d", toolCallCount, len(messages))
	items, err := agent.reviewSkillsForEvolution(ctx, llm, model, promptCache, contextConfig, messages, fileHistory, attribution, todos)
	if err != nil {
		return fmt.Errorf("review: %w", err)
	}
	if err := agent.saveSkillEvolutionItems(ctx, items); err != nil {
		return fmt.Errorf("save: %w", err)
	}
	agent.log(traceID, "skill_evolution review completed items=%d", len(items))
	return nil
}

func (agent *Agent) reviewSkillsForEvolution(ctx context.Context, llm LLM, model string, promptCache PromptCacheConfig, config ContextConfig, messages []Message, fileHistory []FileHistoryEntry, attribution []AttributionEntry, todos []TodoItem) ([]SkillEvolutionItem, error) {
	prompt := buildSkillEvolutionPrompt(config, messages, fileHistory, attribution, todos)
	resp, err := llm.Chat(ctx, []Message{{Role: "user", Content: []ContentBlock{TextBlock(prompt)}}}, ChatOptions{
		Model:       model,
		PromptCache: promptCache,
	})
	if err != nil {
		return nil, err
	}
	return parseSkillEvolutionItems(resp.Content)
}

func buildSkillEvolutionPrompt(config ContextConfig, messages []Message, fileHistory []FileHistoryEntry, attribution []AttributionEntry, todos []TodoItem) string {
	var builder strings.Builder
	builder.WriteString("你是 CodingMan 的经验审查器。请审查最近一段复杂 coding agent 对话，把通过试错得到、未来可复用的工程经验沉淀为用户级 SKILL 文档。\n\n")
	builder.WriteString("审查视角：\n")
	builder.WriteString("- 保存可复用的工作流、调试路径、项目类型通用约束、工具使用顺序、验证方法、失败模式和修复策略。\n")
	builder.WriteString("- 保存需要多步探索才发现的经验，尤其是对以后 coding agent 有操作价值的内容。\n")
	builder.WriteString("- 不保存一次性的临时状态、当前任务进度、可从代码直接推断的信息、敏感信息、密钥、私人路径细节、普通聊天内容。\n")
	builder.WriteString("- 不要重复已有 SKILL；如果已有 SKILL 主题相同，输出 update，并给出合并后的完整 SKILL.md 内容。\n")
	builder.WriteString("- 如果没有值得沉淀的经验，只输出 []。\n\n")
	builder.WriteString("输出必须是严格 JSON 数组，不要 Markdown，不要解释。格式：\n")
	builder.WriteString(`[{"action":"create或者update","name":"<skill-name>","description":"一句话描述","content":"完整的内容"}]`)
	builder.WriteString("\ncontent 必须是完整 SKILL.md，包含 frontmatter：---、name、description、allow_tools、context，然后是可执行的操作指南。\n\n")
	builder.WriteString("已有用户级和项目级 SKILL 索引：\n")
	skillResult := LoadSkillsWithWarnings(config)
	if len(skillResult.Skills) == 0 {
		builder.WriteString("- none\n")
	} else {
		for _, skill := range skillResult.Skills {
			builder.WriteString(fmt.Sprintf("- %s: %s (%s)\n", skill.Name, skill.Description, skill.Path))
		}
	}
	builder.WriteString("\n最近对话：\n")
	builder.WriteString(FormatMessages(messages))
	builder.WriteString("\n\n文件历史：\n")
	for _, item := range fileHistory {
		builder.WriteString(fmt.Sprintf("- %s %s by %s\n", item.Action, item.Path, item.AgentID))
	}
	builder.WriteString("\nAttribution：\n")
	for _, item := range attribution {
		builder.WriteString(fmt.Sprintf("- %s by %s %s\n", item.Path, item.AgentID, item.Note))
	}
	builder.WriteString("\nTodos：\n")
	for _, item := range todos {
		builder.WriteString(fmt.Sprintf("- [%s] %s\n", item.Status, item.Content))
	}
	return builder.String()
}

func parseSkillEvolutionItems(content string) ([]SkillEvolutionItem, error) {
	content = strings.TrimSpace(extractJSONArray(content))
	if content == "" {
		return nil, errors.New("empty skill evolution response")
	}
	var items []SkillEvolutionItem
	if err := json.Unmarshal([]byte(content), &items); err != nil {
		return nil, err
	}
	cleaned := make([]SkillEvolutionItem, 0, len(items))
	for _, item := range items {
		item.Action = strings.ToLower(strings.TrimSpace(item.Action))
		item.Name = strings.TrimSpace(item.Name)
		item.Description = strings.TrimSpace(item.Description)
		item.Content = strings.TrimSpace(item.Content)
		if item.Action != "create" && item.Action != "update" {
			continue
		}
		if item.Name == "" || item.Content == "" {
			continue
		}
		cleaned = append(cleaned, item)
	}
	return cleaned, nil
}

func stripJSONFence(content string) string {
	content = strings.TrimSpace(content)
	if !strings.HasPrefix(content, "```") {
		return content
	}
	lines := strings.Split(content, "\n")
	if len(lines) >= 3 && strings.HasPrefix(strings.TrimSpace(lines[0]), "```") && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		return strings.Join(lines[1:len(lines)-1], "\n")
	}
	return content
}

func (agent *Agent) saveSkillEvolutionItems(ctx context.Context, items []SkillEvolutionItem) error {
	if len(items) == 0 {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	base := filepath.Join(home, ".codingman", "skills")
	for _, item := range items {
		name := sanitizeSkillDirName(item.Name)
		if name == "" {
			continue
		}
		path := filepath.Join(base, name, "SKILL.md")
		if err := ensureUserSkillPathSafe(base, path); err != nil {
			return fmt.Errorf("%s: %w", item.Name, err)
		}
		content := ensureSkillDocument(item, name)
		if err := agent.saveSkillDocumentWithTools(ctx, path, content, item.Action); err != nil {
			return fmt.Errorf("%s: %w", item.Name, err)
		}
	}
	return nil
}

func (agent *Agent) saveSkillDocumentWithTools(ctx context.Context, path string, content string, action string) error {
	traceID := TraceIDFromContext(ctx)
	agent.mu.Lock()
	registry := agent.registry
	agent.mu.Unlock()
	if registry == nil {
		return errors.New("tool registry is nil")
	}

	if action == "create" {
		agent.log(traceID, "skill_evolution write action=create path=%s", path)
		_, err := registry.CallContext(ctx, "write", map[string]any{
			"filePath":  path,
			"content":   content,
			"overwrite": false,
		})
		if err == nil {
			return nil
		}
		if !strings.Contains(err.Error(), "already exists") {
			return err
		}
	}

	oldContent, err := os.ReadFile(path)
	if err == nil {
		agent.log(traceID, "skill_evolution write action=update path=%s", path)
		_, editErr := registry.CallContext(ctx, "edit", map[string]any{
			"filePath": path,
			"oldText":  string(oldContent),
			"newText":  content,
		})
		return editErr
	}
	if !os.IsNotExist(err) {
		return err
	}
	agent.log(traceID, "skill_evolution write action=create_missing path=%s", path)
	_, err = registry.CallContext(ctx, "write", map[string]any{
		"filePath":  path,
		"content":   content,
		"overwrite": false,
	})
	return err
}

func ensureUserSkillPathSafe(base string, path string) error {
	absBase := cleanAbs(base)
	absPath := cleanAbs(path)
	if !isWithinDir(absBase, absPath) {
		return fmt.Errorf("skill path escapes user skill directory: %s", path)
	}
	dir := filepath.Dir(absPath)
	if resolvedDir, err := filepath.EvalSymlinks(dir); err == nil {
		resolvedBase := absBase
		if realBase, baseErr := filepath.EvalSymlinks(absBase); baseErr == nil {
			resolvedBase = cleanAbs(realBase)
		}
		if !isWithinDir(resolvedBase, cleanAbs(resolvedDir)) {
			return fmt.Errorf("skill path resolves outside user skill directory: %s", path)
		}
	}
	return nil
}

func ensureSkillDocument(item SkillEvolutionItem, safeName string) string {
	content := strings.TrimSpace(item.Content)
	if strings.HasPrefix(content, "---") {
		return content + "\n"
	}
	description := strings.TrimSpace(item.Description)
	if description == "" {
		description = "Reusable coding agent workflow."
	}
	var builder strings.Builder
	builder.WriteString("---\n")
	builder.WriteString("name: ")
	builder.WriteString(safeName)
	builder.WriteString("\n")
	builder.WriteString("description: ")
	builder.WriteString(description)
	builder.WriteString("\n")
	builder.WriteString("allow_tools: []\n")
	builder.WriteString("context: fork\n")
	builder.WriteString("---\n\n")
	builder.WriteString(content)
	builder.WriteString("\n")
	return builder.String()
}

var invalidSkillNameChars = regexp.MustCompile(`[^a-z0-9._-]+`)

func sanitizeSkillDirName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var builder strings.Builder
	lastDash := false
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' {
			builder.WriteRune(r)
			lastDash = false
			continue
		}
		if r == '-' || unicode.IsSpace(r) {
			if !lastDash {
				builder.WriteByte('-')
				lastDash = true
			}
		}
	}
	cleaned := strings.Trim(builder.String(), "-._")
	cleaned = invalidSkillNameChars.ReplaceAllString(cleaned, "-")
	return strings.Trim(cleaned, "-._")
}
