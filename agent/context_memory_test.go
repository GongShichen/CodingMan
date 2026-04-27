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

	content, err := agent.LoadProjectMemory(agent.ContextConfig{
		Cwd:              project,
		ProjectRoot:      project,
		BaseSystem:       "base",
		MaxAgentsMDBytes: 40000,
		LoadAgentsMD:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(content, "secret") {
		t.Fatalf("unsafe symlink include loaded:\n%s", content)
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
