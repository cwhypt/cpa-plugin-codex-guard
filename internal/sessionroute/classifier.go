// Package sessionroute contains the configurable, side-effect-free routing policy.
package sessionroute

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Config is intended to be embedded under session_routing in plugin YAML.
// No account identifiers or classification rules are compiled into the plugin.
type Config struct {
	Enabled         bool             `yaml:"enabled" json:"enabled"`
	DefaultCategory string           `yaml:"default_category" json:"default_category"`
	Rules           []Rule           `yaml:"rules" json:"rules"`
	Categories      map[string]Route `yaml:"categories" json:"categories"`
	BindingTTL      string           `yaml:"binding_ttl" json:"binding_ttl"`
	StateFile       string           `yaml:"state_file" json:"state_file"`
	ModelInclude    string           `yaml:"model_include_regex" json:"model_include_regex"`
	compiledModel   *regexp.Regexp
}

type Route struct {
	PreferredAuthIDs []string `yaml:"preferred_auth_ids" json:"preferred_auth_ids"`
	MaxPriority      *int     `yaml:"max_priority" json:"max_priority"`
	ExcludeAuthIDs   []string `yaml:"exclude_auth_ids" json:"exclude_auth_ids"`
}

type Rule struct {
	Category      string   `yaml:"category" json:"category"`
	Models        []string `yaml:"models" json:"models"`
	UserAgent     string   `yaml:"user_agent_regex" json:"user_agent_regex"`
	TextAll       []string `yaml:"text_all_regex" json:"text_all_regex"`
	compiledUA    *regexp.Regexp
	compiledTexts []*regexp.Regexp
}

// Compile validates and compiles configuration before publishing it to callers.
func (c *Config) Compile() error {
	if !c.Enabled {
		return nil
	}
	if c.DefaultCategory == "" {
		return fmt.Errorf("default_category is required")
	}
	if _, ok := c.Categories[c.DefaultCategory]; !ok {
		return fmt.Errorf("default category is not configured")
	}
	if c.StateFile == "" {
		return fmt.Errorf("state_file is required")
	}
	if ttl, err := time.ParseDuration(c.BindingTTL); err != nil || ttl <= 0 {
		return fmt.Errorf("binding_ttl must be a positive Go duration")
	}
	if c.ModelInclude == "" {
		return fmt.Errorf("model_include_regex is required")
	}
	model, err := regexp.Compile(c.ModelInclude)
	if err != nil {
		return fmt.Errorf("model_include_regex: %w", err)
	}
	c.compiledModel = model
	for name, route := range c.Categories {
		if name == "" || len(route.PreferredAuthIDs) == 0 && route.MaxPriority == nil {
			return fmt.Errorf("category %q needs preferred_auth_ids or max_priority", name)
		}
		for _, id := range append(append([]string(nil), route.PreferredAuthIDs...), route.ExcludeAuthIDs...) {
			if strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id {
				return fmt.Errorf("category %q contains an empty or padded auth ID", name)
			}
		}
	}
	for i := range c.Rules {
		rule := &c.Rules[i]
		if _, ok := c.Categories[rule.Category]; !ok || rule.Category == "" {
			return fmt.Errorf("rule %d references an unknown category", i)
		}
		if len(rule.TextAll) == 0 {
			return fmt.Errorf("rule %d must match user text", i)
		}
		if rule.UserAgent != "" {
			compiled, err := regexp.Compile(rule.UserAgent)
			if err != nil {
				return fmt.Errorf("rule %d user_agent_regex: %w", i, err)
			}
			rule.compiledUA = compiled
		}
		rule.compiledTexts = nil
		for _, pattern := range rule.TextAll {
			if pattern == "" {
				return fmt.Errorf("rule %d has an empty text pattern", i)
			}
			compiled, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("rule %d text pattern: %w", i, err)
			}
			rule.compiledTexts = append(rule.compiledTexts, compiled)
		}
	}
	return nil
}

// Classify evaluates ordered rules against the latest user text only.
// ok=false means the request must use the host's normal routing.
func (c *Config) Classify(model, userAgent string, body []byte) (category string, ok bool) {
	if !c.IncludesModel(model) || len(body) == 0 || len(body) > 15*1024*1024 {
		return "", false
	}
	text, ok := latestUserText(body)
	if !ok {
		return "", false
	}
	for _, rule := range c.Rules {
		if len(rule.Models) > 0 && !contains(rule.Models, model) {
			continue
		}
		if rule.compiledUA != nil && !rule.compiledUA.MatchString(userAgent) {
			continue
		}
		matched := len(rule.compiledTexts) == len(rule.TextAll) && len(rule.compiledTexts) > 0
		for _, pattern := range rule.compiledTexts {
			if !pattern.MatchString(text) {
				matched = false
				break
			}
		}
		if matched {
			return rule.Category, true
		}
	}
	return c.DefaultCategory, true
}

func (c *Config) IncludesModel(model string) bool {
	return c != nil && c.Enabled && c.compiledModel != nil && c.compiledModel.MatchString(model)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func latestUserText(body []byte) (string, bool) {
	var root struct {
		Input    json.RawMessage `json:"input"`
		Messages json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil {
		return "", false
	}
	if len(root.Input) > 0 {
		var text string
		if json.Unmarshal(root.Input, &text) == nil {
			return text, strings.TrimSpace(text) != ""
		}
		return lastUserMessage(root.Input)
	}
	return lastUserMessage(root.Messages)
}

func lastUserMessage(raw json.RawMessage) (string, bool) {
	var items []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return "", false
	}
	for i := len(items) - 1; i >= 0; i-- {
		if items[i].Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(items[i].Content, &text) == nil {
			return text, strings.TrimSpace(text) != ""
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(items[i].Content, &parts) != nil {
			return "", false
		}
		var result strings.Builder
		for _, part := range parts {
			if part.Type != "text" && part.Type != "input_text" {
				return "", false
			}
			if result.Len() > 0 {
				result.WriteByte('\n')
			}
			result.WriteString(part.Text)
		}
		text = result.String()
		return text, strings.TrimSpace(text) != ""
	}
	return "", false
}
