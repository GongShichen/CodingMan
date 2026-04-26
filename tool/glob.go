package toolUse

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type GlobTool struct {
	BaseTool
}

func NewGlobTool() *GlobTool {
	return &GlobTool{
		BaseTool: BaseTool{
			name:        "glob",
			description: "使用 glob 模式匹配文件路径。\n支持 ** 递归匹配。\n默认只返回文件，不返回目录。\n返回结果按修改时间从新到旧排序。",
			inputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "glob 模式，如：**/*.py",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "搜索的根目录，默认当前目录。",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "最多返回多少条结果，默认 100。",
					},
					"includeDirs": map[string]any{
						"type":        "boolean",
						"description": "是否包含目录，默认 false。",
					},
				},
				"required": []string{"pattern"},
			},
		},
	}
}

func (t *GlobTool) Call(input map[string]any) (string, error) {
	pattern, ok := input["pattern"].(string)
	if !ok || pattern == "" {
		return "", errors.New("pattern is required")
	}
	root, ok := input["path"].(string)
	if !ok || root == "" {
		root = "."
	}
	limit, err := parsePositiveInt(input["limit"], 100)
	if err != nil {
		return "", err
	}
	includeDirs, _ := input["includeDirs"].(bool)

	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !rootInfo.IsDir() {
		return "", errors.New("path must be a directory")
	}

	pattern = normalizePattern(pattern)
	matches := make([]globMatch, 0)

	err = filepath.WalkDir(root, func(filePath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filePath == root {
			return nil
		}
		if entry.IsDir() && !includeDirs {
			return nil
		}

		relPath, err := filepath.Rel(root, filePath)
		if err != nil {
			return err
		}
		relPath = filepath.ToSlash(relPath)

		if !matchGlob(pattern, relPath) {
			return nil
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		matches = append(matches, globMatch{
			path:    filePath,
			modTime: info.ModTime(),
		})
		return nil
	})
	if err != nil {
		return "", err
	}

	sort.Slice(matches, func(i, j int) bool {
		if matches[i].modTime.Equal(matches[j].modTime) {
			return matches[i].path < matches[j].path
		}
		return matches[i].modTime.After(matches[j].modTime)
	})

	if len(matches) > limit {
		matches = matches[:limit]
	}
	if len(matches) == 0 {
		return "", nil
	}

	results := make([]string, 0, len(matches))
	for _, match := range matches {
		results = append(results, match.path)
	}
	return strings.Join(results, "\n"), nil
}

type globMatch struct {
	path    string
	modTime time.Time
}

func normalizePattern(pattern string) string {
	pattern = filepath.ToSlash(pattern)
	pattern = strings.TrimPrefix(pattern, "./")
	return strings.Trim(pattern, "/")
}

func matchGlob(pattern string, name string) bool {
	patternParts := splitGlobPath(pattern)
	nameParts := splitGlobPath(name)
	return matchGlobParts(patternParts, nameParts)
}

func splitGlobPath(value string) []string {
	value = strings.Trim(value, "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}

func matchGlobParts(patternParts []string, nameParts []string) bool {
	if len(patternParts) == 0 {
		return len(nameParts) == 0
	}

	if patternParts[0] == "**" {
		for i := 0; i <= len(nameParts); i++ {
			if matchGlobParts(patternParts[1:], nameParts[i:]) {
				return true
			}
		}
		return false
	}

	if len(nameParts) == 0 {
		return false
	}

	matched, err := path.Match(patternParts[0], nameParts[0])
	if err != nil {
		return false
	}
	if !matched {
		return false
	}
	return matchGlobParts(patternParts[1:], nameParts[1:])
}
