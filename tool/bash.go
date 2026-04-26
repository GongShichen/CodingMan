package tool

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

const defaultBashTimeoutMs = 120000

type BashTool struct {
	BaseTool
}

func NewBashTool() *BashTool {
	return &BashTool{
		BaseTool: NewBaseTool(
			"bash",
			"执行 shell 命令。命令会在 bash -c 中执行，支持管道、重定向等 shell 特性。默认超时为 120 秒。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"command": map[string]any{
						"type":        "string",
						"description": "要执行的 shell 命令。",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "超时时间，单位毫秒，默认 120000。",
					},
					"cwd": map[string]any{
						"type":        "string",
						"description": "工作目录，默认当前目录。",
					},
				},
				"required": []string{"command"},
			},
		),
	}
}

func (b *BashTool) Call(input map[string]any) (string, error) {
	command, ok := input["command"].(string)
	if !ok || command == "" {
		return "", errors.New("command is required")
	}

	timeout := parseTimeoutMs(input["timeout"])
	cwd, _ := input["cwd"].(string)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(ctx, "bash", "-c", command)
	if cwd != "" {
		cmd.Dir = cwd
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return formatBashOutput(stdout.String(), stderr.String()), fmt.Errorf("command timed out after %d ms", timeout)
	}
	if err != nil {
		return formatBashOutput(stdout.String(), stderr.String()), err
	}

	return formatBashOutput(stdout.String(), stderr.String()), nil
}

func parseTimeoutMs(value any) int {
	switch v := value.(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}

	return defaultBashTimeoutMs
}

func formatBashOutput(stdout string, stderr string) string {
	return fmt.Sprintf("stdout:\n%s\nstderr:\n%s", stdout, stderr)
}
