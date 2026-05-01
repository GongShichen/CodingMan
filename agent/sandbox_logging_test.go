package agent

import (
	"fmt"
	"strings"
	"testing"
)

type recordingLogger struct {
	lines []string
}

func (logger *recordingLogger) Log(traceID string, format string, args ...any) {
	logger.lines = append(logger.lines, traceID+" "+formatLog(format, args...))
}

func formatLog(format string, args ...any) string {
	if len(args) == 0 {
		return format
	}
	return strings.TrimSpace(strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " "))
}

func TestSandboxLogWriterForwardsLines(t *testing.T) {
	logger := &recordingLogger{}
	writer := &sandboxLogWriter{logger: logger, traceID: "trace-1", prefix: "sandbox_vfkit stderr:"}
	if _, err := writer.Write([]byte("first line\nsecond")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(" line\n")); err != nil {
		t.Fatal(err)
	}
	if len(logger.lines) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %#v", len(logger.lines), logger.lines)
	}
	if !strings.Contains(logger.lines[0], "trace-1 sandbox_vfkit stderr: first line") {
		t.Fatalf("unexpected first line: %q", logger.lines[0])
	}
	if !strings.Contains(logger.lines[1], "trace-1 sandbox_vfkit stderr: second line") {
		t.Fatalf("unexpected second line: %q", logger.lines[1])
	}
}
