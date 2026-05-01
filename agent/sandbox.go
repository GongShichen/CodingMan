package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	SandboxEnabledAuto  = "auto"
	SandboxEnabledTrue  = "true"
	SandboxEnabledFalse = "false"

	SandboxBootstrapAuto  = "auto"
	SandboxBootstrapTrue  = "true"
	SandboxBootstrapFalse = "false"

	defaultSandboxCPUs              = 2
	defaultSandboxMemory            = "2048M"
	defaultSandboxKeepaliveInterval = 30 * time.Second
	defaultSandboxStartTimeout      = 10 * time.Minute
	defaultSandboxVsockPort         = 8080
	defaultSandboxMCPPath           = "/mcp"
)

type SandboxConfig struct {
	Enabled           string
	RootFS            string
	VFKitPath         string
	CPUs              int
	Memory            string
	KeepaliveInterval time.Duration
	SocketPath        string
	Bootstrap         string
	MCPServerPath     string
	EFIVariableStore  string
}

type SandboxManager struct {
	mu         sync.Mutex
	config     SandboxConfig
	cwd        string
	logger     Logger
	cmd        *exec.Cmd
	listener   net.Listener
	tcpAddr    string
	socketPath string
	closed     chan struct{}
	startErr   error
}

type SandboxUnavailableError struct {
	Reason string
}

func (err SandboxUnavailableError) Error() string {
	if strings.TrimSpace(err.Reason) == "" {
		return "sandbox unavailable"
	}
	return "sandbox unavailable: " + err.Reason
}

func IsSandboxUnavailable(err error) bool {
	var unavailable SandboxUnavailableError
	return errors.As(err, &unavailable)
}

func NewSandboxManager(config SandboxConfig, cwd string, logger Logger) *SandboxManager {
	if logger == nil {
		logger = noopLogger{}
	}
	config = normalizeSandboxConfig(config)
	if cwd == "" {
		cwd = "."
	}
	absCwd, err := filepath.Abs(cwd)
	if err == nil {
		cwd = absCwd
	}
	return &SandboxManager{
		config: config,
		cwd:    cwd,
		logger: logger,
		closed: make(chan struct{}),
	}
}

func normalizeSandboxConfig(config SandboxConfig) SandboxConfig {
	config.Enabled = strings.ToLower(strings.TrimSpace(config.Enabled))
	if config.Enabled == "" {
		config.Enabled = SandboxEnabledAuto
	}
	config.Bootstrap = strings.ToLower(strings.TrimSpace(config.Bootstrap))
	if config.Bootstrap == "" {
		config.Bootstrap = SandboxBootstrapAuto
	}
	if config.CPUs <= 0 {
		config.CPUs = defaultSandboxCPUs
	}
	if strings.TrimSpace(config.Memory) == "" {
		config.Memory = defaultSandboxMemory
	}
	if config.KeepaliveInterval <= 0 {
		config.KeepaliveInterval = defaultSandboxKeepaliveInterval
	}
	if strings.TrimSpace(config.VFKitPath) == "" {
		config.VFKitPath = "vfkit"
	}
	return config
}

func (sandbox *SandboxManager) ShouldRoute(mode PermissionMode, name string, input map[string]any) bool {
	if sandbox == nil {
		return false
	}
	if sandbox.config.Enabled == SandboxEnabledFalse {
		return false
	}
	if runtime.GOOS != "darwin" {
		return false
	}
	if mode != PermissionModeAsk {
		return false
	}
	return IsSandboxRequiredToolCall(name, input)
}

