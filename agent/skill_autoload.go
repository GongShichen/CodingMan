package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type autoSkillSelection struct {
	Skill  string `json:"skill"`
	Reason string `json:"reason"`
}

func (agent *Agent) AutoSelectSkillForPrompt(ctx context.Context, prompt string, blocks ...ContentBlock) (Skill, bool, error) {
	ctx, traceID := ensureTrace(ctx)
	agent.mu.Lock()
	if agent.activeSkill != "" && !agent.activeSkillAuto {
		activeName := agent.activeSkill
		agent.mu.Unlock()
		agent.log(traceID, "skill_autoload skipped reason=manual_active active=%s", activeName)
		return Skill{Name: activeName}, false, nil
	}
	llm := agent.llm
	model := agent.model
	promptCache := agent.promptCache
	contextConfig := agent.contextConfig
	agent.mu.Unlock()

	if llm == nil {
		return Skill{}, false, nil
	}
	userSkills := LoadUserSkillsWithWarnings(contextConfig).Skills
	if len(userSkills) == 0 {
		agent.clearAutoSkill()
		return Skill{}, false, nil
	}

	selectionPrompt := buildAutoSkillSelectionPrompt(prompt, blocks, userSkills)
	resp, err := llm.Chat(ctx, []Message{{Role: "user", Content: []ContentBlock{TextBlock(selectionPrompt)}}}, ChatOptions{
		Model:       model,
		PromptCache: promptCache,
	})
	if err != nil {
		agent.log(traceID, "skill_autoload llm_error=%v", err)
		agent.clearAutoSkill()
		return Skill{}, false, nil
	}
	selection, err := parseAutoSkillSelection(resp.Content)
	if err != nil {
		agent.log(traceID, "skill_autoload parse_error=%v content=%s", err, resp.Content)
		agent.clearAutoSkill()
		return Skill{}, false, nil
	}
	selectedName := strings.TrimSpace(selection.Skill)
	if selectedName == "" {
		agent.clearAutoSkill()
		return Skill{}, false, nil
	}
	for _, skill := range userSkills {
		if skill.Name == selectedName || strings.EqualFold(skill.Name, selectedName) {
			if err := agent.activateAutoSkill(skill); err != nil {
				return Skill{}, false, err
			}
			if err := recordSkillUsage(skill, time.Now()); err != nil {
				agent.log(traceID, "skill_autoload usage_error=%v", err)
			}
			agent.emitEvent(AgentEvent{Type: AgentEventSkillSelected, SkillSelected: &SkillSelectedEvent{
				Skill:  cloneSkill(skill),
				Reason: strings.TrimSpace(selection.Reason),
				Auto:   true,
			}})
			agent.log(traceID, "skill_autoload selected name=%s reason=%s", skill.Name, selection.Reason)
			return cloneSkill(skill), true, nil
		}
	}
	agent.log(traceID, "skill_autoload unknown_skill=%s", selectedName)
	agent.clearAutoSkill()
	return Skill{}, false, nil
}

func buildAutoSkillSelectionPrompt(prompt string, blocks []ContentBlock, skills []Skill) string {
	var builder strings.Builder
	builder.WriteString("你是 CodingMan 的 SKILL 路由器。请根据用户即将执行的任务，从用户级 SKILL 中选择最多一个最相关的 SKILL。\n")
	builder.WriteString("只输出严格 JSON 对象，不要 Markdown，不要解释。格式：{\"skill\":\"<skill name or empty>\",\"reason\":\"<short reason>\"}\n")
	builder.WriteString("如果没有明显相关 SKILL，skill 必须为空字符串。\n\n")
	builder.WriteString("用户任务：\n")
	builder.WriteString(strings.TrimSpace(prompt))
	if len(blocks) > 0 {
		builder.WriteString(fmt.Sprintf("\n\n附加内容块数量：%d\n", len(blocks)))
	}
	builder.WriteString("\n\n用户级 SKILL 索引：\n")
	for _, skill := range skills {
		builder.WriteString(fmt.Sprintf("- name: %s\n  description: %s\n  path: %s\n", skill.Name, skill.Description, skill.Path))
	}
	return builder.String()
}

func parseAutoSkillSelection(content string) (autoSkillSelection, error) {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		content = stripJSONFence(content)
	}
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end < start {
		return autoSkillSelection{}, fmt.Errorf("missing json object")
	}
	var selection autoSkillSelection
	if err := json.Unmarshal([]byte(content[start:end+1]), &selection); err != nil {
		return autoSkillSelection{}, err
	}
	return selection, nil
}

func (agent *Agent) activateAutoSkill(skill Skill) error {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	agent.activeSkill = skill.Name
	agent.activeSkillContent = fullSkillContent(skill)
	agent.activeSkillAuto = true
	agent.skillAllowedTools, agent.skillToolRestriction = skillToolAllowlist(skill)
	return nil
}

func (agent *Agent) clearAutoSkill() {
	agent.mu.Lock()
	defer agent.mu.Unlock()
	if !agent.activeSkillAuto {
		return
	}
	agent.activeSkill = ""
	agent.activeSkillContent = ""
	agent.activeSkillAuto = false
	agent.skillAllowedTools = nil
	agent.skillToolRestriction = false
}
