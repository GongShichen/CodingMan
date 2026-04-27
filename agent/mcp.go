package agent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tool "github.com/GongShichen/CodingMan/tool"
)

type MCPConfig struct {
	Servers []MCPServerConfig `json:"mcp_servers"`
}

type MCPServerConfig struct {
	Name              string            `json:"name"`
	Transport         string            `json:"transport"`
	Command           string            `json:"command,omitempty"`
	Args              []string          `json:"args,omitempty"`
	Env               map[string]string `json:"env,omitempty"`
	URL               string            `json:"url,omitempty"`
	HeartbeatInterval string            `json:"heartbeat_interval,omitempty"`
	ReconnectInterval string            `json:"reconnect_interval,omitempty"`
}

type MCPManager struct {
	mu      sync.Mutex
	clients map[string]*MCPClient
	logger  Logger
}

type MCPClient struct {
	mu                sync.Mutex
	config            MCPServerConfig
	transport         mcpTransport
	tools             []mcpTool
	resources         []MCPResource
	logger            Logger
	id                atomic.Int64
	closed            chan struct{}
	heartbeatInterval time.Duration
	reconnectInterval time.Duration
}

type MCPResource struct {
	Server      string `json:"server"`
	URI         string `json:"uri"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mime_type,omitempty"`
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema,omitempty"`
}

type mcpJSONRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type mcpJSONRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *mcpRPCError    `json:"error,omitempty"`
}

type mcpRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpTransport interface {
	Start(context.Context) error
	Request(context.Context, mcpJSONRPCRequest) (mcpJSONRPCResponse, error)
	Close() error
}

func NewMCPManager(config MCPConfig, logger Logger) (*MCPManager, error) {
	if logger == nil {
		logger = noopLogger{}
	}
	manager := &MCPManager{clients: make(map[string]*MCPClient), logger: logger}
	for _, server := range config.Servers {
		server.Name = sanitizeMCPName(server.Name)
		if server.Name == "" {
			return nil, errors.New("mcp server name is required")
		}
		if _, exists := manager.clients[server.Name]; exists {
			return nil, fmt.Errorf("duplicate mcp server: %s", server.Name)
		}
		client := NewMCPClient(server, logger)
		manager.clients[server.Name] = client
	}
	return manager, nil
}

func NewMCPClient(config MCPServerConfig, logger Logger) *MCPClient {
	if logger == nil {
		logger = noopLogger{}
	}
	heartbeat := parseMCPDuration(config.HeartbeatInterval, 30*time.Second)
	reconnect := parseMCPDuration(config.ReconnectInterval, 3*time.Second)
	return &MCPClient{
		config:            config,
		logger:            logger,
		closed:            make(chan struct{}),
		heartbeatInterval: heartbeat,
		reconnectInterval: reconnect,
	}
}

func (manager *MCPManager) Start(ctx context.Context, registry *tool.Registry) {
	if manager == nil || registry == nil {
		return
	}
	manager.mu.Lock()
	clients := make([]*MCPClient, 0, len(manager.clients))
	for _, client := range manager.clients {
		clients = append(clients, client)
	}
	manager.mu.Unlock()

	_ = registry.Register(newListMCPResourcesTool(manager))
	_ = registry.Register(newReadMCPResourceTool(manager))
	for _, client := range clients {
		if err := client.Connect(ctx); err != nil {
			manager.logger.Log(TraceIDFromContext(ctx), "mcp server=%s connect_error=%v", client.config.Name, err)
			go client.reconnectLoop(ctx, registry)
			continue
		}
		client.registerRemoteTools(registry)
		go client.heartbeatLoop(ctx, registry)
	}
}