func (sandbox *SandboxManager) Start(ctx context.Context) error {
	if sandbox == nil {
		return SandboxUnavailableError{Reason: "manager is nil"}
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if sandbox.tcpAddr != "" {
		return nil
	}
	if sandbox.config.Enabled == SandboxEnabledFalse {
		sandbox.startErr = SandboxUnavailableError{Reason: "disabled by configuration"}
		return sandbox.startErr
	}
	if runtime.GOOS != "darwin" {
		sandbox.startErr = SandboxUnavailableError{Reason: "only macOS is supported"}
		return sandbox.startErr
	}
	if strings.TrimSpace(sandbox.config.RootFS) == "" {
		sandbox.startErr = SandboxUnavailableError{Reason: "SANDBOX_ROOTFS is not configured"}
		return sandbox.startErr
	}
	if info, err := os.Stat(sandbox.config.RootFS); err != nil {
		sandbox.startErr = SandboxUnavailableError{Reason: "SANDBOX_ROOTFS not found: " + err.Error()}
		return sandbox.startErr
	} else if info.IsDir() {
		sandbox.startErr = SandboxUnavailableError{Reason: "SANDBOX_ROOTFS must be a bootable raw disk image, got directory: " + sandbox.config.RootFS}
		return sandbox.startErr
	}
	if _, err := exec.LookPath(sandbox.config.VFKitPath); err != nil {
		sandbox.startErr = SandboxUnavailableError{Reason: "vfkit not found in PATH"}
		return sandbox.startErr
	}
	if err := sandbox.prepareHostShare(); err != nil {
		sandbox.startErr = SandboxUnavailableError{Reason: err.Error()}
		return sandbox.startErr
	}
	socketPath := sandbox.config.SocketPath
	if strings.TrimSpace(socketPath) == "" {
		socketPath = filepath.Join(os.TempDir(), fmt.Sprintf("codingman-sandbox-%d.sock", os.Getpid()))
	}
	_ = os.Remove(socketPath)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		sandbox.startErr = SandboxUnavailableError{Reason: err.Error()}
		return sandbox.startErr
	}
	sandbox.listener = listener
	sandbox.tcpAddr = listener.Addr().String()
	sandbox.socketPath = socketPath

	args := sandbox.vfkitArgs(socketPath)
	cmd := exec.CommandContext(ctx, sandbox.config.VFKitPath, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = listener.Close()
		sandbox.listener = nil
		sandbox.tcpAddr = ""
		sandbox.startErr = SandboxUnavailableError{Reason: err.Error()}
		return sandbox.startErr
	}
	sandbox.cmd = cmd
	go sandbox.proxyLoop(listener, socketPath)
	go sandbox.keepaliveLoop()
	sandbox.logger.Log(TraceIDFromContext(ctx), "sandbox started tcp=%s socket=%s rootfs=%s", sandbox.tcpAddr, socketPath, sandbox.config.RootFS)
	return nil
}

func (sandbox *SandboxManager) UpdateConfig(config SandboxConfig) error {
	if sandbox == nil {
		return SandboxUnavailableError{Reason: "manager is nil"}
	}
	if err := sandbox.Close(); err != nil {
		return err
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	sandbox.config = normalizeSandboxConfig(config)
	sandbox.startErr = nil
	return nil
}

func (sandbox *SandboxManager) vfkitArgs(socketPath string) []string {
	efiStore := sandbox.config.EFIVariableStore
	if strings.TrimSpace(efiStore) == "" {
		efiStore = sandbox.config.RootFS + ".efi"
	}
	sandboxShare := sandbox.hostShareDir()
	args := []string{
		"--bootloader", "efi,variable-store=" + efiStore + ",create",
		"--cpus", strconv.Itoa(sandbox.config.CPUs),
		"--memory", strconv.Itoa(parseMemoryMiB(sandbox.config.Memory)),
		"--device", "virtio-blk,path=" + sandbox.config.RootFS,
		"--device", "virtio-fs,sharedDir=" + sandbox.cwd + ",mountTag=codingman",
		"--device", "virtio-fs,sharedDir=" + sandboxShare + ",mountTag=codingman-sandbox",
		"--device", "virtio-net,nat",
		"--device", "virtio-rng",
		"--device", fmt.Sprintf("virtio-vsock,port=%d,socketURL=%s,connect", defaultSandboxVsockPort, socketPath),
	}
	if userData, metaData, ok := sandbox.cloudInitFiles(); ok {
		args = append(args, "--cloud-init", userData+","+metaData)
	}
	return args
}

func (sandbox *SandboxManager) prepareHostShare() error {
	share := sandbox.hostShareDir()
	if strings.TrimSpace(share) == "" {
		return errors.New("sandbox host share directory is not configured")
	}
	if err := os.MkdirAll(share, 0o755); err != nil {
		return err
	}
	if strings.TrimSpace(sandbox.config.MCPServerPath) != "" {
		if info, err := os.Stat(sandbox.config.MCPServerPath); err != nil {
			return fmt.Errorf("sandbox MCP server not found: %w", err)
		} else if info.IsDir() {
			return fmt.Errorf("sandbox MCP server is a directory: %s", sandbox.config.MCPServerPath)
		}
	}
	return os.WriteFile(filepath.Join(share, "host-workspace"), []byte(sandbox.cwd+"\n"), 0o644)
}

func (sandbox *SandboxManager) hostShareDir() string {
	if strings.TrimSpace(sandbox.config.MCPServerPath) != "" {
		return filepath.Dir(sandbox.config.MCPServerPath)
	}
	if strings.TrimSpace(sandbox.config.RootFS) != "" {
		return filepath.Dir(sandbox.config.RootFS)
	}
	return filepath.Join(os.TempDir(), "codingman-sandbox")
}

func (sandbox *SandboxManager) cloudInitFiles() (string, string, bool) {
	if strings.TrimSpace(sandbox.config.RootFS) == "" {
		return "", "", false
	}
	dir := filepath.Join(filepath.Dir(sandbox.config.RootFS), "cloud-init")
	userData := filepath.Join(dir, "user-data")
	metaData := filepath.Join(dir, "meta-data")
	if _, err := os.Stat(userData); err != nil {
		return "", "", false
	}
	if _, err := os.Stat(metaData); err != nil {
		return "", "", false
	}
	return userData, metaData, true
}

func parseMemoryMiB(value string) int {
	value = strings.TrimSpace(strings.ToUpper(value))
	value = strings.TrimSuffix(value, "MIB")
	value = strings.TrimSuffix(value, "MB")
	value = strings.TrimSuffix(value, "M")
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return 2048
	}
	return parsed
}

