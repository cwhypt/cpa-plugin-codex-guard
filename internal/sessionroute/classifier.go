// Package sessionroute contains the configurable, side-effect-free routing policy.
package sessionroute

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Config is intended to be embedded under session_routing in plugin YAML.
// No account identifiers or classification rules are compiled into the plugin.
type Config struct {
	Enabled         bool                     `yaml:"enabled" json:"enabled"`
	DefaultCategory string                   `yaml:"default_category" json:"default_category"`
	Rules           []Rule                   `yaml:"rules" json:"rules"`
	Categories      map[string]Route         `yaml:"categories" json:"categories"`
	BindingTTL      string                   `yaml:"binding_ttl" json:"binding_ttl"`
	StateFile       string                   `yaml:"state_file" json:"state_file"`
	ModelInclude    string                   `yaml:"model_include_regex" json:"model_include_regex"`
	ProtectedAuths  map[string]AuthAdmission `yaml:"protected_auths" json:"protected_auths"`
	compiledModel   *regexp.Regexp
}

// AuthAdmission limits which session categories/models may borrow a reserved
// credential, including fallback selections. It never infers quality itself.
type AuthAdmission struct {
	Categories    []string `yaml:"categories" json:"categories"`
	ModelInclude  string   `yaml:"model_include_regex" json:"model_include_regex"`
	compiledModel *regexp.Regexp
}

func (c *Config) AllowsAuth(id, category, model string) bool {
	admission, protected := c.ProtectedAuths[id]
	return !protected || category != "" && contains(admission.Categories, category) && admission.compiledModel != nil && admission.compiledModel.MatchString(model)
}

type Route struct {
	PreferredAuthIDs []string `yaml:"preferred_auth_ids" json:"preferred_auth_ids"`
	MaxPriority      *int     `yaml:"max_priority" json:"max_priority"`
	ExcludeAuthIDs   []string `yaml:"exclude_auth_ids" json:"exclude_auth_ids"`
}

