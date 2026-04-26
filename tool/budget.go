package tool

import "fmt"

func TruncateToolResult(content string, maxLen int, headLen int, tailLen int) (string, error) {
	if maxLen == 0 {
		maxLen = 10000
	}
	if headLen == 0 {
		headLen = 3000
	}
	if tailLen == 0 {
		tailLen = 3000
	}

	if len(content) <= maxLen {
		return content, nil
	}
	if headLen+tailLen >= len(content) {
		return content, nil
	}

	head := content[:headLen]
	tail := content[len(content)-tailLen:]
	omitted := len(content) - tailLen - headLen

	return fmt.Sprintf("%s\n...[内容已截断，省略%d个字符，共%d个字符]...\n%s", head, omitted, len(content), tail), nil
}
