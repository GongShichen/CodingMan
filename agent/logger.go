package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

type Logger interface {
	Log(traceID string, format string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Log(string, string, ...any) {}

type FileLogger struct {
	mu   sync.Mutex
	file *os.File
}

func NewFileLogger(path string) (*FileLogger, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	return &FileLogger{file: file}, nil
}

func (logger *FileLogger) Log(traceID string, format string, args ...any) {
	if logger == nil || logger.file == nil {
		return
	}
	if traceID == "" {
		traceID = "-"
	}
	line := fmt.Sprintf(format, args...)
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")

	logger.mu.Lock()
	defer logger.mu.Unlock()
	_, _ = fmt.Fprintf(logger.file, "[%s][%s] %s\n", timestamp, traceID, line)
}

func (logger *FileLogger) Close() error {
	if logger == nil || logger.file == nil {
		return nil
	}
	logger.mu.Lock()
	defer logger.mu.Unlock()
	return logger.file.Close()
}

type traceContextKey struct{}

func NewTraceID() string {
	var data [8]byte
	if _, err := rand.Read(data[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data[:])
}

func WithTraceID(ctx context.Context, traceID string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if traceID == "" {
		traceID = NewTraceID()
	}
	return context.WithValue(ctx, traceContextKey{}, traceID)
}

func TraceIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	traceID, _ := ctx.Value(traceContextKey{}).(string)
	return traceID
}

func ensureTrace(ctx context.Context) (context.Context, string) {
	if ctx == nil {
		ctx = context.Background()
	}
	traceID := TraceIDFromContext(ctx)
	if traceID != "" {
		return ctx, traceID
	}
	traceID = NewTraceID()
	return WithTraceID(ctx, traceID), traceID
}

func formatContentForLog(text string, blocks []ContentBlock) string {
	var builder strings.Builder
	if text != "" {
		builder.WriteString(text)
	}
	for _, block := range blocks {
		if builder.Len() > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(formatBlockForLog(block))
	}
	if builder.Len() == 0 {
		return "<empty>"
	}
	return builder.String()
}

func formatAssistantResponseForLog(resp LLMResponse) string {
	blocks := make([]ContentBlock, 0, 1+len(resp.ToolUses))
	if resp.Content != "" {
		blocks = append(blocks, TextBlock(resp.Content))
	}
	for _, toolUse := range resp.ToolUses {
		blocks = append(blocks, ToolUseBlock(toolUse.ID, toolUse.Name, toolUse.Input))
	}
	if len(blocks) == 0 {
		return "<empty>"
	}

	var builder strings.Builder
	for i, block := range blocks {
		if i > 0 {
			builder.WriteString("\n")
		}
		builder.WriteString(formatBlockForLog(block))
	}
	return builder.String()
}

func formatBlockForLog(block ContentBlock) string {
	switch block.Type {
	case ContentTypeText:
		return block.Text
	case ContentTypeImage:
		if block.ImageURL != "" {
			return "[image url]"
		}
		return fmt.Sprintf("[image base64] media_type=%s", block.MediaType)
	case ContentTypeToolUse:
		return fmt.Sprintf("[tool_use] id=%s name=%s input=%s", block.ToolID, block.ToolName, block.ToolInput)
	case ContentTypeToolResult:
		return fmt.Sprintf("[tool_result] id=%s error=%v content=%s", block.ToolID, block.IsError, block.Text)
	default:
		return fmt.Sprintf("[%s]", block.Type)
	}
}
