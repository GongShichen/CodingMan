package tool

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestBashToolCallContextCancelsCommand(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := NewBashTool().CallContext(ctx, map[string]any{
		"command": "sleep 1",
		"timeout": 5000,
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected context canceled, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("cancelled command took too long: %s", elapsed)
	}
}
