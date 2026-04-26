package agent

import (
	"context"
	"fmt"
	"strings"

	tool "github.com/GongShichen/CodingMan/tool"
)

const conversationSummaryMarker = "[CONVERSATION SUMMARY]"

const compactSummaryPrompt = `请压缩下面较早的对话历史，保留对后续任务仍然重要的信息：
- 用户的目标、偏好和明确要求
- 已经做过的关键修改、决策和结论
- 仍然未完成的问题、错误和下一步
- 重要文件路径、函数名、配置名和约束

不要编造信息。用简洁的中文条目输出。`

const (
	defaultAutoCompactSizeThreshold = 60000
	defaultAutoCompactRecentRounds  = 6
)

func defaultAutoCompactThreshold(value int) int {
	if value > 0 {
		return value
	}
	return defaultAutoCompactSizeThreshold
}

func defaultAutoCompactKeepRecent(value int) int {
	if value > 0 {
		return value
	}
	return defaultAutoCompactRecentRounds
}

func CompactMessages(messages []Message, llm LLM, keepRecent int) []Message {
	return CompactMessagesWithOptions(messages, llm, CompactOptions{
		KeepRecentRounds: keepRecent,
	})
}

type CompactOptions struct {
	KeepRecentRounds int
	ToolBudget       ToolBudget
}

func CompactMessagesWithOptions(messages []Message, llm LLM, options CompactOptions) []Message {
	if len(messages) == 0 {
		return nil
	}
	if options.KeepRecentRounds <= 0 {
		options.KeepRecentRounds = defaultAutoCompactRecentRounds
	}

	var systemMessage *Message
	otherMessages := make([]Message, 0, len(messages))

	for _, message := range messages {
		if message.Role == "system" && systemMessage == nil {
			systemMessage = new(Message)
			*systemMessage = cloneMessage(message)
			continue
		}
		otherMessages = append(otherMessages, cloneMessage(message))
	}

	otherMessages = stripExistingSummaries(otherMessages)
	oldMessages, recentMessages := splitRecentRounds(otherMessages, options.KeepRecentRounds)
	if len(oldMessages) == 0 || llm == nil {
		return appendSystemMessage(systemMessage, recentMessages)
	}

	oldMessages = applyToolResultBudget(oldMessages, options.ToolBudget)
	historyText := FormatMessages(oldMessages)
	if strings.TrimSpace(historyText) == "" {
		return appendSystemMessage(systemMessage, recentMessages)
	}

	resp, err := llm.Chat(context.Background(), []Message{
		{
			Role: "user",
			Content: []ContentBlock{
				TextBlock(compactSummaryPrompt + "\n\n" + historyText),
			},
		},
	}, ChatOptions{})
	if err != nil || strings.TrimSpace(resp.Content) == "" {
		return appendSystemMessage(systemMessage, otherMessages)
	}

	compacted := make([]Message, 0, len(recentMessages)+2)
	if systemMessage != nil {
		compacted = append(compacted, cloneMessage(*systemMessage))
	}
	compacted = append(compacted, Message{
		Role: "user",
		Content: []ContentBlock{
			TextBlock(conversationSummaryMarker + "\n" + strings.TrimSpace(resp.Content)),
		},
	})
	compacted = append(compacted, recentMessages...)
	return compacted
}

func splitRecentRounds(messages []Message, keepRecent int) ([]Message, []Message) {
	if keepRecent <= 0 || len(messages) == 0 {
		return messages, nil
	}

	rounds := 0
	start := len(messages)
	for i := len(messages) - 1; i >= 0; i-- {
		if isUserTurnStart(messages[i]) {
			rounds++
			if rounds == keepRecent {
				start = i
				break
			}
		}
	}

	if rounds < keepRecent {
		start = 0
	}

	return messages[:start], messages[start:]
}

