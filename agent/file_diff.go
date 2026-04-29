package agent

import (
	"fmt"
	"os"
	"strings"
)

type fileSnapshot struct {
	path    string
	exists  bool
	content string
}

func captureFileSnapshot(input map[string]any) fileSnapshot {
	path, _ := stringFromMap(input, "path", "file_path", "filePath")
	if strings.TrimSpace(path) == "" {
		return fileSnapshot{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fileSnapshot{path: path}
	}
	return fileSnapshot{path: path, exists: true, content: string(data)}
}

func unifiedFileDiff(path string, before string, after string) string {
	if before == after {
		return ""
	}
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("--- %s\n", path))
	builder.WriteString(fmt.Sprintf("+++ %s\n", path))
	beforeLines := splitDiffLines(before)
	afterLines := splitDiffLines(after)
	for _, op := range diffLines(beforeLines, afterLines) {
		switch op.kind {
		case diffEqual:
			builder.WriteString(" ")
		case diffDelete:
			builder.WriteString("-")
		case diffInsert:
			builder.WriteString("+")
		}
		builder.WriteString(op.line)
		if !strings.HasSuffix(op.line, "\n") {
			builder.WriteString("\n")
		}
	}
	return builder.String()
}

func splitDiffLines(content string) []string {
	if content == "" {
		return nil
	}
	parts := strings.SplitAfter(content, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}

type diffKind int

const (
	diffEqual diffKind = iota
	diffDelete
	diffInsert
)

type diffOp struct {
	kind diffKind
	line string
}

func diffLines(a []string, b []string) []diffOp {
	m, n := len(a), len(b)
	table := make([][]int, m+1)
	for i := range table {
		table[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i][j] = table[i+1][j+1] + 1
			} else if table[i+1][j] >= table[i][j+1] {
				table[i][j] = table[i+1][j]
			} else {
				table[i][j] = table[i][j+1]
			}
		}
	}
	var ops []diffOp
	i, j := 0, 0
	for i < m && j < n {
		if a[i] == b[j] {
			ops = append(ops, diffOp{kind: diffEqual, line: a[i]})
			i++
			j++
		} else if table[i+1][j] >= table[i][j+1] {
			ops = append(ops, diffOp{kind: diffDelete, line: a[i]})
			i++
		} else {
			ops = append(ops, diffOp{kind: diffInsert, line: b[j]})
			j++
		}
	}
	for i < m {
		ops = append(ops, diffOp{kind: diffDelete, line: a[i]})
		i++
	}
	for j < n {
		ops = append(ops, diffOp{kind: diffInsert, line: b[j]})
		j++
	}
	return ops
}
