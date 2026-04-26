package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const (
	PromptCacheRetentionInMemory = "in-memory"
	PromptCacheRetention24h      = "24h"
	PromptCacheTTL5m             = "5m"
	PromptCacheTTL1h             = "1h"
)

type PromptCacheConfig struct {
	Enabled   bool
	Key       string
	Retention string
	TTL       string
}

func normalizePromptCacheConfig(config PromptCacheConfig) PromptCacheConfig {
	config.Key = strings.TrimSpace(config.Key)
	config.Retention = strings.TrimSpace(config.Retention)
	config.TTL = strings.TrimSpace(config.TTL)
	if config.Retention == "" {
		config.Retention = PromptCacheRetentionInMemory
	}
	if config.TTL == "" {
		config.TTL = PromptCacheTTL5m
	}
	return config
}

func promptCacheKey(config PromptCacheConfig, system string, tools []Tool) string {
	if strings.TrimSpace(config.Key) != "" {
		return strings.TrimSpace(config.Key)
	}

	type toolSchema struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		InputSchema map[string]any `json:"input_schema"`
	}
	payload := struct {
		System string       `json:"system"`
		Tools  []toolSchema `json:"tools"`
	}{
		System: system,
		Tools:  make([]toolSchema, 0, len(tools)),
	}
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		payload.Tools = append(payload.Tools, toolSchema{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.InputSchema(),
		})
	}

	data, err := json.Marshal(payload)
	if err != nil {
		data = []byte(system)
	}
	sum := sha256.Sum256(data)
	return "codingman-" + hex.EncodeToString(sum[:])[:32]
}
