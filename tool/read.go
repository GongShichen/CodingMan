package tool

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type ReadTool struct {
	BaseTool
}

func NewReadTool() *ReadTool {
	return &ReadTool{
		BaseTool: BaseTool{
			name:        "read",
			description: "读取文件内容\n支持制定偏移量和行数限制，用于读取大文件的部分内容\n返回的内容带行号",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{
						"type":        "string",
						"description": "要读取的文件路径",
					},
					"offset": map[string]any{
						"type":        "integer",
						"description": "从第几行开始读（默认1，即第一行）",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "最多读取多少行，默认1行",
					},
				},
				"required": []string{"filePath"},
			},
		},
	}
}

func (t *ReadTool) Call(input map[string]any) (string, error) {
	filePath, ok := input["filePath"].(string)
	if !ok || filePath == "" {
		return "", errors.New("filePath is illegal")
	}

	offset, err := parsePositiveInt(input["offset"], 1)
	if err != nil {
		return "", err
	}
	limit, err := parsePositiveInt(input["limit"], 1)
	if err != nil {
		return "", err
	}

	file, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	scanner := bufio.NewScanner(strings.NewReader(string(file)))
	currentLine := 0
	results := make([]string, 0, limit)

	for scanner.Scan() {
		currentLine++
		if currentLine < offset {
			continue
		}
		if len(results) >= limit {
			break
		}

		results = append(results, fmt.Sprintf("line:%d\t%s", currentLine, scanner.Text()))
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return strings.Join(results, "\n"), nil
}

func parsePositiveInt(value any, defaultValue int) (int, error) {
	if value == nil {
		return defaultValue, nil
	}

	switch v := value.(type) {
	case int:
		if v < 1 {
			return defaultValue, nil
		}
		return v, nil
	case int64:
		if v < 1 {
			return defaultValue, nil
		}
		return int(v), nil
	case float64:
		if v < 1 {
			return defaultValue, nil
		}
		return int(v), nil
	case string:
		if v == "" {
			return defaultValue, nil
		}
		parsed, err := strconv.Atoi(v)
		if err != nil {
			return 0, errors.New("value must be an integer")
		}
		if parsed < 1 {
			return defaultValue, nil
		}
		return parsed, nil
	default:
		return 0, errors.New("value must be an integer")
	}
}
