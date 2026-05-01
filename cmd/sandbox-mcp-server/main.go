package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const defaultTimeout = 120 * time.Second

type server struct {
	mu        sync.Mutex
	workspace string
	nextID    int64
	commands  map[string]*commandState
}

type commandState struct {
	cancel context.CancelFunc
	done   chan struct{}
	mu     sync.Mutex
	output bytes.Buffer
	err    error
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func main() {
	workspace := os.Getenv("CODINGMAN_WORKSPACE")
	if workspace == "" {
		workspace = "/workspace"
	}
	s := &server{workspace: workspace, commands: make(map[string]*commandState)}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/mcp", s.handleMCP)
	addr := getenv("CODINGMAN_MCP_ADDR", "127.0.0.1:8080")
	if err := http.ListenAndServe(addr, mux); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	defer r.Body.Close()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}})
		return
	}
	result, rpcErr := s.dispatch(r.Context(), req.Method, req.Params)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result}
	if rpcErr != nil {
		resp.Result = nil
		resp.Error = rpcErr
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo": map[string]any{
				"name":    "codingman-sandbox",
				"version": "0.1.0",
			},
			"capabilities": map[string]any{"tools": map[string]any{}},
		}, nil
	case "tools/list":
		return map[string]any{"tools": tools()}, nil
	case "tools/call":
		var payload struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(params, &payload); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		text, err := s.callTool(ctx, payload.Name, payload.Arguments)
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	case "resources/list":
		return map[string]any{"resources": []any{}}, nil
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func tools() []map[string]any {
	return []map[string]any{
		toolSchema("bash_execute", "Execute a shell command in the sandbox.", map[string]any{
			"command": "string", "cwd": "string", "timeout": "integer",
		}, []string{"command"}),
		toolSchema("bash_output", "Read command output by command_id.", map[string]any{"command_id": "string"}, []string{"command_id"}),
		toolSchema("bash_kill", "Kill a running command by command_id.", map[string]any{"command_id": "string"}, []string{"command_id"}),
		toolSchema("file_write", "Write a file inside the mounted workspace.", map[string]any{"path": "string", "content": "string", "overwrite": "boolean"}, []string{"path", "content"}),
		toolSchema("grep", "Search files inside the mounted workspace.", map[string]any{"pattern": "string", "path": "string", "limit": "integer"}, []string{"pattern"}),
		toolSchema("check_runtime", "Check sandbox runtimes and mount status.", map[string]any{}, nil),
	}
}

func toolSchema(name string, description string, props map[string]any, required []string) map[string]any {
	properties := map[string]any{}
	for key, typ := range props {
		properties[key] = map[string]any{"type": typ}
	}
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": properties,
			"required":   required,
		},
	}
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

func (s *server) callTool(ctx context.Context, name string, args map[string]any) (string, error) {
	switch name {
	case "bash_execute":
		return s.bashExecute(ctx, args)
	case "bash_output":
		return s.bashOutput(args)
	case "bash_kill":
		return s.bashKill(args)
	case "file_write":
		return s.fileWrite(args)
	case "grep":
		return s.grep(args)
	case "check_runtime":
		return s.checkRuntime()
	default:
		return "", fmt.Errorf("unknown tool: %s", name)
	}
}

func (s *server) bashExecute(ctx context.Context, args map[string]any) (string, error) {
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return "", errors.New("command is required")
	}
	cwd, _ := args["cwd"].(string)
	if cwd == "" {
		cwd = s.workspace
	}
	if _, err := s.safePath(cwd); err != nil {
		return "", err
	}
	timeout := durationArg(args["timeout"], defaultTimeout)
	cmdCtx, cancel := context.WithCancel(context.Background())
	state := &commandState{cancel: cancel, done: make(chan struct{})}
	id := s.nextCommandID()
	s.mu.Lock()
	s.commands[id] = state
	s.mu.Unlock()

	cmd := exec.CommandContext(cmdCtx, "bash", "-lc", command)
	cmd.Dir = cwd
	cmd.Stdout = &lockedWriter{state: state}
	cmd.Stderr = &lockedWriter{state: state}
	go func() {
		state.err = cmd.Run()
		close(state.done)
	}()

	select {
	case <-state.done:
		return state.snapshot(id), nil
	case <-time.After(timeout):
		return state.snapshot(id), nil
	case <-ctx.Done():
		cancel()
		return state.snapshot(id), ctx.Err()
	}
}