func (sandbox *SandboxManager) Close() error {
	if sandbox == nil {
		return nil
	}
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	select {
	case <-sandbox.closed:
	default:
		close(sandbox.closed)
	}
	if sandbox.listener != nil {
		_ = sandbox.listener.Close()
	}
	if sandbox.cmd != nil && sandbox.cmd.Process != nil {
		_ = sandbox.cmd.Process.Kill()
	}
	if sandbox.socketPath != "" {
		_ = os.Remove(sandbox.socketPath)
	}
	sandbox.cmd = nil
	sandbox.listener = nil
	sandbox.tcpAddr = ""
	sandbox.socketPath = ""
	sandbox.startErr = nil
	sandbox.closed = make(chan struct{})
	return nil
}

func (sandbox *SandboxManager) proxyLoop(listener net.Listener, socketPath string) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go sandbox.proxyConn(conn, socketPath)
	}
}

func (sandbox *SandboxManager) ProxyLoopForTest(listener net.Listener, socketPath string) {
	sandbox.proxyLoop(listener, socketPath)
}

func (sandbox *SandboxManager) proxyConn(client net.Conn, socketPath string) {
	defer client.Close()
	server, err := net.Dial("unix", socketPath)
	if err != nil {
		return
	}
	defer server.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(server, client)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, server)
		done <- struct{}{}
	}()
	<-done
}

func (sandbox *SandboxManager) keepaliveLoop() {
	ticker := time.NewTicker(sandbox.config.KeepaliveInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = sandbox.Health(ctx)
			cancel()
		case <-sandbox.closed:
			return
		}
	}
}

func (sandbox *SandboxManager) Health(ctx context.Context) error {
	addr, err := sandbox.addr()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("health status=%d", resp.StatusCode)
	}
	return nil
}

func (sandbox *SandboxManager) addr() (string, error) {
	sandbox.mu.Lock()
	defer sandbox.mu.Unlock()
	if sandbox.tcpAddr != "" {
		return sandbox.tcpAddr, nil
	}
	if sandbox.startErr != nil {
		return "", sandbox.startErr
	}
	return "", SandboxUnavailableError{Reason: "not started"}
}

func (sandbox *SandboxManager) CallTool(ctx context.Context, name string, input map[string]any) (string, error) {
	if err := sandbox.Start(ctx); err != nil {
		return "", err
	}
	if err := sandbox.WaitUntilReady(ctx); err != nil {
		return "", SandboxUnavailableError{Reason: "health check failed: " + err.Error()}
	}
	switch name {
	case "bash":
		return sandbox.callMCPTool(ctx, "bash_execute", input)
	case "write":
		return sandbox.callMCPTool(ctx, "file_write", map[string]any{
			"path":      stringValue(input, "filePath", "file_path", "path"),
			"content":   input["content"],
			"overwrite": input["overwrite"],
		})
	case "edit":
		return sandbox.callMCPTool(ctx, "bash_execute", map[string]any{
			"command": sandboxEditCommand(input),
			"cwd":     sandbox.cwd,
			"timeout": input["timeout"],
		})
	default:
		return "", fmt.Errorf("tool %s is not routed to sandbox", name)
	}
}

