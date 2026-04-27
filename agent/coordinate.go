package agent

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const CoordinatorSystemPrompt = `## 1. Role
You are the coordinator agent. Own the user's goal end to end, decompose work, assign independent subtasks to workers, integrate results, and make the final engineering decision.

## 2. Tools
Use normal coding tools for immediate work. Use the subagent tool only to start direct child workers for concrete side tasks that can run independently. Use await to collect results and taskstop to cancel work that is no longer useful.

## 3. Worker Modes
worker mode starts an isolated worker with only the delegated task. fork mode starts a worker from a snapshot of the coordinator conversation, then adds the delegated task. Prefer worker mode unless the worker genuinely needs the parent conversation.

## 4. Workflow
Reflect briefly, split independent work, start workers asynchronously when useful, continue local work while workers run, await only when their result is needed, integrate outputs, verify important claims, and stop obsolete workers.

## 5. Instructions
A2A communication is limited to the coordinator and its direct workers. Workers cannot create workers. Keep worker tasks narrow, include expected output, and avoid overlapping write ownership. Results arrive as <task-notification> XML.

## 6. Writing And Examples
Example: start two workers for independent file areas, keep implementing the central path locally, await both workers, reconcile conflicts, run tests, then report changed files and verification.`

type WorkerMode string

const (
	WorkerModeFork   WorkerMode = "fork"
	WorkerModeWorker WorkerMode = "worker"
)

type TaskStatus string

const (
	TaskStatusRunning   TaskStatus = "running"
	TaskStatusCompleted TaskStatus = "completed"
	TaskStatusFailed    TaskStatus = "failed"
	TaskStatusKilled    TaskStatus = "killed"
)

type CoordinationConfig struct {
	SharedTempDir     string
	EnableGitWorktree bool
	WorktreeBaseDir   string
}

type WorkerTaskRequest struct {
	Task         string
	AgentName    string
	SystemPrompt string
	Mode         WorkerMode
}

type WorkerTask struct {
	ID            string
	AgentID       string
	AgentName     string
	Mode          WorkerMode
	Status        TaskStatus
	Request       WorkerTaskRequest
	Result        SubAgentResult
	Error         string
	StartedAt     time.Time
	CompletedAt   time.Time
	WorktreeDir   string
	SharedTempDir string
	cancel        context.CancelFunc
	done          chan struct{}
}

type TaskNotification struct {
	XMLName       xml.Name   `xml:"task-notification"`
	TaskID        string     `xml:"task-id,attr"`
	AgentID       string     `xml:"agent-id,attr"`
	AgentName     string     `xml:"agent-name,attr,omitempty"`
	Status        TaskStatus `xml:"status,attr"`
	Mode          WorkerMode `xml:"mode,attr,omitempty"`
	WorktreeDir   string     `xml:"worktree-dir,omitempty"`
	SharedTempDir string     `xml:"shared-temp-dir,omitempty"`
	Content       string     `xml:"content,omitempty"`
	Error         string     `xml:"error,omitempty"`
}

func (notification TaskNotification) String() string {
	data, err := xml.MarshalIndent(notification, "", "  ")
	if err != nil {
		return fmt.Sprintf(`<task-notification task-id="%s" agent-id="%s" status="%s"><error>%s</error></task-notification>`, notification.TaskID, notification.AgentID, notification.Status, err)
	}
	return string(data)
}

type Coordinator struct {
	mu           sync.Mutex
	parent       *Agent
	config       CoordinationConfig
	nextTask     atomic.Uint64
	tasks        map[string]*WorkerTask
	sharedTmpDir string
}

func NewCoordinator(parent *Agent, config CoordinationConfig) *Coordinator {
	return &Coordinator{
		parent: parent,
		config: config,
		tasks:  make(map[string]*WorkerTask),
	}
}

func (coordinator *Coordinator) StartWorker(ctx context.Context, request WorkerTaskRequest) (TaskNotification, error) {
	if coordinator == nil || coordinator.parent == nil {
		return TaskNotification{}, errors.New("coordinator is unavailable")
	}
	request.Task = strings.TrimSpace(request.Task)
	if request.Task == "" {
		return TaskNotification{}, errors.New("worker task is required")
	}
	if request.Mode == "" {
		request.Mode = WorkerModeWorker
	}
	if request.Mode != WorkerModeWorker && request.Mode != WorkerModeFork {
		return TaskNotification{}, fmt.Errorf("invalid worker mode: %s", request.Mode)
	}

	taskIndex := coordinator.nextTask.Add(1)
	taskID := fmt.Sprintf("task-%d", taskIndex)
	childID := coordinator.parent.nextChildAgentID()
	agentName := strings.TrimSpace(request.AgentName)
	if agentName == "" {
		agentName = fmt.Sprintf("worker-%d", taskIndex)
	}
	sharedTempDir, err := coordinator.sharedTemp()
	if err != nil {
		return TaskNotification{}, err
	}
	worktreeDir, err := coordinator.prepareWorktree(ctx, taskID)
	if err != nil {
		return TaskNotification{}, err
	}

	taskCtx, cancel := context.WithCancel(ctx)
	task := &WorkerTask{
		ID:            taskID,
		AgentID:       childID,
		AgentName:     agentName,
		Mode:          request.Mode,
		Status:        TaskStatusRunning,
		Request:       request,
		StartedAt:     time.Now(),
		WorktreeDir:   worktreeDir,
		SharedTempDir: sharedTempDir,
		cancel:        cancel,
		done:          make(chan struct{}),
	}

	coordinator.mu.Lock()
	coordinator.tasks[taskID] = task
	coordinator.mu.Unlock()

	notification := task.notification()
	go coordinator.runWorker(taskCtx, task)
	return notification, nil
}