type Rule struct {
	Category         string   `yaml:"category" json:"category"`
	Models           []string `yaml:"models" json:"models"`
	ModelInclude     string   `yaml:"model_include_regex" json:"model_include_regex"`
	ReasoningEfforts []string `yaml:"reasoning_efforts" json:"reasoning_efforts"`
	MinUserChars     int      `yaml:"min_user_chars" json:"min_user_chars"`
	MinContextChars  int      `yaml:"min_context_chars" json:"min_context_chars"`
	UserAgent        string   `yaml:"user_agent_regex" json:"user_agent_regex"`
	TextAll          []string `yaml:"text_all_regex" json:"text_all_regex"`
	Standalone       bool     `yaml:"standalone" json:"standalone"`
	DisallowTools    bool     `yaml:"disallow_tools" json:"disallow_tools"`
	MaxInputBytes    int      `yaml:"max_input_bytes" json:"max_input_bytes"`
	CallerScopes     []string `yaml:"caller_scopes" json:"caller_scopes"`
	compiledUA       *regexp.Regexp
	compiledModel    *regexp.Regexp
	compiledTexts    []*regexp.Regexp
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
	if c.Categories[c.DefaultCategory].MaxPriority != nil {
		return fmt.Errorf("default_category cannot be a priority pool; uncertain context must not become a probe")
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
	for id, admission := range c.ProtectedAuths {
		if id == "" || strings.TrimSpace(id) != id || len(admission.Categories) == 0 || admission.ModelInclude == "" {
			return fmt.Errorf("protected auth requires an exact ID, categories and model_include_regex")
		}
		for _, category := range admission.Categories {
			if _, ok := c.Categories[category]; !ok || category == "" {
				return fmt.Errorf("protected auth references unknown category %q", category)
			}
		}
		compiled, err := regexp.Compile(admission.ModelInclude)
		if err != nil {
			return fmt.Errorf("protected auth model_include_regex: %w", err)
		}
		admission.compiledModel = compiled
		c.ProtectedAuths[id] = admission
	}
	for i := range c.Rules {
		rule := &c.Rules[i]
		if _, ok := c.Categories[rule.Category]; !ok || rule.Category == "" {
			return fmt.Errorf("rule %d references an unknown category", i)
		}
		if rule.MinUserChars < 0 || rule.MinContextChars < 0 || rule.MinUserChars > 15*1024*1024 || rule.MinContextChars > 15*1024*1024 {
			return fmt.Errorf("rule %d has an invalid minimum text length", i)
		}
		if len(rule.TextAll) == 0 && (rule.MinUserChars == 0 && rule.MinContextChars == 0 || len(rule.ReasoningEfforts) == 0 || rule.ModelInclude == "" && len(rule.Models) == 0) {
			return fmt.Errorf("rule %d needs user text patterns or model, effort and minimum text length constraints", i)
		}
		for _, effort := range rule.ReasoningEfforts {
			if effort == "" || strings.TrimSpace(effort) != effort {
				return fmt.Errorf("rule %d contains an invalid reasoning effort", i)
			}
		}
		rule.compiledModel = nil
		if rule.ModelInclude != "" {
			compiled, err := regexp.Compile(rule.ModelInclude)
			if err != nil {
				return fmt.Errorf("rule %d model_include_regex: %w", i, err)
			}
			rule.compiledModel = compiled
		}
		if rule.MaxInputBytes < 0 || rule.MaxInputBytes > 15*1024*1024 {
			return fmt.Errorf("rule %d max_input_bytes must be between 0 and 15 MiB", i)
		}
		for _, scope := range rule.CallerScopes {
			if strings.TrimSpace(scope) == "" || strings.TrimSpace(scope) != scope {
				return fmt.Errorf("rule %d contains an empty or padded caller scope", i)
			}
		}
		if c.Categories[rule.Category].MaxPriority != nil && (len(rule.TextAll) == 0 || !rule.Standalone || rule.MaxInputBytes == 0 || rule.UserAgent == "" && len(rule.CallerScopes) == 0) {
			return fmt.Errorf("rule %d priority pool requires standalone, max_input_bytes and user_agent_regex or caller_scopes", i)
		}
		rule.compiledUA = nil
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

// Classify evaluates rules without a caller-specific constraint.
// ok=false means the request must use the host's normal routing.
func (c *Config) Classify(model, userAgent string, body []byte) (category string, ok bool) {
	return c.ClassifyForCaller(model, userAgent, "", body)
}

// ClassifyForCaller uses the first effective user message of a newly observed
// session. Context constraints inspect the entire request, never just that text.
func (c *Config) ClassifyForCaller(model, userAgent, caller string, body []byte) (category string, ok bool) {
	return c.ClassifyWithMetadata(model, userAgent, caller, body, nil)
}

func (c *Config) ClassifyWithMetadata(model, userAgent, caller string, body []byte, metadata map[string]any) (category string, ok bool) {
	if !c.IncludesModel(model) || len(body) == 0 || len(body) > 15*1024*1024 {
		return "", false
	}
	text, standalone, hasTools, ok := firstUserText(body)
	if !ok {
		return "", false
	}
	effort := reasoningEffort(body, metadata)
	contextChars := -1
	for _, rule := range c.Rules {
		if rule.compiledModel != nil && !rule.compiledModel.MatchString(model) || len(rule.ReasoningEfforts) > 0 && !contains(rule.ReasoningEfforts, effort) || utf8.RuneCountInString(strings.TrimSpace(text)) < rule.MinUserChars {
			continue
		}
		if rule.MinContextChars > 0 {
			if contextChars < 0 {
				contextChars = ConversationChars(body)
			}
			if contextChars < rule.MinContextChars {
				continue
			}
		}
		if rule.Standalone && (!standalone || hasTools) || rule.DisallowTools && hasTools || rule.MaxInputBytes > 0 && len(body) > rule.MaxInputBytes {
			continue
		}
		if len(rule.CallerScopes) > 0 && !contains(rule.CallerScopes, caller) {
			continue
		}
		if len(rule.Models) > 0 && !contains(rule.Models, model) {
			continue
		}
		if rule.compiledUA != nil && !rule.compiledUA.MatchString(userAgent) {
			continue
		}
		matched := len(rule.compiledTexts) == len(rule.TextAll)
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

func reasoningEffort(body []byte, metadata map[string]any) string {
	var root struct {
		Reasoning struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		Effort string `json:"reasoning_effort"`
	}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	if effort := strings.TrimSpace(root.Reasoning.Effort); effort != "" {
		return effort
	}
	if effort := strings.TrimSpace(root.Effort); effort != "" {
		return effort
	}
	value, _ := metadata["reasoning_effort"].(string)
	return strings.TrimSpace(value)
}

// ConversationChars measures visible conversation context, not JSON size. It
// excludes system/developer instructions, tool definitions, call arguments,
// images/base64, and encrypted reasoning. Tool result text is real context.
func ConversationChars(body []byte) int {
	var root struct {
		Input    json.RawMessage `json:"input"`
		Messages json.RawMessage `json:"messages"`
	}
	if json.Unmarshal(body, &root) != nil {
		return 0
	}
	raw := root.Messages
	if len(root.Input) > 0 {
		raw = root.Input
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return utf8.RuneCountInString(strings.TrimSpace(text))
	}
	var items []struct {
		Role    string          `json:"role"`
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
		Output  json.RawMessage `json:"output"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return 0
	}
	count := 0
	for _, item := range items {
		if item.Role == "system" || item.Role == "developer" {
			continue
		}
		if item.Type == "function_call_output" {
			if json.Unmarshal(item.Output, &text) == nil {
				count += utf8.RuneCountInString(strings.TrimSpace(text))
			}
		}
		if item.Role != "user" && item.Role != "assistant" && item.Role != "tool" && item.Role != "function" {
			continue
		}
		if json.Unmarshal(item.Content, &text) == nil {
			count += utf8.RuneCountInString(strings.TrimSpace(text))
			continue
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(item.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if part.Type == "text" || part.Type == "input_text" || part.Type == "output_text" {
				count += utf8.RuneCountInString(strings.TrimSpace(part.Text))
			}
		}
	}
	return count
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

func firstUserText(body []byte) (text string, standalone, hasTools, ok bool) {
	var root struct {
		Input            json.RawMessage `json:"input"`
		Messages         json.RawMessage `json:"messages"`
		Tools            json.RawMessage `json:"tools"`
		Functions        json.RawMessage `json:"functions"`
		FunctionCall     json.RawMessage `json:"function_call"`
		ToolChoice       json.RawMessage `json:"tool_choice"`
		PreviousResponse json.RawMessage `json:"previous_response_id"`
		Conversation     json.RawMessage `json:"conversation"`
	}
	if json.Unmarshal(body, &root) != nil {
		return "", false, false, false
	}
	hasTools = nonemptyValue(root.Tools) || nonemptyValue(root.Functions) || nonemptyValue(root.FunctionCall) && string(root.FunctionCall) != `"none"` || nonemptyValue(root.ToolChoice) && string(root.ToolChoice) != `"none"`
	continuation := nonemptyValue(root.PreviousResponse) || nonemptyValue(root.Conversation)
	raw := root.Messages
	if len(root.Input) > 0 {
		if json.Unmarshal(root.Input, &text) == nil {
			return text, !continuation, hasTools, strings.TrimSpace(text) != ""
		}
		raw = root.Input
	}
	var items []struct {
		Role         string          `json:"role"`
		Type         string          `json:"type"`
		Content      json.RawMessage `json:"content"`
		ToolCalls    json.RawMessage `json:"tool_calls"`
		FunctionCall json.RawMessage `json:"function_call"`
	}
	if json.Unmarshal(raw, &items) != nil {
		return "", false, hasTools, false
	}
	users := 0
	complex := continuation
	for _, item := range items {
		if item.Role == "tool" || item.Role == "function" || strings.Contains(item.Type, "call") || nonemptyValue(item.ToolCalls) || nonemptyValue(item.FunctionCall) {
			hasTools = true
		}
		if item.Type != "" && item.Type != "message" {
			complex = true
		}
		switch item.Role {
		case "user":
			users++
			if text == "" {
				var valid bool
				text, valid = userMessageText(item.Content)
				if !valid {
					return "", false, hasTools, false
				}
			}
		case "system", "developer":
		default:
			complex = true
		}
	}
	return text, users == 1 && !complex, hasTools, strings.TrimSpace(text) != ""
}

func nonemptyValue(raw json.RawMessage) bool {
	value := strings.TrimSpace(string(raw))
	return value != "" && value != "null" && value != "[]" && value != `""`
}

func userMessageText(raw json.RawMessage) (string, bool) {
	var text string
	if string(raw) != "null" && json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text), true
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) != nil || len(parts) == 0 {
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
	return strings.TrimSpace(result.String()), true
}