func (sandbox *SandboxManager) WaitUntilReady(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	waitCtx, cancel := context.WithTimeout(ctx, defaultSandboxStartTimeout)
	defer cancel()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	var lastErr error
	for {
		if err := sandbox.Health(waitCtx); err == nil {
			return nil
		} else {
			lastErr = err
		}
		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return lastErr
			}
			return waitCtx.Err()
		case <-sandbox.closed:
			return SandboxUnavailableError{Reason: "closed while waiting for health"}
		case <-ticker.C:
		}
	}
}

func (sandbox *SandboxManager) callMCPTool(ctx context.Context, toolName string, args map[string]any) (string, error) {
	addr, err := sandbox.addr()
	if err != nil {
		return "", err
	}
	reqBody := map[string]any{
		"jsonrpc": "2.0",
		"id":      time.Now().UnixNano(),
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	}
	data, err := json.Marshal(reqBody)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+defaultSandboxMCPPath, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("sandbox mcp status=%d body=%s", resp.StatusCode, string(body))
	}
	var decoded struct {
		Result json.RawMessage `json:"result"`
		Error  *mcpRPCError    `json:"error"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		return "", err
	}
	if decoded.Error != nil {
		return "", fmt.Errorf("sandbox mcp rpc error %d: %s", decoded.Error.Code, decoded.Error.Message)
	}
	return formatMCPToolResult(decoded.Result), nil
}

func IsSandboxRequiredToolCall(name string, input map[string]any) bool {
	switch name {
	case "write", "edit":
		return true
	case "bash":
		return true
	default:
		return false
	}
}

func IsDangerousShellCommand(command string) bool {
	command = normalizeShellCommand(command)
	if command == "" {
		return false
	}
	lower := strings.ToLower(command)
	if hasShellWriteOperation(lower) {
		return true
	}
	dangerTokens := []string{
		"curl ", "curl\t", " curl ",
		"wget ", " wget ",
		"python ", "python3 ", "node ", "npm ", "npx ",
		"pip ", "pip3 ", "pnpm ", "yarn ",
		"sh ", "bash ",
	}
	padded := " " + lower + " "
	for _, token := range dangerTokens {
		if strings.Contains(padded, token) || strings.HasPrefix(lower, strings.TrimSpace(token)+" ") {
			return true
		}
	}
	return strings.Contains(lower, "| bash") || strings.Contains(lower, "| sh")
}

func sandboxEditCommand(input map[string]any) string {
	data, _ := json.Marshal(input)
	encoded := base64.StdEncoding.EncodeToString(data)
	script := `python3 - <<'PY'
import base64, json, pathlib, sys
payload = json.loads(base64.b64decode("` + encoded + `").decode())
path = pathlib.Path(payload.get("filePath") or payload.get("file_path") or "")
mode = payload.get("mode") or "replace"
old = payload.get("oldText") or payload.get("old_string") or ""
new = payload.get("newText") or payload.get("new_string") or ""
replace_all = bool(payload.get("replaceAll"))
occurrence = payload.get("occurrence")
if not path or not old:
    raise SystemExit("filePath and oldText are required")
text = path.read_text()
count = text.count(old)
if count == 0:
    raise SystemExit("oldText not found")
if replace_all and occurrence:
    raise SystemExit("occurrence cannot be used when replaceAll is true")
if not replace_all and not occurrence and count > 1:
    raise SystemExit(f"oldText appears {count} times; set replaceAll to true or provide a more specific oldText")
def replacement():
    if mode == "delete":
        return ""
    if mode == "insert_before":
        return new + old
    if mode == "insert_after":
        return old + new
    if mode == "replace":
        return new
    raise SystemExit("unsupported edit mode: " + mode)
if replace_all:
    text = text.replace(old, replacement())
    changed = count
else:
    n = int(occurrence or 1)
    if n < 1 or n > count:
        raise SystemExit(f"occurrence {n} is out of range; oldText appears {count} times")
    start = -1
    offset = 0
    for _ in range(n):
        start = text.index(old, offset)
        offset = start + len(old)
    text = text[:start] + replacement() + text[start + len(old):]
    changed = 1
path.write_text(text)
print(f"成功编辑文件: {path}，编辑 {changed} 处")
PY`
	return script
}

func stringValue(input map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, ok := input[key].(string); ok {
			return value
		}
	}
	return ""
}
