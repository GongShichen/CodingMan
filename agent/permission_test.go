package agent_test

import (
	"context"
	"testing"

	"github.com/GongShichen/CodingMan/agent"
)

func TestDefaultPermissionAllowsWebSearch(t *testing.T) {
	manager := agent.NewPermissionManager(agent.DefaultPermissionConfig())
	check, err := manager.CheckWithResult(context.Background(), agent.PermissionRequest{
		ToolName: "websearch",
		ToolInput: map[string]any{
			"query": "golang",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !check.ParallelSafe {
		t.Fatal("websearch should be parallel safe")
	}
}
