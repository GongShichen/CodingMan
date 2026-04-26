package toolUse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type WriteTool struct {
	BaseTool
}

func NewWriteTool() *WriteTool {
	return &WriteTool{
		BaseTool: BaseTool{
			name:        "write",
			description: "写入文件内容。\n默认创建新文件，如果文件已存在会报错，不会覆盖。\n当 overwrite 为 true 时允许覆盖已有文件。\n会自动创建所需的父目录",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{
						"type":        "string",
						"description": "要写入的路径",
					},
					"content": map[string]any{
						"type":        "string",
						"description": "要写入的内容",
					},
					"overwrite": map[string]any{
						"type":        "boolean",
						"description": "是否允许覆盖已存在文件，默认 false。",
					},
				},
				"required": []string{"filePath", "content"},
			},
		},
	}
}

func (tool *WriteTool) Call(input map[string]any) (string, error) {
	filePath, ok := input["filePath"].(string)
	if !ok || filePath == "" {
		return "", errors.New("file path required")
	}
	content, ok := input["content"].(string)
	if !ok {
		return "", errors.New("content required")
	}
	overwrite, ok := input["overwrite"].(bool)
	if !ok {
		overwrite = false
	}
	dir := filepath.Dir(filePath)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	flag := os.O_WRONLY | os.O_CREATE
	if overwrite {
		flag |= os.O_TRUNC
	} else {
		flag |= os.O_EXCL
	}

	file, err := os.OpenFile(filePath, flag, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return "", errors.New("file already exists")
		}
		return "", err
	}
	defer file.Close()

	if _, err := file.WriteString(content); err != nil {
		return "", err
	}

	if overwrite {
		return fmt.Sprintf("成功写入文件: %s", filePath), nil
	}
	return fmt.Sprintf("成功创建文件: %s", filePath), nil
}
