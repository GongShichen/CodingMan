package agent

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type FileHistoryEntry struct {
	Path      string    `json:"path"`
	Action    string    `json:"action"`
	AgentID   string    `json:"agent_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

type AttributionEntry struct {
	Path    string `json:"path"`
	AgentID string `json:"agent_id"`
	TaskID  string `json:"task_id,omitempty"`
	Note    string `json:"note,omitempty"`
}

type TodoItem struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	Status    string `json:"status"`
	AgentID   string `json:"agent_id,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	UpdatedAt string `json:"updated_at,omitempty"`
}

type SessionSnapshot struct {
	SessionID   string             `json:"session_id"`
	ProjectDir  string             `json:"project_dir"`
	Messages    []Message          `json:"messages"`
	FileHistory []FileHistoryEntry `json:"file_history"`
	Attribution []AttributionEntry `json:"attribution"`
	Todos       []TodoItem         `json:"todos"`
	UpdatedAt   time.Time          `json:"updated_at"`
}

type SessionRecord struct {
	Type      string          `json:"type"`
	Timestamp time.Time       `json:"timestamp"`
	Snapshot  SessionSnapshot `json:"snapshot"`
}

type SessionInfo struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages"`
}

type SessionStore struct {
	projectDir string
	projectKey string
	dir        string
}

func NewSessionStore(projectDir string) (*SessionStore, error) {
	absProjectDir, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	key := ProjectHash(absProjectDir)
	dir := filepath.Join(home, ".codingman", "projects", key)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}
	return &SessionStore{
		projectDir: absProjectDir,
		projectKey: key,
		dir:        dir,
	}, nil
}

func ProjectHash(projectDir string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(projectDir)))
	return hex.EncodeToString(sum[:])[:16]
}

func NewSessionID() string {
	return time.Now().UTC().Format("20060102T150405.000000000Z")
}

func (store *SessionStore) Dir() string {
	if store == nil {
		return ""
	}
	return store.dir
}

func (store *SessionStore) ProjectDir() string {
	if store == nil {
		return ""
	}
	return store.projectDir
}

func (store *SessionStore) AppendSnapshot(snapshot SessionSnapshot) error {
	if store == nil {
		return errors.New("session store is nil")
	}
	if strings.TrimSpace(snapshot.SessionID) == "" {
		return errors.New("session id is required")
	}
	snapshot.ProjectDir = store.projectDir
	snapshot.UpdatedAt = time.Now().UTC()
	record := SessionRecord{
		Type:      "session_state",
		Timestamp: snapshot.UpdatedAt,
		Snapshot:  snapshot,
	}
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	path := store.SessionPath(snapshot.SessionID)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return nil
}

func (store *SessionStore) Load(sessionID string) (SessionSnapshot, error) {
	if store == nil {
		return SessionSnapshot{}, errors.New("session store is nil")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return SessionSnapshot{}, errors.New("session id is required")
	}
	return loadSessionSnapshot(store.SessionPath(sessionID))
}

func (store *SessionStore) LoadLatest() (SessionSnapshot, error) {
	sessions, err := store.List()
	if err != nil {
		return SessionSnapshot{}, err
	}
	if len(sessions) == 0 {
		return SessionSnapshot{}, os.ErrNotExist
	}
	return store.Load(sessions[0].ID)
}

func (store *SessionStore) List() ([]SessionInfo, error) {
	if store == nil {
		return nil, errors.New("session store is nil")
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return nil, err
	}
	result := make([]SessionInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".jsonl" {
			continue
		}
		path := filepath.Join(store.dir, entry.Name())
		snapshot, err := loadSessionSnapshot(path)
		if err != nil {
			continue
		}
		result = append(result, SessionInfo{
			ID:        snapshot.SessionID,
			Path:      path,
			UpdatedAt: snapshot.UpdatedAt,
			Messages:  len(snapshot.Messages),
		})
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (store *SessionStore) SessionPath(sessionID string) string {
	sessionID = strings.TrimSpace(sessionID)
	sessionID = strings.ReplaceAll(sessionID, string(filepath.Separator), "_")
	return filepath.Join(store.dir, sessionID+".jsonl")
}

func loadSessionSnapshot(path string) (SessionSnapshot, error) {
	file, err := os.Open(path)
	if err != nil {
		return SessionSnapshot{}, err
	}
	defer file.Close()

	var latest SessionSnapshot
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record SessionRecord
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return SessionSnapshot{}, fmt.Errorf("%s: decode session jsonl: %w", path, err)
		}
		if record.Type == "session_state" {
			latest = record.Snapshot
			if latest.UpdatedAt.IsZero() {
				latest.UpdatedAt = record.Timestamp
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return SessionSnapshot{}, err
	}
	if latest.SessionID == "" {
		return SessionSnapshot{}, errors.New("session has no state records")
	}
	return latest, nil
}