func (coordinator *Coordinator) AwaitTask(ctx context.Context, taskID string) (TaskNotification, error) {
	task, err := coordinator.task(taskID)
	if err != nil {
		return TaskNotification{}, err
	}
	select {
	case <-task.done:
		return task.notification(), nil
	case <-ctx.Done():
		return task.notification(), ctx.Err()
	}
}

func (coordinator *Coordinator) StopTask(taskID string) (TaskNotification, error) {
	task, err := coordinator.task(taskID)
	if err != nil {
		return TaskNotification{}, err
	}
	coordinator.mu.Lock()
	if task.Status == TaskStatusRunning {
		task.Status = TaskStatusKilled
		task.Error = "killed by coordinator"
		task.CompletedAt = time.Now()
		task.cancel()
	}
	coordinator.mu.Unlock()
	return task.notification(), nil
}

func (coordinator *Coordinator) TaskStatus(taskID string) (TaskNotification, error) {
	task, err := coordinator.task(taskID)
	if err != nil {
		return TaskNotification{}, err
	}
	return task.notification(), nil
}

func (coordinator *Coordinator) ListTasks() []TaskNotification {
	if coordinator == nil {
		return nil
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	result := make([]TaskNotification, 0, len(coordinator.tasks))
	for _, task := range coordinator.tasks {
		result = append(result, task.notificationLocked())
	}
	return result
}

func (coordinator *Coordinator) runWorker(ctx context.Context, task *WorkerTask) {
	defer close(task.done)
	result, err := coordinator.parent.runWorkerAgent(ctx, task)
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if task.Status == TaskStatusKilled {
		return
	}
	task.Result = result
	task.CompletedAt = time.Now()
	if err != nil {
		task.Status = TaskStatusFailed
		task.Error = err.Error()
		return
	}
	task.Status = TaskStatusCompleted
}

func (coordinator *Coordinator) task(taskID string) (*WorkerTask, error) {
	if coordinator == nil {
		return nil, errors.New("coordinator is unavailable")
	}
	taskID = strings.TrimSpace(taskID)
	if taskID == "" {
		return nil, errors.New("task_id is required")
	}
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	task := coordinator.tasks[taskID]
	if task == nil {
		return nil, fmt.Errorf("task not found: %s", taskID)
	}
	return task, nil
}

func (coordinator *Coordinator) sharedTemp() (string, error) {
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	if coordinator.sharedTmpDir != "" {
		return coordinator.sharedTmpDir, nil
	}
	if coordinator.config.SharedTempDir != "" {
		if err := os.MkdirAll(coordinator.config.SharedTempDir, 0755); err != nil {
			return "", err
		}
		coordinator.sharedTmpDir = coordinator.config.SharedTempDir
		return coordinator.sharedTmpDir, nil
	}
	dir, err := os.MkdirTemp("", "codingman-workers-*")
	if err != nil {
		return "", err
	}
	coordinator.sharedTmpDir = dir
	return dir, nil
}

func (coordinator *Coordinator) prepareWorktree(ctx context.Context, taskID string) (string, error) {
	if !coordinator.config.EnableGitWorktree {
		return "", nil
	}
	baseDir := coordinator.config.WorktreeBaseDir
	if baseDir == "" {
		shared, err := coordinator.sharedTemp()
		if err != nil {
			return "", err
		}
		baseDir = filepath.Join(shared, "worktrees")
	}
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return "", err
	}
	worktreeDir := filepath.Join(baseDir, taskID)
	cmd := exec.CommandContext(ctx, "git", "worktree", "add", "--detach", worktreeDir, "HEAD")
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("create git worktree: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return worktreeDir, nil
}

func (task *WorkerTask) notification() TaskNotification {
	if task == nil {
		return TaskNotification{}
	}
	return task.notificationLocked()
}

func (task *WorkerTask) notificationLocked() TaskNotification {
	notification := TaskNotification{
		TaskID:        task.ID,
		AgentID:       task.AgentID,
		AgentName:     task.AgentName,
		Status:        task.Status,
		Mode:          task.Mode,
		WorktreeDir:   task.WorktreeDir,
		SharedTempDir: task.SharedTempDir,
		Content:       task.Result.Content,
		Error:         task.Error,
	}
	if notification.Error == "" {
		notification.Error = task.Result.Error
	}
	return notification
}

func taskNotificationsJSON(notifications []TaskNotification) (string, error) {
	data, err := json.MarshalIndent(notifications, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}
