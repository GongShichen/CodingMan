package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const defaultAgentsMDMaxBytes = 40000
const defaultMemoryMaxChars = 40000

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
	ProjectRoot      string
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
	if config.ProjectRoot == "" {
		config.ProjectRoot = findMemoryProjectRoot(config.Cwd)
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
		agentsMD, _ := LoadProjectMemory(config)
		if agentsMD != "" {
			parts = append(parts, "## 项目记忆与规则\n")
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

type memoryLoadContext struct {
	projectRoot string
	cwd         string
	maxChars    int
	usedChars   int
	visited     map[string]struct{}
}

func LoadProjectMemory(config ContextConfig) (string, error) {
	config = normalizeContextConfig(config)
	ctx := &memoryLoadContext{
		projectRoot: cleanRealPath(config.ProjectRoot),
		cwd:         cleanRealPath(config.Cwd),
		maxChars:    config.MaxAgentsMDBytes,
		visited:     make(map[string]struct{}),
	}
	if ctx.maxChars <= 0 {
		ctx.maxChars = defaultMemoryMaxChars
	}
	var files []string
	files = append(files, userMemoryFiles()...)
	files = append(files, projectMemoryFiles(ctx.projectRoot, ctx.cwd)...)

	var parts []string
	for _, file := range files {
		content, err := ctx.loadMemoryFile(file, filepath.Dir(file))
		if err != nil || strings.TrimSpace(content) == "" {
			continue
		}
		parts = append(parts, fmt.Sprintf("### %s\n%s", file, content))
		if ctx.usedChars >= ctx.maxChars {
			break
		}
	}
	return strings.Join(parts, "\n\n"), nil
}

func userMemoryFiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	base := filepath.Join(home, ".codingman")
	return memoryFilesInDir(base)
}

func projectMemoryFiles(projectRoot string, cwd string) []string {
	if projectRoot == "" {
		projectRoot = cwd
	}
	root := cleanAbs(projectRoot)
	target := cleanAbs(cwd)
	if !isWithinDir(root, target) {
		target = root
	}
	var reversed []string
	for current := target; ; current = filepath.Dir(current) {
		reversed = append(reversed, current)
		if current == root || current == filepath.Dir(current) {
			break
		}
	}
	var dirs []string
	for i := len(reversed) - 1; i >= 0; i-- {
		dirs = append(dirs, reversed[i])
	}
	var files []string
	for _, dir := range dirs {
		files = append(files, memoryFilesInDir(filepath.Join(dir, ".codingman"))...)
	}
	return uniqueStrings(files)
}

func memoryFilesInDir(base string) []string {
	var files []string
	agents := filepath.Join(base, "AGENTS.md")
	if fileExists(agents) {
		files = append(files, agents)
	}
	rulesDir := filepath.Join(base, "rules")
	entries, err := os.ReadDir(rulesDir)
	if err == nil {
		for _, entry := range entries {
			if entry.IsDir() || strings.ToLower(filepath.Ext(entry.Name())) != ".md" {
				continue
			}
			files = append(files, filepath.Join(rulesDir, entry.Name()))
		}
	}
	sort.Strings(files)
	return files
}

func (ctx *memoryLoadContext) loadMemoryFile(path string, includeBase string) (string, error) {
	resolved, err := ctx.safeResolve(path)
	if err != nil {
		return "", err
	}
	if _, seen := ctx.visited[resolved]; seen {
		return "", nil
	}
	ctx.visited[resolved] = struct{}{}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return "", err
	}
	body, frontmatter := splitFrontmatter(string(data))
	if !frontmatterMatches(frontmatter, ctx.cwd, ctx.projectRoot) {
		return "", nil
	}
	body = stripHTMLComments(body)
	body = ctx.expandIncludes(body, filepath.Dir(resolved))
	body = strings.TrimSpace(body)
	if body == "" {
		return "", nil
	}
	remaining := ctx.maxChars - ctx.usedChars
	if remaining <= 0 {
		return "", nil
	}
	if len([]rune(body)) > remaining {
		runes := []rune(body)
		body = string(runes[:remaining]) + "\n\n[project memory truncated]\n"
		ctx.usedChars = ctx.maxChars
		return body, nil
	}
	ctx.usedChars += len([]rune(body))
	return body, nil
}

func (ctx *memoryLoadContext) expandIncludes(content string, baseDir string) string {
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "@") || strings.Contains(trimmed, " ") {
			continue
		}
		includePath, ok := ctx.resolveInclude(trimmed, baseDir)
		if !ok {
			continue
		}
		included, err := ctx.loadMemoryFile(includePath, filepath.Dir(includePath))
		if err == nil {
			lines[i] = included
		}
	}
	return strings.Join(lines, "\n")
}