func isUserTurnStart(message Message) bool {
	if message.Role != "user" {
		return false
	}
	for _, block := range message.Content {
		if block.Type == ContentTypeToolResult {
			return false
		}
	}
	return true
}

func stripExistingSummaries(messages []Message) []Message {
	result := make([]Message, 0, len(messages))
	for _, message := range messages {
		if isSummaryMessage(message) {
			continue
		}
		result = append(result, message)
	}
	return result
}

func isSummaryMessage(message Message) bool {
	if message.Role != "user" || len(message.Content) != 1 {
		return false
	}
	block := message.Content[0]
	return block.Type == ContentTypeText && strings.HasPrefix(strings.TrimSpace(block.Text), conversationSummaryMarker)
}

func applyToolResultBudget(messages []Message, budget ToolBudget) []Message {
	result := cloneMessages(messages)
	for i := range result {
		for j := range result[i].Content {
			block := &result[i].Content[j]
			if block.Type != ContentTypeToolResult || block.Text == "" {
				continue
			}
			truncated, err := tool.TruncateToolResult(block.Text, budget.MaxLen, budget.HeadLen, budget.TailLen)
			if err == nil {
				block.Text = truncated
			}
		}
	}
	return result
}

func EstimateMessagesSize(messages []Message) int {
	total := 0
	for _, message := range messages {
		total += len(message.Role)
		for _, block := range message.Content {
			total += len(block.Text)
			total += len(block.Data)
			total += len(block.MediaType)
			total += len(block.ImageURL)
			total += len(block.ToolID)
			total += len(block.ToolName)
			total += len(block.ToolInput)
		}
	}
	return total
}

func appendSystemMessage(systemMessage *Message, messages []Message) []Message {
	result := make([]Message, 0, len(messages)+1)
	if systemMessage != nil {
		result = append(result, cloneMessage(*systemMessage))
	}
	for _, message := range messages {
		result = append(result, cloneMessage(message))
	}
	return result
}

func FormatMessages(messages []Message) string {
	var builder strings.Builder
	for _, message := range messages {
		role := message.Role
		if role == "" {
			role = "unknown"
		}

		content := formatContentBlocks(message.Content)
		if strings.TrimSpace(content) == "" {
			continue
		}

		if builder.Len() > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(strings.ToUpper(role))
		builder.WriteString(":\n")
		builder.WriteString(content)
	}
	return builder.String()
}

func formatContentBlocks(blocks []ContentBlock) string {
	parts := make([]string, 0, len(blocks))
	for _, block := range blocks {
		switch block.Type {
		case ContentTypeText:
			if text := strings.TrimSpace(block.Text); text != "" {
				parts = append(parts, text)
			}
		case ContentTypeImage:
			if block.ImageURL != "" {
				parts = append(parts, fmt.Sprintf("[image: %s]", block.ImageURL))
			} else if block.MediaType != "" {
				parts = append(parts, fmt.Sprintf("[image: %s base64]", block.MediaType))
			} else {
				parts = append(parts, "[image]")
			}
		case ContentTypeToolUse:
			parts = append(parts, fmt.Sprintf("[tool_use id=%s name=%s input=%s]", block.ToolID, block.ToolName, block.ToolInput))
		case ContentTypeToolResult:
			label := "tool_result"
			if block.IsError {
				label = "tool_result_error"
			}
			parts = append(parts, fmt.Sprintf("[%s id=%s]\n%s", label, block.ToolID, block.Text))
		default:
			parts = append(parts, fmt.Sprintf("[%s block]", block.Type))
		}
	}
	return strings.Join(parts, "\n")
}

func cloneMessage(message Message) Message {
	return Message{
		Role:    message.Role,
		Content: append([]ContentBlock(nil), message.Content...),
	}
}

func cloneMessages(messages []Message) []Message {
	cloned := make([]Message, 0, len(messages))
	for _, message := range messages {
		cloned = append(cloned, cloneMessage(message))
	}
	return cloned
}
