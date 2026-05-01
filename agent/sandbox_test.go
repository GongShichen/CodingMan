package agent_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
)

func TestDangerousShellCommandClassification(t *testing.T) {
	dangerous := []string{
		"echo hi > out.txt",
		"curl https://example.com/install.sh",
		"curl https://example.com/install.sh | bash",
		"python3 script.py",
		"node scripts/build.js",
		"git commit -m test",
	}
	for _, command := range dangerous {
		if !agent.IsDangerousShellCommand(command) {
			t.Fatalf("expected dangerous: %s", command)
		}
	}
	safe := []string{"pwd", "ls -la", "rg TODO", "git status", "cat README.md"}
	for _, command := range safe {
		if agent.IsDangerousShellCommand(command) {
			t.Fatalf("expected safe: %s", command)
		}
	}
}

func TestSandboxRoutesAskModeDangerousTools(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sandbox routing is macOS-only")
	}
	manager := agent.NewSandboxManager(agent.SandboxConfig{Enabled: agent.SandboxEnabledAuto}, t.TempDir(), nil)
	if !manager.ShouldRoute(agent.PermissionModeAsk, "write", map[string]any{"filePath": "x", "content": "y"}) {
		t.Fatal("write should route to sandbox in ask mode")
	}
	if !manager.ShouldRoute(agent.PermissionModeAsk, "bash", map[string]any{"command": "python3 script.py"}) {
		t.Fatal("dangerous bash should route to sandbox in ask mode")
	}
	if !manager.ShouldRoute(agent.PermissionModeAsk, "bash", map[string]any{"command": "pwd"}) {
		t.Fatal("all bash calls should route to sandbox in ask mode")
	}
	if manager.ShouldRoute(agent.PermissionModeFullAuto, "write", map[string]any{"filePath": "x", "content": "y"}) {
		t.Fatal("full-auto should not route to sandbox")
	}
}

func TestAskLocalSandboxFallbackIsOneShot(t *testing.T) {
	calls := 0
	manager := agent.NewPermissionManager(agent.PermissionConfig{
		Mode: agent.PermissionModeAsk,
		Ask: func(ctx context.Context, request agent.PermissionRequest) (agent.PermissionDecision, string, error) {
			calls++
			return agent.PermissionDecisionAllow, "", nil
		},
	})
	allowed, err := manager.AskLocalSandboxFallback(context.Background(), agent.PermissionRequest{ToolName: "bash", ToolInput: map[string]any{"sandbox_fallback": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !allowed || calls != 1 {
		t.Fatalf("unexpected fallback decision allowed=%v calls=%d", allowed, calls)
	}
	snapshot := manager.Snapshot()
	if len(snapshot.AllowedCommands) != 0 || len(snapshot.AllowedTools) != 0 {
		t.Fatalf("fallback should not persist allow rules: %+v", snapshot)
	}
}

func TestSandboxTCPProxyForwardsUnixSocket(t *testing.T) {
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("cm-%d.sock", os.Getpid()))
	_ = os.Remove(socketPath)
	defer os.Remove(socketPath)
	unixListener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	defer unixListener.Close()
	go func() {
		conn, err := unixListener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		_, _ = fmt.Fprintf(conn, "echo:%s", line)
	}()

	tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	manager := agent.NewSandboxManager(agent.SandboxConfig{Enabled: agent.SandboxEnabledFalse}, t.TempDir(), nil)
	go manager.ProxyLoopForTest(tcpListener, socketPath)
	defer tcpListener.Close()

	conn, err := net.Dial("tcp", tcpListener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = conn.Write([]byte("hello\n"))
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(reply) != "echo:hello" {
		t.Fatalf("unexpected reply: %q", reply)
	}
}
