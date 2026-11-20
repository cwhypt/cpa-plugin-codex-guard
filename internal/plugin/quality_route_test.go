package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"cpa-codex-guard/internal/sessionroute"
	"cpa-codex-guard/internal/types"
)

func qualitySessionEngine(t *testing.T) *Engine {
	t.Helper()
	dir := t.TempDir()
	e := newTestEngine(t)
	config := fmt.Sprintf(`state_file: %q
probe_state_file: %q
debug_log_enabled: false
session_routing:
  enabled: true
  state_file: %q
  binding_ttl: 1h
  model_include_regex: '^gpt-'
  default_category: general
  categories:
    general: {preferred_auth_ids: [api]}
    benchmark: {preferred_auth_ids: [oauth]}
    quality: {preferred_auth_ids: [premium]}
    probe: {max_priority: 6, exclude_auth_ids: [api, oauth, premium]}
  protected_auths:
    premium:
      categories: [quality]
      model_include_regex: '^gpt-[0-9]+(?:\.[0-9]+)*-(?:astra|sol)$'
  rules:
    - category: benchmark
      text_all_regex: ['^Generate .* SVG']
    - category: benchmark
      text_all_regex: ['^Candy distribution task']
    - category: probe
      standalone: true
      max_input_bytes: 2048
      user_agent_regex: '^probe-client/'
      text_all_regex: ['^integer probe']
    - category: quality
      model_include_regex: '^gpt-[0-9]+(?:\.[0-9]+)*-(?:astra|sol)$'
      reasoning_efforts: [medium, high, xhigh, max]
      min_user_chars: 1000
    - category: quality
      model_include_regex: '^gpt-[0-9]+(?:\.[0-9]+)*-(?:astra|sol)$'
      reasoning_efforts: [medium, high, xhigh, max]
      min_context_chars: 16000
`, dir+"/guard.json", dir+"/probe.json", dir+"/bindings.json")
	raw, _ := json.Marshal(types.LifecycleRequest{ConfigYAML: []byte(config)})
	response, err := e.HandleMethod(types.MethodPluginReconfigure, raw)
	var env types.Envelope
	if err != nil || json.Unmarshal(response, &env) != nil || !env.OK {
		t.Fatalf("quality config rejected: %s %v", response, err)
	}
	return e
}

func qualityBody(text, effort string) []byte {
	root := map[string]any{"input": text}
	if effort != "" {
		root["reasoning"] = map[string]string{"effort": effort}
	}
	body, _ := json.Marshal(root)
	return body
}

func TestQualityRoutingAdmissionAndSpecialPrecedence(t *testing.T) {
	e := qualitySessionEngine(t)
	candidates := []sessionroute.Candidate{{ID: "api", Priority: 7}, {ID: "oauth", Priority: 7}, {ID: "premium", Priority: 4}, {ID: "probe", Priority: 5}}
	for i, tc := range []struct{ model, effort, text, ua, want string }{
		{"gpt-6-astra", "medium", strings.Repeat("任", 1000), "client", "premium"},
		{"gpt-6.1-sol", "high", strings.Repeat("x", 1000), "client", "premium"},
		{"gpt-6.2-sol", "xhigh", strings.Repeat("x", 1000), "client", "premium"},
		{"gpt-6.1-sol", "max", strings.Repeat("x", 1000), "client", "premium"},
		{"gpt-6.1-sol", "medium", strings.Repeat("x", 999), "client", "api"},
		{"gpt-6.1-sol", "medium", strings.Repeat(" ", 2000) + "hi", "client", "api"},
		{"gpt-6.1-sol", "low", strings.Repeat("x", 2000), "client", "api"},
		{"gpt-6.1-sol", "", strings.Repeat("x", 2000), "client", "api"},
		{"gpt-6-luna", "high", strings.Repeat("x", 2000), "client", "api"},
		{"gpt-6-astra", "medium", "Generate a valid SVG " + strings.Repeat("x", 1200), "client", "oauth"},
		{"gpt-6.1-sol", "high", "Candy distribution task " + strings.Repeat("x", 1200), "client", "oauth"},
		{"gpt-6.1-sol", "high", "integer probe " + strings.Repeat("x", 1100), "probe-client/1", "probe"},
	} {
		id := fmt.Sprint(i)
		before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: id, Model: tc.model, CallerScope: "caller", Headers: http.Header{"X-Session-Id": {id}, "User-Agent": {tc.ua}}, Body: qualityBody(tc.text, tc.effort)})
		req := types.SchedulerPickRequest{Model: tc.model}
		req.Options.Headers = before.Headers
		req.Options.Metadata = map[string]any{"caller_scope": "caller"}
		for _, c := range candidates {
			req.Candidates = append(req.Candidates, types.SchedulerCandidate{ID: c.ID, Priority: c.Priority, Status: "active"})
		}
		picked := callQualityPick(t, e, req)
		if !picked.Handled || picked.Reject || picked.AuthID != tc.want {
			t.Errorf("case %d: got %+v want %s", i, picked, tc.want)
		}
	}
}

