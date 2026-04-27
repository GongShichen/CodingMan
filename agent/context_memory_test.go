package agent_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
)

func TestLoadProjectMemoryOrderIncludesAndComments(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	cwd := filepath.Join(project, "pkg", "feature")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, ".codingman", "AGENTS.md"), "user memory\n")
	writeFile(t, filepath.Join(project, ".codingman", "AGENTS.md"), "root memory\n@shared.md\n<!-- hidden -->\n")
	writeFile(t, filepath.Join(project, ".codingman", "shared.md"), "included root\n")
	writeFile(t, filepath.Join(project, "pkg", ".codingman", "rules", "pkg.md"), "pkg rule\n")
	writeFile(t, filepath.Join(cwd, ".codingman", "AGENTS.md"), "local memory\n")

	content, err := agent.LoadProjectMemory(agent.ContextConfig{
		Cwd:              cwd,
		ProjectRoot:      project,
		BaseSystem:       "base",
		MaxAgentsMDBytes: 40000,
		LoadAgentsMD:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	assertOrder(t, content, "user memory", "root memory", "included root", "pkg rule", "local memory")
	if strings.Contains(content, "hidden") {
		t.Fatalf("html comment was not stripped:\n%s", content)
	}
}

func TestLoadProjectMemoryFrontmatterPathCondition(t *testing.T) {
	project := t.TempDir()
	cwd := filepath.Join(project, "src")
	if err := os.MkdirAll(cwd, 0755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(project, ".codingman", "rules", "match.md"), "---\npaths: src\n---\nmatched rule\n")
	writeFile(t, filepath.Join(project, ".codingman", "rules", "array.md"), "---\npaths: [\"src\", \"cmd/**\"]\n---\narray matched rule\n")
	writeFile(t, filepath.Join(project, ".codingman", "rules", "skip.md"), "---\npaths: docs\n---\nskipped rule\n")

	content, err := agent.LoadProjectMemory(agent.ContextConfig{
		Cwd:              cwd,
		ProjectRoot:      project,
		BaseSystem:       "base",
		MaxAgentsMDBytes: 40000,
		LoadAgentsMD:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "matched rule") {
		t.Fatalf("matching rule missing:\n%s", content)
	}
	if !strings.Contains(content, "array matched rule") {
		t.Fatalf("quoted array rule missing:\n%s", content)
	}
	if strings.Contains(content, "skipped rule") {
		t.Fatalf("non-matching rule loaded:\n%s", content)
	}
}

func TestLoadProjectMemoryRejectsUnsafeSymlink(t *testing.T) {
	project := t.TempDir()
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "secret.md"), "secret\n")
	if err := os.MkdirAll(filepath.Join(project, ".codingman"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.md"), filepath.Join(project, ".codingman", "linked.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writeFile(t, filepath.Join(project, ".codingman", "AGENTS.md"), "@linked.md\n")

	result := agent.LoadProjectMemoryWithWarnings(agent.ContextConfig{
		Cwd:              project,
		ProjectRoot:      project,
		BaseSystem:       "base",
		MaxAgentsMDBytes: 40000,
		LoadAgentsMD:     true,
	})
	if len(result.Warnings) == 0 {
		t.Fatalf("expected unsafe symlink warning")
	}
	if strings.Contains(result.Content, "secret") {
		t.Fatalf("unsafe symlink include loaded:\n%s", result.Content)
	}
}

func TestLoadProjectMemoryProgressiveIndex(t *testing.T) {
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".codingman", "AGENTS.md"), "root memory\n")
	largeRulePath := filepath.Join(project, ".codingman", "rules", "large.md")
	writeFile(t, largeRulePath, "large rule start\n"+strings.Repeat("x", 2000)+"\nvery-large-rule-tail\n")

	content, err := agent.LoadProjectMemory(agent.ContextConfig{
		Cwd:                       project,
		ProjectRoot:               project,
		BaseSystem:                "base",
		MaxAgentsMDBytes:          40000,
		ProgressiveMemoryMaxChars: 700,
		LoadAgentsMD:              true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "root memory") {
		t.Fatalf("core memory missing:\n%s", content)
	}
	if !strings.Contains(content, "渐进式加载索引") || !strings.Contains(content, largeRulePath) {
		t.Fatalf("progressive index missing:\n%s", content)
	}
	if strings.Contains(content, "very-large-rule-tail") {
		t.Fatalf("large rule was fully loaded instead of deferred:\n%s", content)
	}
}

func TestLoadSkillsUserProjectOverrideAndContextModes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	writeFile(t, filepath.Join(home, ".codingman", "skills", "review", "SKILL.md"), "---\nname: review\ndescription: user review skill\nallow_tools: [read]\ncontext: inline\n---\nuser inline body\n")
	writeFile(t, filepath.Join(project, ".codingman", "skills", "review", "SKILL.md"), "---\nname: review\ndescription: project review skill\nallow_tools: [read, grep]\ncontext: fork\n---\nproject body should not inline\n")
	writeFile(t, filepath.Join(project, ".codingman", "skills", "build", "SKILL.md"), "---\nname: build\ndescription: build skill\ncontext: inline\n---\nbuild inline body\n")

	result := agent.LoadSkillsWithWarnings(agent.ContextConfig{
		Cwd:         project,
		ProjectRoot: project,
		BaseSystem:  "base",
	})
	if len(result.Warnings) != 0 {
		t.Fatalf("unexpected skill warnings: %+v", result.Warnings)
	}
	if len(result.Skills) != 2 {
		t.Fatalf("expected two skills after override, got %+v", result.Skills)
	}
	if !strings.Contains(result.Content, "project review skill") || strings.Contains(result.Content, "user review skill") {
		t.Fatalf("project skill did not override user skill:\n%s", result.Content)
	}
	if strings.Contains(result.Content, "project body should not inline") {
		t.Fatalf("fork skill body was inlined:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "build inline body") {
		t.Fatalf("inline skill body missing:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "allow_tools: read, grep") {
		t.Fatalf("allow_tools missing:\n%s", result.Content)
	}
}

func TestBuildSystemPromptIncludesSkills(t *testing.T) {
	project := t.TempDir()
	writeFile(t, filepath.Join(project, ".codingman", "skills", "docs", "SKILL.md"), "---\nname: docs\ndescription: documentation skill\ncontext: inline\n---\nwrite docs carefully\n")
	system, err := agent.BuildSystemPromptWithConfig(agent.ContextConfig{
		Cwd:          project,
		ProjectRoot:  project,
		BaseSystem:   "base",
		IncludeDate:  false,
		LoadAgentsMD: false,
		LoadSkills:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(system, "## Skills") || !strings.Contains(system, "write docs carefully") {
		t.Fatalf("skill missing from system prompt:\n%s", system)
	}
}

func TestInlineSkillBudgetDefersLargeBody(t *testing.T) {
	project := t.TempDir()
	skillPath := filepath.Join(project, ".codingman", "skills", "large", "SKILL.md")
	writeFile(t, skillPath, "---\nname: large\ndescription: large skill\ncontext: inline\n---\nlarge skill start\n"+strings.Repeat("x", 2000)+"\nlarge skill tail\n")
	result := agent.LoadSkillsWithWarnings(agent.ContextConfig{
		Cwd:                      project,
		ProjectRoot:              project,
		BaseSystem:               "base",
		ProgressiveSkillMaxChars: 500,
	})
	if strings.Contains(result.Content, "large skill tail") {
		t.Fatalf("large inline skill was not deferred:\n%s", result.Content)
	}
	if !strings.Contains(result.Content, "Skill 渐进式加载索引") || !strings.Contains(result.Content, skillPath) {
		t.Fatalf("skill progressive index missing:\n%s", result.Content)
	}
}

func writeFile(t *testing.T, path string, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func assertOrder(t *testing.T, content string, values ...string) {
	t.Helper()
	last := -1
	for _, value := range values {
		index := strings.Index(content, value)
		if index < 0 {
			t.Fatalf("%q missing from:\n%s", value, content)
		}
		if index < last {
			t.Fatalf("%q appeared out of order in:\n%s", value, content)
		}
		last = index
	}
}
