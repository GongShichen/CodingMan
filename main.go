package main

import (
	"agent"
	"encoding/json"
	"fmt"
	"os"
)

type Config struct {
	Provider string `json:"provider"`
	BaseURL  string `json:"base_url"`
	APIKey   string `json:"api_key"`
}

func loadConfig(path string) (Config, error) {
	var cfg Config

	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}

	err = json.Unmarshal(data, &cfg)
	return cfg, err
}

func main() {
	cfg, err := loadConfig("config.json")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}

	client, err := agent.CreateLLM(agent.LLMConfig{
		Provider: cfg.Provider,
		BaseURL:  cfg.BaseURL,
		APIKey:   cfg.APIKey,
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create llm: %v\n", err)
		os.Exit(1)
	}

	_ = client
}