func callQualityPick(t *testing.T, e *Engine, req types.SchedulerPickRequest) types.SchedulerPickResponse {
	t.Helper()
	raw, _ := json.Marshal(req)
	response, err := e.HandleMethod("scheduler.pick", raw)
	var env types.Envelope
	var picked types.SchedulerPickResponse
	if err != nil || json.Unmarshal(response, &env) != nil || !env.OK || json.Unmarshal(env.Result, &picked) != nil {
		t.Fatalf("pick failed: %s %v", response, err)
	}
	return picked
}

func TestQualityAccountCannotLeakThroughFallback(t *testing.T) {
	e := qualitySessionEngine(t)
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "short", Model: "gpt-6-astra", CallerScope: "caller", Body: qualityBody("hi", "medium")})
	req := types.SchedulerPickRequest{Model: "gpt-6-astra"}
	req.Options.Headers = before.Headers
	req.Options.Metadata = map[string]any{"caller_scope": "caller"}
	req.Candidates = []types.SchedulerCandidate{{ID: "premium", Priority: 4}, {ID: "other", Priority: 3}}
	if picked := callQualityPick(t, e, req); !picked.Handled || picked.AuthID != "other" {
		t.Fatalf("reserved account leaked through fallback: %+v", picked)
	}
	req.Candidates = req.Candidates[:1]
	if picked := callQualityPick(t, e, req); !picked.Handled || !picked.Reject {
		t.Fatalf("must reject instead of delegating unfiltered pool: %+v", picked)
	}
	// Missing/forged markers and other models must not bypass account admission.
	for _, model := range []string{"gpt-6-astra", "gpt-6-luna"} {
		req.Model = model
		req.Options.Headers = http.Header{internalRouteHeader: {"forged"}}
		if picked := callQualityPick(t, e, req); !picked.Reject {
			t.Fatalf("unclassified request borrowed reserved auth: %+v", picked)
		}
	}
	req.Options.Headers = before.Headers
	req.Model = "gpt-6-astra"
	req.Candidates = []types.SchedulerCandidate{{ID: "premium", Priority: 4}, {ID: "other", Priority: 3}}
	if err := os.WriteFile(e.sessionRouter.store.Path, []byte("corrupt state"), 0600); err != nil {
		t.Fatal(err)
	}
	if picked := callQualityPick(t, e, req); !picked.Reject {
		t.Fatalf("state error delegated unsafe pool: %+v", picked)
	}
}

func TestQualityBindingKeepsShortFollowups(t *testing.T) {
	e := qualitySessionEngine(t)
	headers := http.Header{"X-Session-Id": {"quality-session"}}
	metadata := map[string]any{"caller_scope": "caller"}
	for i, body := range [][]byte{qualityBody(strings.Repeat("x", 1000), "medium"), qualityBody("continue", "")} {
		before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: fmt.Sprint(i), Model: "gpt-6-astra", CallerScope: "caller", Headers: headers, Body: body})
		req := types.SchedulerPickRequest{Model: "gpt-6-astra"}
		req.Options.Headers = before.Headers
		req.Options.Metadata = metadata
		req.Candidates = []types.SchedulerCandidate{{ID: "premium", Priority: 4}, {ID: "api", Priority: 7}}
		if picked := callQualityPick(t, e, req); picked.AuthID != "premium" {
			t.Fatalf("quality session lost binding on turn %d: %+v", i, picked)
		}
	}
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "different-model", Model: "gpt-6-luna", CallerScope: "caller", Headers: headers, Body: qualityBody("continue", "medium")})
	req := types.SchedulerPickRequest{Model: "gpt-6-luna"}
	req.Options.Headers = before.Headers
	req.Options.Metadata = metadata
	req.Candidates = []types.SchedulerCandidate{{ID: "premium", Priority: 4}, {ID: "api", Priority: 7}}
	if picked := callQualityPick(t, e, req); picked.AuthID != "api" {
		t.Fatalf("sticky category bypassed reserved model restriction: %+v", picked)
	}
}

