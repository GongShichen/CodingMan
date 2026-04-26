package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/GongShichen/CodingMan/agent"
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

	a := agent.NewAgent(agent.AgentConfig{
		LLM: client,
	})
	RunReply(a, true)
}

func RunReply(a *agent.Agent, useStream bool) {
	scanner := bufio.NewScanner(os.Stdin)
	fmt.Println("Enter prompt. Type '/exit' or '/quit' to stop.")

	for {
		fmt.Print("> ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				_, _ = fmt.Fprintf(os.Stderr, "read input: %v\n", err)
			}
			return
		}

		prompt := strings.TrimSpace(scanner.Text())
		if prompt == "" {
			continue
		}
		if prompt == "/exit" || prompt == "/quit" {
			return
		}

		if useStream {
			stream := a.Stream(context.Background(), prompt)
			for event := range stream {
				if event.Err != nil {
					_, _ = fmt.Fprintf(os.Stderr, "stream error: %v\n", event.Err)
					break
				}
				if event.Text != "" {
					fmt.Print(event.Text)
				}
				if event.Done {
					fmt.Println()
				}
			}
			continue
		}

		resp, err := a.Chat(context.Background(), prompt)
		if err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "chat error: %v\n", err)
			continue
		}
		fmt.Println(resp.Content)
	}
}