func (ctx *memoryLoadContext) resolveInclude(token string, baseDir string) (string, bool) {
	ref := strings.TrimPrefix(strings.TrimSpace(token), "@")
	if ref == "" {
		return "", false
	}
	if strings.HasPrefix(ref, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		return filepath.Join(home, strings.TrimPrefix(ref, "~/")), true
	}
	if strings.HasPrefix(ref, "/.") {
		return filepath.Join(ctx.projectRoot, strings.TrimPrefix(ref, "/")), true
	}
	if strings.HasPrefix(ref, "/") {
		return ref, true
	}
	return filepath.Join(baseDir, ref), true
}

func (ctx *memoryLoadContext) safeResolve(path string) (string, error) {
	abs := cleanAbs(path)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved = cleanAbs(resolved)
	home, _ := os.UserHomeDir()
	home = cleanRealPath(home)
	allowed := []string{ctx.projectRoot, cleanRealPath(filepath.Join(home, ".codingman"))}
	if strings.HasPrefix(resolved, home) {
		allowed = append(allowed, home)
	}
	for _, root := range allowed {
		if root != "" && isWithinDir(cleanAbs(root), resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("memory include escapes allowed roots: %s", path)
}

func splitFrontmatter(content string) (string, string) {
	if !strings.HasPrefix(content, "---\n") {
		return content, ""
	}
	end := strings.Index(content[4:], "\n---")
	if end < 0 {
		return content, ""
	}
	front := content[4 : 4+end]
	body := content[4+end:]
	body = strings.TrimPrefix(body, "\n---")
	body = strings.TrimPrefix(body, "---")
	body = strings.TrimPrefix(body, "\n")
	return body, front
}

func frontmatterMatches(frontmatter string, cwd string, projectRoot string) bool {
	frontmatter = strings.TrimSpace(frontmatter)
	if frontmatter == "" {
		return true
	}
	patterns := frontmatterPathPatterns(frontmatter)
	if len(patterns) == 0 {
		return true
	}
	rel, err := filepath.Rel(projectRoot, cwd)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = cwd
	}
	rel = filepath.ToSlash(rel)
	for _, pattern := range patterns {
		if ok, _ := filepath.Match(filepath.ToSlash(pattern), rel); ok {
			return true
		}
		if matched, _ := regexp.MatchString(pattern, rel); matched {
			return true
		}
	}
	return false
}

func frontmatterPathPatterns(frontmatter string) []string {
	var patterns []string
	lines := strings.Split(frontmatter, "\n")
	inPaths := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "paths:") || strings.HasPrefix(trimmed, "path:") || strings.HasPrefix(trimmed, "match:") || strings.HasPrefix(trimmed, "matches:") {
			inPaths = strings.HasSuffix(trimmed, ":")
			if value := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(trimmed, "paths:"), "path:"), "matches:"), "match:")); value != "" {
				patterns = append(patterns, splitInlinePatterns(value)...)
			}
			continue
		}
		if inPaths && strings.HasPrefix(trimmed, "-") {
			patterns = append(patterns, strings.Trim(strings.TrimSpace(strings.TrimPrefix(trimmed, "-")), `"'`))
			continue
		}
		inPaths = false
	}
	return compactStrings(patterns)
}

func splitInlinePatterns(value string) []string {
	value = strings.Trim(value, `[] "'`)
	if value == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(parts[i]), `"'`)
	}
	return compactStrings(parts)
}

var htmlCommentPattern = regexp.MustCompile(`(?s)<!--.*?-->`)

func stripHTMLComments(content string) string {
	return htmlCommentPattern.ReplaceAllString(content, "")
}

func findMemoryProjectRoot(start string) string {
	current := cleanAbs(start)
	for {
		if fileExists(filepath.Join(current, ".git")) || fileExists(filepath.Join(current, "go.mod")) || fileExists(filepath.Join(current, ".codingman")) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleanAbs(start)
		}
		current = parent
	}
}

func cleanAbs(path string) string {
	if path == "" {
		path = "."
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(abs)
}

func cleanRealPath(path string) string {
	abs := cleanAbs(path)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs
	}
	return cleanAbs(resolved)
}

func isWithinDir(root string, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func compactStrings(values []string) []string {
	var result []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			result = append(result, value)
		}
	}
	return result
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