func (s *server) bashOutput(args map[string]any) (string, error) {
	state, id, err := s.command(args)
	if err != nil {
		return "", err
	}
	return state.snapshot(id), nil
}

func (s *server) bashKill(args map[string]any) (string, error) {
	state, id, err := s.command(args)
	if err != nil {
		return "", err
	}
	state.cancel()
	return "killed command_id=" + id, nil
}

func (s *server) command(args map[string]any) (*commandState, string, error) {
	id, _ := args["command_id"].(string)
	if id == "" {
		return nil, "", errors.New("command_id is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.commands[id]
	if state == nil {
		return nil, "", fmt.Errorf("command not found: %s", id)
	}
	return state, id, nil
}

func (s *server) fileWrite(args map[string]any) (string, error) {
	path, _ := args["path"].(string)
	content, ok := args["content"].(string)
	if path == "" || !ok {
		return "", errors.New("path and content are required")
	}
	safe, err := s.safePath(path)
	if err != nil {
		return "", err
	}
	overwrite, _ := args["overwrite"].(bool)
	if !overwrite {
		if _, err := os.Stat(safe); err == nil {
			return "", errors.New("file already exists")
		}
	}
	if err := os.MkdirAll(filepath.Dir(safe), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(safe, []byte(content), 0o644); err != nil {
		return "", err
	}
	return "成功写入文件: " + safe, nil
}

func (s *server) grep(args map[string]any) (string, error) {
	pattern, _ := args["pattern"].(string)
	if pattern == "" {
		return "", errors.New("pattern is required")
	}
	root, _ := args["path"].(string)
	if root == "" {
		root = s.workspace
	}
	safeRoot, err := s.safePath(root)
	if err != nil {
		return "", err
	}
	limit := intArg(args["limit"], 100)
	expr, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}
	matches := make([]string, 0)
	err = filepath.WalkDir(safeRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() || len(matches) >= limit {
			return walkErr
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if expr.MatchString(line) {
				matches = append(matches, fmt.Sprintf("%s:%d: %s", path, i+1, line))
				if len(matches) >= limit {
					break
				}
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return strings.Join(matches, "\n"), nil
}

func (s *server) checkRuntime() (string, error) {
	checks := map[string]string{
		"workspace": s.workspace,
		"node":      commandVersion("node", "--version"),
		"python3":   commandVersion("python3", "--version"),
		"git":       commandVersion("git", "--version"),
		"busybox":   commandVersion("busybox", "--help"),
	}
	data, _ := json.MarshalIndent(checks, "", "  ")
	return string(data), nil
}

func (s *server) safePath(value string) (string, error) {
	if value == "" {
		return "", errors.New("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(s.workspace, value)
	}
	abs, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	workspace, err := filepath.Abs(s.workspace)
	if err != nil {
		return "", err
	}
	if abs != workspace && !strings.HasPrefix(abs, workspace+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes workspace: %s", value)
	}
	return abs, nil
}

func (s *server) nextCommandID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	return strconv.FormatInt(s.nextID, 10)
}

func (state *commandState) snapshot(id string) string {
	state.mu.Lock()
	output := state.output.String()
	err := state.err
	state.mu.Unlock()
	status := "running"
	select {
	case <-state.done:
		status = "exited"
	default:
	}
	payload := map[string]any{"command_id": id, "status": status, "output": output}
	if err != nil {
		payload["error"] = err.Error()
	}
	data, _ := json.MarshalIndent(payload, "", "  ")
	return string(data)
}

type lockedWriter struct {
	state *commandState
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	return w.state.output.Write(p)
}

func commandVersion(name string, arg string) string {
	out, err := exec.Command(name, arg).CombinedOutput()
	if err != nil {
		return err.Error()
	}
	return strings.TrimSpace(string(out))
}

func getenv(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func durationArg(value any, fallback time.Duration) time.Duration {
	ms := intArg(value, int(fallback/time.Millisecond))
	if ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}

func intArg(value any, fallback int) int {
	switch v := value.(type) {
	case int:
		if v > 0 {
			return v
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	case string:
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			return parsed
		}
	}
	return fallback
}
