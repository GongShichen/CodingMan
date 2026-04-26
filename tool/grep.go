package tool

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type GrepTool struct {
	BaseTool
}

func NewGrepTool() *GrepTool {
	return &GrepTool{
		BaseTool{
			name:        "grep",
			description: "在文件中搜索正则表达式，返回匹配的行及其行号，\n格式：filePath:line: content",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "正则表达式",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "文件或目录，默认当前目录。",
					},
					"include": map[string]any{
						"type":        "string",
						"description": "文件过滤 glob，如 *.go 或 **/*.go。",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "最多返回多少条匹配，默认 100。",
					},
					"caseSensitive": map[string]any{
						"type":        "boolean",
						"description": "是否区分大小写，默认 true。",
					},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

func (t *GrepTool) Call(input map[string]any) (string, error) {
	pattern, ok := input["pattern"].(string)
	if !ok || pattern == "" {
		return "", errors.New("pattern is required")
	}

	root, ok := input["path"].(string)
	if !ok || root == "" {
		root = "."
	}

	include, _ := input["include"].(string)
	limit, err := parsePositiveInt(input["limit"], 100)
	if err != nil {
		return "", err
	}

	caseSensitive := true
	if value, ok := input["caseSensitive"].(bool); ok {
		caseSensitive = value
	}
	if !caseSensitive {
		pattern = "(?i)" + pattern
	}

	expr, err := regexp.Compile(pattern)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}

	matches := make([]string, 0, limit)
	if !info.IsDir() {
		if include != "" && !grepIncludeMatches(include, filepath.Base(root), filepath.Base(root)) {
			return "", nil
		}
		if err := grepFile(root, expr, limit, &matches); err != nil {
			return "", err
		}
		return strings.Join(matches, "\n"), nil
	}

	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if len(matches) >= limit {
			return filepath.SkipAll
		}
		if entry.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		relPath = filepath.ToSlash(relPath)
		if include != "" && !grepIncludeMatches(include, relPath, entry.Name()) {
			return nil
		}

		return grepFile(filePath, expr, limit, &matches)
	})
	if err != nil {
		return "", err
	}

	return strings.Join(matches, "\n"), nil
}

func grepIncludeMatches(include string, relPath string, baseName string) bool {
	include = normalizePattern(include)
	if strings.Contains(include, "/") {
		return matchGlob(include, relPath)
	}
	return matchGlob(include, baseName)
}

func grepFile(filePath string, expr *regexp.Regexp, limit int, matches *[]string) error {
	file, err := os.Open(filePath)
	if err != nil {
		return err
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	lineNumber := 0
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			lineNumber++
			line = strings.TrimRight(line, "\r\n")
			if expr.MatchString(line) {
				*matches = append(*matches, fmt.Sprintf("%s:%d: %s", filePath, lineNumber, line))
				if len(*matches) >= limit {
					return nil
				}
			}
		}

		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
