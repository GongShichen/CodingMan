package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
)

type Agent struct {
	mu       sync.Mutex
	llm      LLM
	system   string
	model    string
	messages []Message
	turns    int
}

type AgentConfig struct {
	LLM    LLM
	System string
	Model  string
}

func NewAgent(config AgentConfig) *Agent {
	agent := &Agent{
		llm:      config.LLM,
		system:   config.System,
		model:    config.Model,
		messages: make([]Message, 0),
	}

	if config.System != "" {
		agent.messages = append(agent.messages, Message{
			Role: "system",
			Content: []ContentBlock{
				TextBlock(config.System),
			},
		})
	}

	return agent
}

func NewAgentFromLLMConfig(llmConfig LLMConfig, system string, model string) (*Agent, error) {
	llmInstance, err := CreateLLM(llmConfig)
	if err != nil {
		return nil, err
	}

	return NewAgent(AgentConfig{
		LLM:    llmInstance,
		System: system,
		Model:  model,
	}), nil
}

func (agent *Agent) Chat(ctx context.Context, prompt string, blocks ...ContentBlock) (LLMResponse, error) {
	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		return LLMResponse{}, err
	}

	agent.mu.Lock()
	if agent.llm == nil {
		agent.mu.Unlock()
		return LLMResponse{StopReason: "agent is nil"}, errors.New("agent llm is nil")
	}

	var system *string
	if agent.system != "" {
		system = &agent.system
	}
	messagesSnapshot := agent.snapshotMessagesLocked()
	model := agent.model
	llm := agent.llm
	agent.mu.Unlock()

	resp, err := llm.Chat(ctx, messagesSnapshot, ChatOptions{
		System: system,
		Model:  model,
	})
	if err != nil {
		return LLMResponse{StopReason: err.Error()}, err
	}

	agent.appendAssistantMessage(resp.Content)

	return resp, nil
}

func (agent *Agent) Stream(ctx context.Context, prompt string, blocks ...ContentBlock) <-chan StreamEvent {
	streamRes := make(chan StreamEvent)

	if err := agent.appendUserMessage(prompt, blocks...); err != nil {
		go func() {
			defer close(streamRes)
			streamRes <- StreamEvent{
				Err:  err,
				Done: true,
			}
		}()
		return streamRes
	}

	agent.mu.Lock()
	if agent.llm == nil {
		agent.mu.Unlock()
		go func() {
			defer close(streamRes)
			streamRes <- StreamEvent{
				Err:  errors.New("agent llm is nil"),
				Done: true,
			}
		}()
		return streamRes
	}

	var system *string
	if agent.system != "" {
		system = &agent.system
	}
	messagesSnapshot := agent.snapshotMessagesLocked()
	model := agent.model
	llm := agent.llm
	agent.mu.Unlock()

	go func(messagesSnapshot []Message) {
		defer close(streamRes)

		var builder strings.Builder
		stream := llm.Stream(ctx, messagesSnapshot, ChatOptions{
			System: system,
			Model:  model,
		})

		for event := range stream {
			if event.Text != "" {
				builder.WriteString(event.Text)
			}

			streamRes <- event

			if event.Err != nil {
				return
			}
		}

		responseText := builder.String()
		if responseText != "" {
			agent.appendAssistantMessage(responseText)
		}
	}(messagesSnapshot)

	return streamRes
}

func (agent *Agent) Clear() {
	agent.mu.Lock()
	defer agent.mu.Unlock()

	agent.turns = 0
	agent.messages = agent.messages[:0]
}

func (agent *Agent) appendUserMessage(text string, blocks ...ContentBlock) error {
	content := make([]ContentBlock, 0, len(blocks)+1)
	if text != "" {
		content = append(content, TextBlock(text))
	}
	content = append(content, blocks...)
	if len(content) == 0 {
		return errors.New("user message must contain text or content blocks")
	}

	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = append(agent.messages, Message{
		Role:    "user",
		Content: content,
	})
	return nil
}

func (agent *Agent) appendAssistantMessage(text string) {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.messages = append(agent.messages, Message{
		Role: "assistant",
		Content: []ContentBlock{
			TextBlock(text),
		},
	})
	agent.turns++
}

func (agent *Agent) Messages() []Message {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	return agent.snapshotMessagesLocked()
}

func (agent *Agent) snapshotMessagesLocked() []Message {
	snapshot := make([]Message, 0, len(agent.messages))
	for _, message := range agent.messages {
		content := append([]ContentBlock(nil), message.Content...)
		snapshot = append(snapshot, Message{
			Role:    message.Role,
			Content: content,
		})
	}
	return snapshot
}