func (manager *MCPManager) Close() error {
	if manager == nil {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	var errs []string
	for _, client := range manager.clients {
		if err := client.Close(); err != nil {
			errs = append(errs, err.Error())
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func (manager *MCPManager) ListResources(ctx context.Context) ([]MCPResource, error) {
	if manager == nil {
		return nil, nil
	}
	manager.mu.Lock()
	clients := make([]*MCPClient, 0, len(manager.clients))
	for _, client := range manager.clients {
		clients = append(clients, client)
	}
	manager.mu.Unlock()
	var resources []MCPResource
	var errs []string
	for _, client := range clients {
		list, err := client.ListResources(ctx)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", client.config.Name, err))
			continue
		}
		resources = append(resources, list...)
	}
	if len(errs) > 0 {
		return resources, errors.New(strings.Join(errs, "; "))
	}
	return resources, nil
}

func (manager *MCPManager) ReadResource(ctx context.Context, server string, uri string) (string, error) {
	if manager == nil {
		return "", errors.New("mcp manager is nil")
	}
	manager.mu.Lock()
	client := manager.clients[sanitizeMCPName(server)]
	manager.mu.Unlock()
	if client == nil {
		return "", fmt.Errorf("mcp server not found: %s", server)
	}
	return client.ReadResource(ctx, uri)
}

func (client *MCPClient) Connect(ctx context.Context) error {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.transport != nil {
		_ = client.transport.Close()
		client.transport = nil
	}
	transport, err := newMCPTransport(client.config)
	if err != nil {
		return err
	}
	if err := transport.Start(ctx); err != nil {
		return err
	}
	client.transport = transport
	if _, err := client.callLocked(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"clientInfo": map[string]any{
			"name":    "CodingMan",
			"version": "0.1.0",
		},
		"capabilities": map[string]any{},
	}); err != nil {
		return fmt.Errorf("initialize: %w", err)
	}
	if err := client.refreshToolsLocked(ctx); err != nil {
		return err
	}
	_ = client.refreshResourcesLocked(ctx)
	return nil
}

func (client *MCPClient) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()
	select {
	case <-client.closed:
	default:
		close(client.closed)
	}
	if client.transport != nil {
		return client.transport.Close()
	}
	return nil
}

func (client *MCPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	return client.callLocked(ctx, method, params)
}

func (client *MCPClient) callLocked(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if client.transport == nil {
		return nil, errors.New("mcp transport is not connected")
	}
	req := mcpJSONRPCRequest{
		JSONRPC: "2.0",
		ID:      client.id.Add(1),
		Method:  method,
		Params:  params,
	}
	resp, err := client.transport.Request(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("mcp rpc error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

func (client *MCPClient) refreshToolsLocked(ctx context.Context) error {
	raw, err := client.callLocked(ctx, "tools/list", map[string]any{})
	if err != nil {
		return err
	}
	var decoded struct {
		Tools []mcpTool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	client.tools = decoded.Tools
	return nil
}

func (client *MCPClient) refreshResourcesLocked(ctx context.Context) error {
	raw, err := client.callLocked(ctx, "resources/list", map[string]any{})
	if err != nil {
		return err
	}
	var decoded struct {
		Resources []MCPResource `json:"resources"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return err
	}
	for i := range decoded.Resources {
		decoded.Resources[i].Server = client.config.Name
	}
	client.resources = decoded.Resources
	return nil
}

func (client *MCPClient) registerRemoteTools(registry *tool.Registry) {
	client.mu.Lock()
	tools := append([]mcpTool(nil), client.tools...)
	server := client.config.Name
	client.mu.Unlock()
	used := make(map[string]struct{}, len(tools))
	for _, remoteTool := range tools {
		name := mcpToolName(server, remoteTool.Name)
		if _, exists := used[name]; exists {
			client.logger.Log("", "mcp server=%s tool_name_collision remote=%s generated=%s", server, remoteTool.Name, name)
			name = name + "_" + shortMCPHash(remoteTool.Name)
		}
		used[name] = struct{}{}
		registry.Unregister(name)
		if err := registry.Register(&mcpRemoteTool{client: client, server: server, remote: remoteTool, name: name}); err != nil {
			client.logger.Log("", "mcp server=%s register_tool_error remote=%s name=%s error=%v", server, remoteTool.Name, name, err)
		}
	}
}

func (client *MCPClient) ListResources(ctx context.Context) ([]MCPResource, error) {
	client.mu.Lock()
	if err := client.refreshResourcesLocked(ctx); err != nil {
		cached := append([]MCPResource(nil), client.resources...)
		client.mu.Unlock()
		return cached, err
	}
	resources := append([]MCPResource(nil), client.resources...)
	client.mu.Unlock()
	return resources, nil
}

func (client *MCPClient) ReadResource(ctx context.Context, uri string) (string, error) {
	raw, err := client.call(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", err
	}
	var decoded struct {
		Contents []struct {
			URI      string `json:"uri"`
			MimeType string `json:"mimeType"`
			Text     string `json:"text"`
			Blob     string `json:"blob"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", err
	}
	var builder strings.Builder
	for _, content := range decoded.Contents {
		if content.Text != "" {
			builder.WriteString(content.Text)
		} else if content.Blob != "" {
			builder.WriteString("[binary resource omitted: ")
			builder.WriteString(content.MimeType)
			builder.WriteString("]")
		}
		if !strings.HasSuffix(builder.String(), "\n") {
			builder.WriteString("\n")
		}
	}
	return strings.TrimSpace(builder.String()), nil
}

func (client *MCPClient) heartbeatLoop(ctx context.Context, registry *tool.Registry) {
	ticker := time.NewTicker(client.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if _, err := client.call(ctx, "tools/list", map[string]any{}); err != nil {
				client.logger.Log(TraceIDFromContext(ctx), "mcp server=%s heartbeat_error=%v", client.config.Name, err)
				client.reconnectLoop(ctx, registry)
				return
			}
		case <-client.closed:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (client *MCPClient) reconnectLoop(ctx context.Context, registry *tool.Registry) {
	ticker := time.NewTicker(client.reconnectInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := client.Connect(ctx); err != nil {
				client.logger.Log(TraceIDFromContext(ctx), "mcp server=%s reconnect_error=%v", client.config.Name, err)
				continue
			}
			client.logger.Log(TraceIDFromContext(ctx), "mcp server=%s reconnected", client.config.Name)
			client.registerRemoteTools(registry)
			go client.heartbeatLoop(ctx, registry)
			return
		case <-client.closed:
			return
		case <-ctx.Done():
			return
		}
	}
}

type mcpRemoteTool struct {
	client *MCPClient
	server string
	remote mcpTool
	name   string
}

func (tool *mcpRemoteTool) Name() string { return tool.name }
func (tool *mcpRemoteTool) Description() string {
	return fmt.Sprintf("MCP tool %s from server %s. Remote tool: %s", tool.remote.Name, tool.server, tool.remote.Description)
}
func (tool *mcpRemoteTool) InputSchema() map[string]any {
	if tool.remote.InputSchema != nil {
		return tool.remote.InputSchema
	}
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (tool *mcpRemoteTool) ToAPIFormat() map[string]any {
	return map[string]any{"name": tool.Name(), "description": tool.Description(), "input_schema": tool.InputSchema()}
}
func (tool *mcpRemoteTool) Call(input map[string]any) (string, error) {
	return tool.CallContext(context.Background(), input)
}

func (tool *mcpRemoteTool) CallContext(ctx context.Context, input map[string]any) (string, error) {
	raw, err := tool.client.call(ctx, "tools/call", map[string]any{
		"name":      tool.remote.Name,
		"arguments": input,
	})
	if err != nil {
		return "", err
	}
	return formatMCPToolResult(raw), nil
}

type listMCPResourcesTool struct {
	manager *MCPManager
}

func newListMCPResourcesTool(manager *MCPManager) tool.Tool {
	return &listMCPResourcesTool{manager: manager}
}
func (tool *listMCPResourcesTool) Name() string { return "list_mcp_resources" }
func (tool *listMCPResourcesTool) Description() string {
	return "List resources exposed by configured MCP servers."
}
func (tool *listMCPResourcesTool) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (tool *listMCPResourcesTool) ToAPIFormat() map[string]any {
	return map[string]any{"name": tool.Name(), "description": tool.Description(), "input_schema": tool.InputSchema()}
}
func (tool *listMCPResourcesTool) Call(input map[string]any) (string, error) {
	return tool.CallContext(context.Background(), input)
}

func (tool *listMCPResourcesTool) CallContext(ctx context.Context, input map[string]any) (string, error) {
	resources, err := tool.manager.ListResources(ctx)
	data, marshalErr := json.MarshalIndent(resources, "", "  ")
	if err != nil {
		if marshalErr != nil {
			return "", err
		}
		return string(data), err
	}
	if marshalErr != nil {
		return "", marshalErr
	}
	return string(data), nil
}

type readMCPResourceTool struct {
	manager *MCPManager
}

func newReadMCPResourceTool(manager *MCPManager) tool.Tool {
	return &readMCPResourceTool{manager: manager}
}
func (tool *readMCPResourceTool) Name() string { return "read_mcp_resource" }
func (tool *readMCPResourceTool) Description() string {
	return "Read a resource exposed by an MCP server."
}
func (tool *readMCPResourceTool) InputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []string{"server", "uri"},
		"properties": map[string]any{
			"server": map[string]any{"type": "string"},
			"uri":    map[string]any{"type": "string"},
		},
	}
}
func (tool *readMCPResourceTool) ToAPIFormat() map[string]any {
	return map[string]any{"name": tool.Name(), "description": tool.Description(), "input_schema": tool.InputSchema()}
}
func (tool *readMCPResourceTool) Call(input map[string]any) (string, error) {
	return tool.CallContext(context.Background(), input)
}

func (tool *readMCPResourceTool) CallContext(ctx context.Context, input map[string]any) (string, error) {
	server, _ := input["server"].(string)
	uri, _ := input["uri"].(string)
	return tool.manager.ReadResource(ctx, server, uri)
}

func formatMCPToolResult(raw json.RawMessage) string {
	var decoded struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Data string `json:"data"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &decoded); err == nil && len(decoded.Content) > 0 {
		var builder strings.Builder
		for _, item := range decoded.Content {
			if item.Text != "" {
				builder.WriteString(item.Text)
			} else if item.Data != "" {
				builder.WriteString("[")
				builder.WriteString(item.Type)
				builder.WriteString(" content omitted]")
			}
			builder.WriteString("\n")
		}
		return strings.TrimSpace(builder.String())
	}
	return string(raw)
}

func newMCPTransport(config MCPServerConfig) (mcpTransport, error) {
	switch strings.ToLower(strings.TrimSpace(config.Transport)) {
	case "", "stdio":
		return &mcpStdioTransport{config: config}, nil
	case "http", "sse":
		return &mcpHTTPTransport{url: config.URL, client: http.DefaultClient}, nil
	case "websocket", "ws":
		return &mcpWebSocketTransport{rawURL: config.URL}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp transport: %s", config.Transport)
	}
}

type mcpHTTPTransport struct {
	url    string
	client *http.Client
}

func (transport *mcpHTTPTransport) Start(ctx context.Context) error {
	if strings.TrimSpace(transport.url) == "" {
		return errors.New("mcp http url is required")
	}
	return nil
}

func (transport *mcpHTTPTransport) Request(ctx context.Context, req mcpJSONRPCRequest) (mcpJSONRPCResponse, error) {
	data, err := json.Marshal(req)
	if err != nil {
		return mcpJSONRPCResponse{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, transport.url, bytes.NewReader(data))
	if err != nil {
		return mcpJSONRPCResponse{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := transport.client.Do(httpReq)
	if err != nil {
		return mcpJSONRPCResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return mcpJSONRPCResponse{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return mcpJSONRPCResponse{}, fmt.Errorf("mcp http status=%d body=%s", resp.StatusCode, string(body))
	}
	body = decodeSSEBody(body)
	var decoded mcpJSONRPCResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return mcpJSONRPCResponse{}, err
	}
	return decoded, nil
}

func (transport *mcpHTTPTransport) Close() error { return nil }

type mcpStdioTransport struct {
	config MCPServerConfig
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	reader *bufio.Reader
	mu     sync.Mutex
}

func (transport *mcpStdioTransport) Start(ctx context.Context) error {
	if strings.TrimSpace(transport.config.Command) == "" {
		return errors.New("mcp stdio command is required")
	}
	cmd := exec.CommandContext(ctx, transport.config.Command, transport.config.Args...)
	cmd.Env = os.Environ()
	for key, value := range transport.config.Env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	transport.cmd = cmd
	transport.stdin = stdin
	transport.reader = bufio.NewReader(stdout)
	return nil
}

func (transport *mcpStdioTransport) Request(ctx context.Context, req mcpJSONRPCRequest) (mcpJSONRPCResponse, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	data, err := json.Marshal(req)
	if err != nil {
		return mcpJSONRPCResponse{}, err
	}
	if _, err := fmt.Fprintf(transport.stdin, "Content-Length: %d\r\n\r\n%s", len(data), data); err != nil {
		return mcpJSONRPCResponse{}, err
	}
	return readMCPHeaderMessage(transport.reader)
}

func (transport *mcpStdioTransport) Close() error {
	if transport.stdin != nil {
		_ = transport.stdin.Close()
	}
	if transport.cmd != nil && transport.cmd.Process != nil {
		_ = transport.cmd.Process.Kill()
	}
	return nil
}

type mcpWebSocketTransport struct {
	rawURL string
	conn   net.Conn
	mu     sync.Mutex
}

func (transport *mcpWebSocketTransport) Start(ctx context.Context) error {
	parsed, err := url.Parse(transport.rawURL)
	if err != nil {
		return err
	}
	host := parsed.Host
	if !strings.Contains(host, ":") {
		if parsed.Scheme == "wss" {
			host += ":443"
		} else {
			host += ":80"
		}
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", host)
	if err != nil {
		return err
	}
	keyBytes := make([]byte, 16)
	_, _ = rand.Read(keyBytes)
	key := base64.StdEncoding.EncodeToString(keyBytes)
	path := parsed.RequestURI()
	if path == "" {
		path = "/"
	}
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Version: 13\r\n\r\n", path, parsed.Host, key)
	if _, err := conn.Write([]byte(req)); err != nil {
		_ = conn.Close()
		return err
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		_ = conn.Close()
		return err
	}
	if !strings.Contains(status, "101") {
		_ = conn.Close()
		return fmt.Errorf("websocket handshake failed: %s", strings.TrimSpace(status))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			_ = conn.Close()
			return err
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	transport.conn = &bufferedConn{Conn: conn, reader: reader}
	return nil
}

func (transport *mcpWebSocketTransport) Request(ctx context.Context, req mcpJSONRPCRequest) (mcpJSONRPCResponse, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	data, err := json.Marshal(req)
	if err != nil {
		return mcpJSONRPCResponse{}, err
	}
	if err := writeWebSocketText(transport.conn, data); err != nil {
		return mcpJSONRPCResponse{}, err
	}
	payload, err := readWebSocketText(transport.conn)
	if err != nil {
		return mcpJSONRPCResponse{}, err
	}
	var decoded mcpJSONRPCResponse
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return mcpJSONRPCResponse{}, err
	}
	return decoded, nil
}

func (transport *mcpWebSocketTransport) Close() error {
	if transport.conn != nil {
		return transport.conn.Close()
	}
	return nil
}

type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (conn *bufferedConn) Read(p []byte) (int, error) {
	return conn.reader.Read(p)
}

func decodeSSEBody(body []byte) []byte {
	trimmed := bytes.TrimSpace(body)
	if bytes.HasPrefix(trimmed, []byte("data:")) {
		var lines []string
		scanner := bufio.NewScanner(bytes.NewReader(trimmed))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "data:") {
				lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		return []byte(strings.Join(lines, "\n"))
	}
	return body
}

func readMCPHeaderMessage(reader *bufio.Reader) (mcpJSONRPCResponse, error) {
	contentLength := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return mcpJSONRPCResponse{}, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			_, _ = fmt.Sscanf(strings.TrimSpace(value), "%d", &contentLength)
		}
	}
	if contentLength <= 0 {
		return mcpJSONRPCResponse{}, errors.New("missing mcp content-length")
	}
	data := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, data); err != nil {
		return mcpJSONRPCResponse{}, err
	}
	var decoded mcpJSONRPCResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return mcpJSONRPCResponse{}, err
	}
	return decoded, nil
}

func writeWebSocketText(conn net.Conn, payload []byte) error {
	header := []byte{0x81}
	length := len(payload)
	maskKey := make([]byte, 4)
	_, _ = rand.Read(maskKey)
	switch {
	case length < 126:
		header = append(header, byte(length)|0x80)
	case length <= 65535:
		header = append(header, 126|0x80, byte(length>>8), byte(length))
	default:
		header = append(header, 127|0x80)
		var lenBytes [8]byte
		binary.BigEndian.PutUint64(lenBytes[:], uint64(length))
		header = append(header, lenBytes[:]...)
	}
	masked := make([]byte, length)
	for i := range payload {
		masked[i] = payload[i] ^ maskKey[i%4]
	}
	_, err := conn.Write(append(append(header, maskKey...), masked...))
	return err
}

func readWebSocketText(conn net.Conn) ([]byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(conn, header[:]); err != nil {
		return nil, err
	}
	opcode := header[0] & 0x0f
	if opcode == 0x8 {
		return nil, io.EOF
	}
	length := int(header[1] & 0x7f)
	if length == 126 {
		var buf [2]byte
		if _, err := io.ReadFull(conn, buf[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint16(buf[:]))
	} else if length == 127 {
		var buf [8]byte
		if _, err := io.ReadFull(conn, buf[:]); err != nil {
			return nil, err
		}
		length = int(binary.BigEndian.Uint64(buf[:]))
	}
	masked := header[1]&0x80 != 0
	var maskKey [4]byte
	if masked {
		if _, err := io.ReadFull(conn, maskKey[:]); err != nil {
			return nil, err
		}
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= maskKey[i%4]
		}
	}
	return payload, nil
}

func parseMCPDuration(value string, fallback time.Duration) time.Duration {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fallback
	}
	return parsed
}

var mcpNamePattern = regexp.MustCompile(`[^a-zA-Z0-9_]+`)

func sanitizeMCPName(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "-", "_")
	value = mcpNamePattern.ReplaceAllString(value, "_")
	return strings.Trim(value, "_")
}

func mcpToolName(server string, remoteTool string) string {
	return "mcp_" + sanitizeMCPName(server) + "_" + sanitizeMCPName(remoteTool)
}

func shortMCPHash(value string) string {
	sum := sha1.Sum([]byte(value))
	return fmt.Sprintf("%x", sum[:4])
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}
