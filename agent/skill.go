package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const defaultProgressiveSkillMaxChars = 12000

type Skill struct {
	Name               string
	Description        string
	AllowTools         []string
	Context            string
	Path               string
	Content            string
	CodingManGenerated bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
	UserLevel          bool
}

type SkillLoadResult struct {
	Skills   []Skill
	Content  string
	Warnings []string
}

func cloneSkill(skill Skill) Skill {
	skill.AllowTools = append([]string(nil), skill.AllowTools...)
	return skill
}

func cloneSkills(skills []Skill) []Skill {
	if len(skills) == 0 {
		return nil
	}
	copied := make([]Skill, len(skills))
	for i, skill := range skills {
		copied[i] = cloneSkill(skill)
	}
	return copied
}

func LoadSkillsWithWarnings(config ContextConfig) SkillLoadResult {
	config = normalizeContextConfig(config)
	var warnings []string
	skillsByName := make(map[string]Skill)
	var order []string
	for _, file := range skillFiles(config.ProjectRoot) {
		skill, err := loadSkillFile(file, config.ProjectRoot)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		if skill.Name == "" {
			warnings = append(warnings, fmt.Sprintf("%s: skill name is required", file))
			continue
		}
		if _, exists := skillsByName[skill.Name]; !exists {
			order = append(order, skill.Name)
		}
		skillsByName[skill.Name] = skill
	}
	var skills []Skill
	for _, name := range order {
		skills = append(skills, skillsByName[name])
	}
	return SkillLoadResult{
		Skills:   skills,
		Content:  renderSkills(skills, config.ProgressiveSkillMaxChars),
		Warnings: warnings,
	}
}

func skillFiles(projectRoot string) []string {
	var files []string
	if home, err := os.UserHomeDir(); err == nil {
		files = append(files, skillFilesInDir(filepath.Join(home, ".codingman", "skills"))...)
	}
	files = append(files, skillFilesInDir(filepath.Join(cleanAbs(projectRoot), ".codingman", "skills"))...)
	return files
}

func skillFilesInDir(base string) []string {
	entries, err := os.ReadDir(base)
	if err != nil {
		return nil
	}
	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(base, entry.Name(), "SKILL.md")
		if fileExists(path) {
			files = append(files, path)
		}
	}
	sort.Strings(files)
	return files
}

func loadSkillFile(path string, projectRoot string) (Skill, error) {
	resolved, err := safeResolveSkillFile(path, projectRoot)
	if err != nil {
		return Skill{}, err
	}
	data, err := os.ReadFile(resolved)
	if err != nil {
		return Skill{}, err
	}
	body, frontmatter := splitFrontmatter(string(data))
	var metadata struct {
		Name               string   `yaml:"name"`
		Description        string   `yaml:"description"`
		AllowTools         []string `yaml:"allow_tools"`
		Context            string   `yaml:"context"`
		CodingManGenerated bool     `yaml:"codingman_generated"`
		CreatedAt          string   `yaml:"created_at"`
		UpdatedAt          string   `yaml:"updated_at"`
	}
	if strings.TrimSpace(frontmatter) != "" {
		if err := yaml.Unmarshal([]byte(frontmatter), &metadata); err != nil {
			return Skill{}, err
		}
	}
	if metadata.Name == "" {
		metadata.Name = filepath.Base(filepath.Dir(resolved))
	}
	contextMode := strings.TrimSpace(metadata.Context)
	if contextMode == "" {
		contextMode = "fork"
	}
	if contextMode != "fork" && contextMode != "inline" {
		return Skill{}, fmt.Errorf("unsupported skill context %q", contextMode)
	}
	createdAt := parseSkillTime(metadata.CreatedAt)
	updatedAt := parseSkillTime(metadata.UpdatedAt)
	return Skill{
		Name:               strings.TrimSpace(metadata.Name),
		Description:        strings.TrimSpace(metadata.Description),
		AllowTools:         compactStrings(metadata.AllowTools),
		Context:            contextMode,
		Path:               resolved,
		Content:            strings.TrimSpace(stripHTMLComments(body)),
		CodingManGenerated: metadata.CodingManGenerated,
		CreatedAt:          createdAt,
		UpdatedAt:          updatedAt,
		UserLevel:          isUserSkillPath(resolved),
	}, nil
}

func parseSkillTime(value string) time.Time {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		parsed, err := time.Parse(layout, value)
		if err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func userSkillRoot() (string, bool) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false
	}
	return cleanRealPath(filepath.Join(home, ".codingman", "skills")), true
}

