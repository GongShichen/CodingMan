package tool

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultWebSearchEndpoint  = "https://html.duckduckgo.com/html/"
	defaultWebSearchTimeoutMs = 15000
	defaultWebSearchLimit     = 5
	maxWebSearchLimit         = 10
)

type WebSearchTool struct {
	BaseTool
	client   *http.Client
	endpoint string
}

type webSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet,omitempty"`
}

func NewWebSearchTool() *WebSearchTool {
	return NewWebSearchToolWithClient(http.DefaultClient, defaultWebSearchEndpoint)
}

func NewWebSearchToolWithClient(client *http.Client, endpoint string) *WebSearchTool {
	if client == nil {
		client = http.DefaultClient
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = defaultWebSearchEndpoint
	}
	return &WebSearchTool{
		BaseTool: NewBaseTool(
			"websearch",
			"搜索互联网并返回相关网页结果。适合查询最新信息、外部资料、文档和网页来源。",
			map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "搜索关键词。",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "最多返回多少条结果，默认 5，最大 10。",
					},
					"region": map[string]any{
						"type":        "string",
						"description": "可选地区代码，如 us-en、cn-zh。",
					},
					"timeout": map[string]any{
						"type":        "integer",
						"description": "超时时间，单位毫秒，默认 15000。",
					},
				},
				"required": []string{"query"},
			},
		),
		client:   client,
		endpoint: endpoint,
	}
}

func (t *WebSearchTool) Call(input map[string]any) (string, error) {
	return t.CallContext(context.Background(), input)
}

func (t *WebSearchTool) CallContext(ctx context.Context, input map[string]any) (string, error) {
	query, ok := input["query"].(string)
	query = strings.TrimSpace(query)
	if !ok || query == "" {
		return "", errors.New("query is required")
	}

	limit, err := parsePositiveInt(input["limit"], defaultWebSearchLimit)
	if err != nil {
		return "", err
	}
	if limit > maxWebSearchLimit {
		limit = maxWebSearchLimit
	}
	timeout := parseTimeoutMsWithDefault(input["timeout"], defaultWebSearchTimeoutMs)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond)
	defer cancel()

	searchURL, err := t.searchURL(query, input)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, searchURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "CodingMan/1.0 (+https://github.com/GongShichen/CodingMan)")

	resp, err := t.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", errors.New("search request failed: " + resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", err
	}
	results := parseDuckDuckGoHTML(string(body), limit)
	if len(results) == 0 {
		return "[]", nil
	}

	encoded, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func (t *WebSearchTool) searchURL(query string, input map[string]any) (string, error) {
	parsed, err := url.Parse(t.endpoint)
	if err != nil {
		return "", err
	}
	values := parsed.Query()
	values.Set("q", query)
	if region, ok := input["region"].(string); ok && strings.TrimSpace(region) != "" {
		values.Set("kl", strings.TrimSpace(region))
	}
	parsed.RawQuery = values.Encode()
	return parsed.String(), nil
}

func parseTimeoutMsWithDefault(value any, defaultValue int) int {
	if value == nil {
		return defaultValue
	}
	switch v := value.(type) {
	case int:
		if v > 0 {
			return v
		}
	case int64:
		if v > 0 {
			return int(v)
		}
	case float64:
		if v > 0 {
			return int(v)
		}
	}
	return defaultValue
}

var (
	duckResultRE  = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	duckSnippetRE = regexp.MustCompile(`(?s)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>|<div[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</div>`)
	tagRE         = regexp.MustCompile(`(?s)<[^>]+>`)
	spaceRE       = regexp.MustCompile(`\s+`)
)

func parseDuckDuckGoHTML(body string, limit int) []webSearchResult {
	resultMatches := duckResultRE.FindAllStringSubmatch(body, -1)
	snippetMatches := duckSnippetRE.FindAllStringSubmatch(body, -1)
	results := make([]webSearchResult, 0, minInt(limit, len(resultMatches)))
	seen := make(map[string]struct{})

	for index, match := range resultMatches {
		if len(results) >= limit {
			break
		}
		link := normalizeSearchResultURL(match[1])
		title := cleanSearchText(match[2])
		if link == "" || title == "" {
			continue
		}
		if _, exists := seen[link]; exists {
			continue
		}
		seen[link] = struct{}{}

		result := webSearchResult{
			Title: title,
			URL:   link,
		}
		if index < len(snippetMatches) {
			for _, snippet := range snippetMatches[index][1:] {
				if cleaned := cleanSearchText(snippet); cleaned != "" {
					result.Snippet = cleaned
					break
				}
			}
		}
		results = append(results, result)
	}
	return results
}

func normalizeSearchResultURL(rawURL string) string {
	rawURL = html.UnescapeString(strings.TrimSpace(rawURL))
	if rawURL == "" {
		return ""
	}
	parsed, err := url.Parse(rawURL)
	if err == nil {
		if encoded := parsed.Query().Get("uddg"); encoded != "" {
			if decoded, err := url.QueryUnescape(encoded); err == nil {
				return decoded
			}
			return encoded
		}
	}
	return rawURL
}

func cleanSearchText(value string) string {
	value = tagRE.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	value = spaceRE.ReplaceAllString(value, " ")
	return strings.TrimSpace(value)
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}
