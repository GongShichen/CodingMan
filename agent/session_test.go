package agent_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
)

func TestSessionStorePersistsAndLoadsLatestSnapshot(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	projectDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	store, err := agent.NewSessionStore(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := agent.NewSessionID()
	snapshot := agent.SessionSnapshot{
		SessionID:  sessionID,
		ProjectDir: projectDir,
		Messages: []agent.Message{{
			Role:    "user",
			Content: []agent.ContentBlock{agent.TextBlock("hello")},
		}},
		FileHistory: []agent.FileHistoryEntry{{Path: "main.go", Action: "read"}},
		Attribution: []agent.AttributionEntry{{
			Path:    "main.go",
			AgentID: "main",
			Note:    "edit",
		}},
		Todos: []agent.TodoItem{{ID: "todo-1", Content: "check", Status: "pending"}},
	}
	if err := store.AppendSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}

	loaded, err := store.Load(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Messages) != 1 || loaded.Messages[0].Content[0].Text != "hello" {
		t.Fatalf("messages not restored: %+v", loaded.Messages)
	}
	if len(loaded.FileHistory) != 1 || loaded.FileHistory[0].Path != "main.go" {
		t.Fatalf("file history not restored: %+v", loaded.FileHistory)
	}
	if len(loaded.Attribution) != 1 || loaded.Attribution[0].AgentID != "main" {
		t.Fatalf("attribution not restored: %+v", loaded.Attribution)
	}
	if len(loaded.Todos) != 1 || loaded.Todos[0].ID != "todo-1" {
		t.Fatalf("todos not restored: %+v", loaded.Todos)
	}
	if _, err := os.Stat(filepath.Join(home, ".codingman", "projects", agent.ProjectHash(projectDir), sessionID+".jsonl")); err != nil {
		t.Fatal(err)
	}

	latest, err := store.LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	if latest.SessionID != sessionID {
		t.Fatalf("latest session id = %q, want %q", latest.SessionID, sessionID)
	}
}

func TestAgentRestoreSessionSnapshot(t *testing.T) {
	a := agent.NewAgent(agent.AgentConfig{LLM: &fakeLLM{}})
	a.Restore(agent.SessionSnapshot{
		SessionID: "s1",
		Messages: []agent.Message{{
			Role:    "assistant",
			Content: []agent.ContentBlock{agent.TextBlock("restored")},
		}},
	})
	messages := a.Messages()
	if len(messages) != 1 || messages[0].Content[0].Text != "restored" {
		t.Fatalf("agent messages not restored: %+v", messages)
	}
}