func isUserSkillPath(path string) bool {
	root, ok := userSkillRoot()
	if !ok || root == "" {
		return false
	}
	return isWithinDir(root, cleanRealPath(path))
}

func LoadUserSkillsWithWarnings(config ContextConfig) SkillLoadResult {
	config = normalizeContextConfig(config)
	home, err := os.UserHomeDir()
	if err != nil {
		return SkillLoadResult{}
	}
	var warnings []string
	var skills []Skill
	for _, file := range skillFilesInDir(filepath.Join(home, ".codingman", "skills")) {
		skill, err := loadSkillFile(file, config.ProjectRoot)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", file, err))
			continue
		}
		if skill.Name == "" {
			warnings = append(warnings, fmt.Sprintf("%s: skill name is required", file))
			continue
		}
		skills = append(skills, skill)
	}
	return SkillLoadResult{
		Skills:   skills,
		Content:  renderSkills(skills, config.ProgressiveSkillMaxChars),
		Warnings: warnings,
	}
}

func fullSkillContent(skill Skill) string {
	if strings.TrimSpace(skill.Path) == "" {
		return strings.TrimSpace(skill.Content)
	}
	data, err := os.ReadFile(skill.Path)
	if err != nil {
		return strings.TrimSpace(skill.Content)
	}
	return strings.TrimSpace(stripHTMLComments(string(data)))
}

func safeResolveSkillFile(path string, projectRoot string) (string, error) {
	abs := cleanAbs(path)
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	resolved = cleanAbs(resolved)
	allowed := []string{cleanRealPath(filepath.Join(projectRoot, ".codingman", "skills"))}
	if home, err := os.UserHomeDir(); err == nil {
		allowed = append(allowed, cleanRealPath(filepath.Join(home, ".codingman", "skills")))
	}
	for _, root := range allowed {
		if root != "" && isWithinDir(root, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("skill escapes allowed roots: %s", path)
}

func renderSkills(skills []Skill, maxChars int) string {
	if len(skills) == 0 {
		return ""
	}
	if maxChars <= 0 {
		maxChars = defaultProgressiveSkillMaxChars
	}
	var builder strings.Builder
	builder.WriteString("可用 Skill 按用户级 -> 项目级加载；同名 Skill 以项目级覆盖用户级。context=fork 的 Skill 只作为索引，任务需要时按 path 读取完整文件；context=inline 的 Skill 已内联。\n")
	usedChars := len([]rune(builder.String()))
	var deferred []string
	for _, skill := range skills {
		var skillBuilder strings.Builder
		skillBuilder.WriteString("\n### ")
		skillBuilder.WriteString(skill.Name)
		skillBuilder.WriteString("\n")
		if skill.Description != "" {
			skillBuilder.WriteString("- description: ")
			skillBuilder.WriteString(skill.Description)
			skillBuilder.WriteString("\n")
		}
		skillBuilder.WriteString("- path: ")
		skillBuilder.WriteString(skill.Path)
		skillBuilder.WriteString("\n")
		skillBuilder.WriteString("- context: ")
		skillBuilder.WriteString(skill.Context)
		skillBuilder.WriteString("\n")
		if len(skill.AllowTools) > 0 {
			skillBuilder.WriteString("- allow_tools: ")
			skillBuilder.WriteString(strings.Join(skill.AllowTools, ", "))
			skillBuilder.WriteString("\n")
		} else {
			skillBuilder.WriteString("- allow_tools: all\n")
		}
		if skill.Context == "inline" && skill.Content != "" {
			content := "\n" + skill.Content + "\n"
			nextLen := usedChars + len([]rune(skillBuilder.String())) + len([]rune(content))
			if nextLen > maxChars {
				deferred = append(deferred, skill.Path)
			} else {
				skillBuilder.WriteString(content)
			}
		}
		usedChars += len([]rune(skillBuilder.String()))
		builder.WriteString(skillBuilder.String())
	}
	if len(deferred) > 0 {
		builder.WriteString("\n\n## Skill 渐进式加载索引\n")
		builder.WriteString("以下 inline Skill 正文未完整注入当前 system prompt。任务需要时使用 read 按需加载：\n")
		for _, path := range uniqueStrings(deferred) {
			builder.WriteString("- ")
			builder.WriteString(path)
			builder.WriteString("\n")
		}
	}
	return strings.TrimSpace(builder.String())
}
