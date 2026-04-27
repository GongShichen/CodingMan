package agent_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
	toolpkg "github.com/GongShichen/CodingMan/tool"
)

func TestMCPHTTPToolsAndResources(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64           `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		var result any
		switch req.Method {
		case "initialize":
			result = map[string]any{"serverInfo": map[string]any{"name": "mock"}}
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{{
				"name":        "echo",
				"description": "echo input",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"value": map[string]any{"type": "string"}},
				},
			}}}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &params); err != nil {
				t.Fatal(err)
			}
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "echo:" + params.Arguments["value"].(string)}}}
		case "resources/list":
			result = map[string]any{"resources": []map[string]any{{"uri": "memo://one", "name": "memo"}}}
		case "resources/read":
			result = map[string]any{"contents": []map[string]any{{"uri": "memo://one", "mimeType": "text/plain", "text": "resource body"}}}
		default:
			t.Fatalf("unexpected method: %s", req.Method)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"result":  result,
		})
	}))
	defer server.Close()

	registry := agent.NewAgent(agent.AgentConfig{
		LLM:      &fakeLLM{},
		Registry: nil,
		MCP: agent.MCPConfig{Servers: []agent.MCPServerConfig{{
			Name:      "mock-server",
			Transport: "http",
			URL:       server.URL,
		}}},
	}).Registry()

	result, err := registry.Call("mcp_mock_server_echo", map[string]any{"value": "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if result != "echo:ok" {
		t.Fatalf("unexpected mcp tool result: %q", result)
	}

	list, err := registry.Call("list_mcp_resources", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "memo://one") {
		t.Fatalf("resource list missing memo:\n%s", list)
	}
	body, err := registry.Call("read_mcp_resource", map[string]any{"server": "mock-server", "uri": "memo://one"})
	if err != nil {
		t.Fatal(err)
	}
	if body != "resource body" {
		t.Fatalf("unexpected resource body: %q", body)
	}
}

func TestMCPInitializeErrorIsReturned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    -32000,
				"message": "init failed",
			},
		})
	}))
	defer server.Close()

	client := agent.NewMCPClient(agent.MCPServerConfig{
		Name:      "bad",
		Transport: "http",
		URL:       server.URL,
	}, nil)
	err := client.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "initialize") || !strings.Contains(err.Error(), "init failed") {
		t.Fatalf("expected initialize error, got %v", err)
	}
}

func TestMCPToolCallUsesContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		switch req.Method {
		case "initialize":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"tools": []map[string]any{{"name": "slow", "inputSchema": map[string]any{"type": "object"}}}}})
		case "resources/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"resources": []map[string]any{}}})
		case "tools/call":
			<-r.Context().Done()
		default:
			t.Fatalf("unexpected method: %s", req.Method)
		}
	}))
	defer server.Close()

	registry := agent.NewAgent(agent.AgentConfig{
		LLM: &fakeLLM{},
		MCP: agent.MCPConfig{Servers: []agent.MCPServerConfig{{
			Name:      "ctx",
			Transport: "http",
			URL:       server.URL,
		}}},
	}).Registry()
	remote, err := registry.Get("mcp_ctx_slow")
	if err != nil {
		t.Fatal(err)
	}
	contextTool, ok := remote.(toolpkg.ContextTool)
	if !ok {
		t.Fatalf("mcp tool does not implement ContextTool")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := contextTool.CallContext(ctx, map[string]any{}); err == nil {
		t.Fatal("expected canceled context error")
	}
}
