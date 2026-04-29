package agent

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type SkillEvictionConfig struct {
	Enabled            bool
	UnusedDays         int
	MinUses            int
	CheckIntervalHours int
}

type SkillUsageEntry struct {
	Name       string    `json:"name"`
	Path       string    `json:"path"`
	UseCount   int       `json:"use_count"`
	LastUsedAt time.Time `json:"last_used_at"`
}

type skillUsageFile struct {
	LastEvictionCheckAt time.Time                  `json:"last_eviction_check_at"`
	Skills              map[string]SkillUsageEntry `json:"skills"`
}

type SkillEvictionResult struct {
	Evicted []string
}

func DefaultSkillEvictionConfig() SkillEvictionConfig {
	return SkillEvictionConfig{
		Enabled:            true,
		UnusedDays:         90,
		MinUses:            3,
		CheckIntervalHours: 24,
	}
}

func normalizeSkillEvictionConfig(config SkillEvictionConfig) SkillEvictionConfig {
	defaults := DefaultSkillEvictionConfig()
	if config == (SkillEvictionConfig{}) {
		return defaults
	}
	if config.UnusedDays <= 0 {
		config.UnusedDays = defaults.UnusedDays
	}
	if config.MinUses <= 0 {
		config.MinUses = defaults.MinUses
	}
	if config.CheckIntervalHours <= 0 {
		config.CheckIntervalHours = defaults.CheckIntervalHours
	}
	return config
}

func skillUsagePath() (string, error) {
	root, ok := userSkillRoot()
	if !ok || root == "" {
		return "", errors.New("user skill root unavailable")
	}
	return filepath.Join(root, ".codingman_usage.json"), nil
}

func loadSkillUsage() (skillUsageFile, string, error) {
	path, err := skillUsagePath()
	if err != nil {
		return skillUsageFile{}, "", err
	}
	usage := skillUsageFile{Skills: map[string]SkillUsageEntry{}}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return usage, path, nil
		}
		return skillUsageFile{}, path, err
	}
	if len(data) == 0 {
		return usage, path, nil
	}
	if err := json.Unmarshal(data, &usage); err != nil {
		return skillUsageFile{}, path, err
	}
	if usage.Skills == nil {
		usage.Skills = map[string]SkillUsageEntry{}
	}
	return usage, path, nil
}

func saveSkillUsage(path string, usage skillUsageFile) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(usage, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return os.WriteFile(path, data, 0o600)
}

func skillUsageKey(skill Skill) string {
	if strings.TrimSpace(skill.Path) != "" {
		return cleanAbs(skill.Path)
	}
	return strings.TrimSpace(skill.Name)
}

func recordSkillUsage(skill Skill, now time.Time) error {
	if strings.TrimSpace(skill.Name) == "" {
		return nil
	}
	usage, path, err := loadSkillUsage()
	if err != nil {
		return err
	}
	key := skillUsageKey(skill)
	entry := usage.Skills[key]
	entry.Name = skill.Name
	entry.Path = cleanAbs(skill.Path)
	entry.UseCount++
	entry.LastUsedAt = now.UTC()
	usage.Skills[key] = entry
	return saveSkillUsage(path, usage)
}

func (agent *Agent) MaybeEvictGeneratedSkills(now time.Time) (SkillEvictionResult, error) {
	agent.mu.Lock()
	config := normalizeSkillEvictionConfig(agent.skillEvictionConfig)
	contextConfig := agent.contextConfig
	traceID := ""
	agent.mu.Unlock()
	if !config.Enabled {
		return SkillEvictionResult{}, nil
	}
	usage, path, err := loadSkillUsage()
	if err != nil {
		return SkillEvictionResult{}, err
	}
	interval := time.Duration(config.CheckIntervalHours) * time.Hour
	if !usage.LastEvictionCheckAt.IsZero() && now.Sub(usage.LastEvictionCheckAt) < interval {
		return SkillEvictionResult{}, nil
	}
	usage.LastEvictionCheckAt = now.UTC()

	userSkills := LoadUserSkillsWithWarnings(contextConfig).Skills
	if len(userSkills) == 0 {
		return SkillEvictionResult{}, nil
	}
	cutoff := now.AddDate(0, 0, -config.UnusedDays)
	result := SkillEvictionResult{}
	for _, skill := range userSkills {
		if !skill.CodingManGenerated {
			continue
		}
		entry := usage.Skills[skillUsageKey(skill)]
		if entry.LastUsedAt.IsZero() {
			if !skill.UpdatedAt.IsZero() {
				entry.LastUsedAt = skill.UpdatedAt
			} else if !skill.CreatedAt.IsZero() {
				entry.LastUsedAt = skill.CreatedAt
			} else {
				entry.LastUsedAt = now.UTC()
			}
		}
		if entry.UseCount >= config.MinUses || !entry.LastUsedAt.Before(cutoff) {
			usage.Skills[skillUsageKey(skill)] = entry
			continue
		}
		if err := removeGeneratedUserSkill(skill); err != nil {
			return result, err
		}
		delete(usage.Skills, skillUsageKey(skill))
		result.Evicted = append(result.Evicted, skill.Name)
		agent.log(traceID, "skill_eviction evicted name=%s path=%s", skill.Name, skill.Path)
	}
	if err := saveSkillUsage(path, usage); err != nil {
		return result, err
	}
	if len(result.Evicted) > 0 {
		agent.mu.Lock()
		agent.skills = loadedSkillsForConfig(agent.contextConfig)
		agent.refreshActiveSkillLocked()
		agent.mu.Unlock()
	}
	return result, nil
}

func removeGeneratedUserSkill(skill Skill) error {
	root, ok := userSkillRoot()
	if !ok || root == "" {
		return errors.New("user skill root unavailable")
	}
	path := cleanAbs(skill.Path)
	if path == "" || filepath.Base(path) != "SKILL.md" {
		return errors.New("invalid skill path")
	}
	if !isWithinDir(root, cleanRealPath(path)) {
		return errors.New("skill path escapes user skill root")
	}
	dir := filepath.Dir(path)
	if dir == root || !isWithinDir(root, cleanRealPath(dir)) {
		return errors.New("skill directory escapes user skill root")
	}
	return os.RemoveAll(dir)
}
