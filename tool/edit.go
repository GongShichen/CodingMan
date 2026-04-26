package tool

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

type EditTool struct {
	BaseTool
}

const (
	editModeReplace      = "replace"
	editModeDelete       = "delete"
	editModeInsertBefore = "insert_before"
	editModeInsertAfter  = "insert_after"
)

func NewEditTool() *EditTool {
	return &EditTool{
		BaseTool: BaseTool{
			name:        "edit",
			description: "通过精确文本锚点编辑已存在文件。\nmode 默认为 replace，可选 replace、delete、insert_before、insert_after。\n默认要求 oldText 在文件中只出现一次，避免误改。\n当 oldText 出现多次且 replaceAll 为 false 时，可以用 occurrence 指定编辑第几处匹配，从 1 开始。\n当 replaceAll 为 true 时会编辑所有匹配项。",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filePath": map[string]any{
						"type":        "string",
						"description": "要编辑的文件路径。",
					},
					"mode": map[string]any{
						"type":        "string",
						"enum":        []string{editModeReplace, editModeDelete, editModeInsertBefore, editModeInsertAfter},
						"description": "编辑模式。replace 替换 oldText；delete 删除 oldText；insert_before 在 oldText 前插入 newText；insert_after 在 oldText 后插入 newText。默认 replace。",
					},
					"oldText": map[string]any{
						"type":        "string",
						"description": "要查找的锚点文本，必须精确匹配。",
					},
					"newText": map[string]any{
						"type":        "string",
						"description": "replace 模式下是替换后的文本；insert_before/insert_after 模式下是要插入的文本；delete 模式下可省略。",
					},
					"replaceAll": map[string]any{
						"type":        "boolean",
						"description": "是否替换所有匹配项，默认 false。false 时要求 oldText 只出现一次。",
					},
					"occurrence": map[string]any{
						"type":        "integer",
						"description": "当 replaceAll 为 false 且 oldText 出现多次时，指定替换第几处匹配，从 1 开始。默认不指定。",
					},
				},
				"required": []string{"filePath", "oldText", "newText"},
			},
		},
	}
}

func (tool *EditTool) Call(input map[string]any) (string, error) {
	filePath, ok := stringInput(input, "filePath", "file_path")
	if !ok || filePath == "" {
		return "", errors.New("filePath required")
	}

	mode, ok := stringInput(input, "mode")
	if !ok || mode == "" {
		mode = editModeReplace
	}
	if !validEditMode(mode) {
		return "", fmt.Errorf("unsupported edit mode: %s", mode)
	}

	oldText, ok := stringInput(input, "oldText", "old_string")
	if !ok || oldText == "" {
		return "", errors.New("oldText required")
	}

	newText, ok := stringInput(input, "newText", "new_string")
	if !ok && mode != editModeDelete {
		return "", errors.New("newText required")
	}

	replaceAll, _ := input["replaceAll"].(bool)
	occurrence, hasOccurrence, err := optionalPositiveInt(input["occurrence"])
	if err != nil {
		return "", err
	}

	info, err := os.Stat(filePath)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", errors.New("filePath is a directory")
	}

	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", err
	}

	content := string(data)
	count := strings.Count(content, oldText)
	if count == 0 {
		return "", errors.New("oldText not found")
	}
	if replaceAll && hasOccurrence {
		return "", errors.New("occurrence cannot be used when replaceAll is true")
	}
	if !replaceAll && !hasOccurrence && count > 1 {
		return "", fmt.Errorf("oldText appears %d times; set replaceAll to true or provide a more specific oldText", count)
	}
	if hasOccurrence && occurrence > count {
		return "", fmt.Errorf("occurrence %d is out of range; oldText appears %d times", occurrence, count)
	}

	replacedCount := 1
	var updated string
	if replaceAll {
		updated = editAll(content, oldText, newText, mode)
		replacedCount = count
	} else if hasOccurrence {
		updated = editNth(content, oldText, newText, occurrence, mode)
	} else {
		updated = editNth(content, oldText, newText, 1, mode)
	}

	if err := os.WriteFile(filePath, []byte(updated), info.Mode().Perm()); err != nil {
		return "", err
	}

	return fmt.Sprintf("成功编辑文件: %s，编辑 %d 处", filePath, replacedCount), nil
}

func validEditMode(mode string) bool {
	switch mode {
	case editModeReplace, editModeDelete, editModeInsertBefore, editModeInsertAfter:
		return true
	default:
		return false
	}
}

func stringInput(input map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		value, ok := input[key].(string)
		if ok {
			return value, true
		}
	}
	return "", false
}

func optionalPositiveInt(value any) (int, bool, error) {
	if value == nil {
		return 0, false, nil
	}

	var parsed int
	switch v := value.(type) {
	case int:
		parsed = v
	case int64:
		parsed = int(v)
	case float64:
		parsed = int(v)
	case string:
		if v == "" {
			return 0, false, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil {
			return 0, false, errors.New("occurrence must be an integer")
		}
		parsed = n
	default:
		return 0, false, errors.New("occurrence must be an integer")
	}

	if parsed < 1 {
		return 0, false, errors.New("occurrence must be greater than 0")
	}
	return parsed, true, nil
}

func editAll(content string, oldText string, newText string, mode string) string {
	replacement := editReplacement(oldText, newText, mode)
	return strings.Replace(content, oldText, replacement, -1)
}

func editNth(content string, oldText string, newText string, occurrence int, mode string) string {
	start := 0
	for i := 1; i <= occurrence; i++ {
		index := strings.Index(content[start:], oldText)
		if i == occurrence {
			matchStart := start + index
			matchEnd := matchStart + len(oldText)
			return content[:matchStart] + editReplacement(oldText, newText, mode) + content[matchEnd:]
		}
		start += index + len(oldText)
	}

	return content
}

func editReplacement(oldText string, newText string, mode string) string {
	switch mode {
	case editModeDelete:
		return ""
	case editModeInsertBefore:
		return newText + oldText
	case editModeInsertAfter:
		return oldText + newText
	default:
		return newText
	}
}
