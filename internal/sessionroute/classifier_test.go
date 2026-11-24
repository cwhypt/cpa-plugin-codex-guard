package sessionroute

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfiguredClassificationUsesFirstUserText(t *testing.T) {
	const config = `
enabled: true
default_category: general
binding_ttl: 1h
state_file: /tmp/session-route-test.json
model_include_regex: '^gpt-'
categories:
  general: {preferred_auth_ids: [account-a]}
  benchmark: {preferred_auth_ids: [account-b]}
  probe: {max_priority: 6, exclude_auth_ids: [account-a, account-b]}
rules:
  - category: benchmark
    models: [gpt-6-astra]
    text_all_regex: ['^Generate .* SVG']
  - category: probe
    models: [gpt-6.1-sol]
    user_agent_regex: '^Codex Desktop/'
    standalone: true
    max_input_bytes: 2048
    text_all_regex: ['1 到 355', '整数']
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(config), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Compile(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ model, ua, body, want string }{
		{"gpt-6-astra", "client", `{"input":"Generate a valid SVG"}`, "benchmark"},
		{"gpt-6.1-sol", "Codex Desktop/1", `{"input":[{"role":"user","content":[{"type":"input_text","text":"选择 1 到 355 的整数"}]}]}`, "probe"},
		{"gpt-6.1-sol", "client", `{"input":[{"role":"user","content":"选择 1 到 355 的整数"}]}`, "general"},
		{"gpt-6-astra", "client", `{"input":[{"role":"user","content":"Generate a valid SVG"},{"role":"assistant","content":"old"},{"role":"user","content":"ordinary question"}]}`, "benchmark"},
		{"gpt-6-astra", "client", `{"messages":[{"role":"tool","content":"Generate a valid SVG"},{"role":"user","content":"ordinary question"}]}`, "general"},
	} {
		got, ok := cfg.Classify(tc.model, tc.ua, []byte(tc.body))
		if !ok || got != tc.want {
			t.Errorf("model %s: got %q, %v; want %q", tc.model, got, ok, tc.want)
		}
	}
	for _, body := range []string{`{"input":null}`, `{"input":[{"role":"user","content":[{"type":"image","text":"Generate a valid SVG"}]}]}`, `{"input":[{"role":"user","content":[{"type":"input_text","text":"Generate a valid SVG"},{"type":"input_image"}]}]}`, `{`} {
		if _, ok := cfg.Classify("gpt-6-astra", "client", []byte(body)); ok {
			t.Errorf("unsupported body must defer: %s", body)
		}
	}
}

func TestThinkingRequiresNonemptyEffort(t *testing.T) {
	cfg := Config{Enabled: true, DefaultCategory: "general", BindingTTL: "1h", StateFile: "state.json", ModelInclude: `^gpt-`, Categories: map[string]Route{"general": {}, "thinking": {PreferredAuthIDs: []string{"api"}}}, Rules: []Rule{{Category: "thinking", ModelInclude: `^gpt-`, MinUserChars: 1000, RequireReasoningEffort: true}}}
	if err := cfg.Compile(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ effort, want string }{{"", "general"}, {"low", "thinking"}, {"future", "thinking"}} {
		body := []byte(`{"input":"` + strings.Repeat("a", 1000) + `","reasoning":{"effort":"` + tc.effort + `"}}`)
		if got, ok := cfg.Classify("gpt-6-terra", "client", body); !ok || got != tc.want {
			t.Fatalf("effort %q: got %q, %v", tc.effort, got, ok)
		}
	}
}

func TestInvalidRulesRejected(t *testing.T) {
	base := Config{Enabled: true, DefaultCategory: "general", BindingTTL: "1h", StateFile: "state.json", ModelInclude: `^gpt-`, Categories: map[string]Route{"general": {PreferredAuthIDs: []string{"a"}}}}
	missing := base
	missing.DefaultCategory = "missing"
	unknownRule := base
	unknownRule.Rules = []Rule{{Category: "missing", TextAll: []string{"x"}}}
	invalidRegex := base
	invalidRegex.Rules = []Rule{{Category: "general", TextAll: []string{"["}}}
	invalidTTL := base
	invalidTTL.BindingTTL = "5d"
	for _, cfg := range []Config{missing, unknownRule, invalidRegex, invalidTTL} {
		if err := cfg.Compile(); err == nil {
			t.Fatalf("expected invalid configuration to be rejected: %+v", cfg)
		}
	}
}

func TestEmptyDefaultRoute(t *testing.T) {
	cfg := Config{Enabled: true, DefaultCategory: "general", BindingTTL: "1h", StateFile: "state.json", ModelInclude: `^gpt-`, Categories: map[string]Route{"general": {}}}
	if err := cfg.Compile(); err != nil {
		t.Fatal(err)
	}
	cfg.Categories["other"] = Route{}
	if err := cfg.Compile(); err == nil {
		t.Fatal("empty non-default route must be rejected")
	}
}