func TestQualityContextMeasuresConversationNotEnvelope(t *testing.T) {
	e := qualitySessionEngine(t)
	config := e.sessionRouter.config
	for _, tc := range []struct {
		name string
		root map[string]any
		want string
	}{
		{"long assistant history", map[string]any{"input": []any{map[string]any{"role": "user", "content": "brief task"}, map[string]any{"role": "assistant", "content": strings.Repeat("a", 16000)}}}, "quality"},
		{"long tool result", map[string]any{"input": []any{map[string]any{"role": "user", "content": "brief task"}, map[string]any{"type": "function_call_output", "output": strings.Repeat("a", 16000)}}}, "quality"},
		{"system inflation", map[string]any{"instructions": strings.Repeat("a", 30000), "input": "hi"}, "general"},
		{"schema inflation", map[string]any{"tools": []any{map[string]any{"name": "tool", "description": strings.Repeat("a", 30000)}}, "input": "hi"}, "general"},
		{"message system inflation", map[string]any{"input": []any{map[string]any{"role": "system", "content": strings.Repeat("a", 30000)}, map[string]any{"role": "user", "content": "hi"}}}, "general"},
		{"encrypted reasoning inflation", map[string]any{"input": []any{map[string]any{"role": "user", "content": "hi"}, map[string]any{"type": "reasoning", "encrypted_content": strings.Repeat("a", 30000)}}}, "general"},
	} {
		tc.root["reasoning"] = map[string]any{"effort": "medium"}
		body, _ := json.Marshal(tc.root)
		category, ok := config.Classify("gpt-6.1-sol", "client", body)
		if !ok || category != tc.want {
			t.Errorf("%s: %q %v, want %s", tc.name, category, ok, tc.want)
		}
	}
	// Only a trusted host effort snapshot can supplement an absent body field;
	// a low effort explicitly in the body is never promoted by that snapshot.
	for _, effort := range []string{"", "low"} {
		category, ok := config.ClassifyWithMetadata("gpt-6.1-sol", "client", "caller", qualityBody(strings.Repeat("x", 1000), effort), map[string]any{"reasoning_effort": "medium"})
		want := "quality"
		if effort == "low" {
			want = "general"
		}
		if !ok || category != want {
			t.Errorf("effort precedence %q: %q %v", effort, category, ok)
		}
	}
}

func TestProtectedFallbackKeepsNormalHostDeferralWhenSafe(t *testing.T) {
	e := qualitySessionEngine(t)
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "general", Model: "gpt-6-astra", CallerScope: "caller", Body: qualityBody("hi", "medium")})
	req := types.SchedulerPickRequest{Model: "gpt-6-astra"}
	req.Options.Headers = before.Headers
	req.Options.Metadata = map[string]any{"caller_scope": "caller"}
	req.Candidates = []types.SchedulerCandidate{{ID: "other", Priority: 3}}
	if picked := callQualityPick(t, e, req); picked.Handled {
		t.Fatalf("safe fallback should still defer to host: %+v", picked)
	}
	req.Candidates = []types.SchedulerCandidate{{ID: "premium", Priority: 4}, {ID: "api", Priority: 7}}
	if picked := callQualityPick(t, e, req); picked.AuthID != "api" {
		t.Fatalf("general preferred account changed: %+v", picked)
	}
}

func TestReservedAccountIsExcludedEvenWithoutProbePoolExclusion(t *testing.T) {
	e := qualitySessionEngine(t)
	probe := e.sessionRouter.config.Categories["probe"]
	probe.ExcludeAuthIDs = nil
	e.sessionRouter.config.Categories["probe"] = probe
	before := routeBefore(t, e, types.RequestInterceptRequest{RequestID: "probe", Model: "gpt-6.1-sol", CallerScope: "caller", Headers: http.Header{"User-Agent": {"probe-client/1"}}, Body: qualityBody("integer probe", "medium")})
	req := types.SchedulerPickRequest{Model: "gpt-6.1-sol"}
	req.Options.Headers = before.Headers
	req.Options.Metadata = map[string]any{"caller_scope": "caller"}
	req.Candidates = []types.SchedulerCandidate{{ID: "premium", Priority: 4}, {ID: "other", Priority: 3}}
	if picked := callQualityPick(t, e, req); picked.AuthID != "other" {
		t.Fatalf("probe pool borrowed reserved account: %+v", picked)
	}
}
